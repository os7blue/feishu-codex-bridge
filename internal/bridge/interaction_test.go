package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type uncertainCardSender struct{ fail bool }

func (s *uncertainCardSender) Send(context.Context, string, string, string) error { return nil }
func (s *uncertainCardSender) SendCardRouted(context.Context, string, json.RawMessage, string) ([]string, error) {
	if s.fail {
		return nil, errors.New("fixture-only delivery failure")
	}
	return []string{"fixture-card"}, nil
}

func TestDemoDeliveryEvidenceRequiresCardTransportSuccess(t *testing.T) {
	b, _ := setup(t)
	if err := b.store.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Bind("chat-1", state.Binding{OwnerID: "ou_owner", ThreadID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SendClickDemo(); err != nil {
		t.Fatal(err)
	}
	first := <-b.out
	sender := &uncertainCardSender{fail: true}
	b.sender = sender
	if b.deliverOne(context.Background(), first) {
		t.Fatal("failed card counted as successful")
	}
	s := b.Diagnostics().Snapshot()
	if s.Counts["demo"]["queued"] != 3 || s.Counts["demo_delivery"]["failed"] != 1 || s.Counts["demo_delivery"]["delivered"] != 0 {
		t.Fatal("queue count mistaken for delivery")
	}
	sender.fail = false
	if !b.deliverOne(context.Background(), first) {
		t.Fatal("retry failed")
	}
	b.deliverOne(context.Background(), first)
	b.sender = fakeSender{} // Text fallback must not certify an interactive card.
	b.deliverOne(context.Background(), <-b.out)
	s = b.Diagnostics().Snapshot()
	if s.Counts["demo_delivery"]["delivered"] != 1 || s.Counts["text_delivery"]["delivered"] != 1 {
		t.Fatal("card ack or fallback evidence wrong")
	}
}

func TestSubmissionRetainsQuestionEditedAnswerAndFirstReceipt(t *testing.T) {
	b, rpc := setup(t)
	b.cfg.AppSecret = "fixture-private-secret"
	request, menu, _ := requestMenu(t, b, "item/tool/requestUserInput", `"questions":[{"id":"choice","question":"选择环境 fixture-private-secret","options":[{"label":"测试环境"}]}]`)
	token, _ := openDraft(t, b, menu, "option_0")
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/answer " + request + " 修改后的环境 fixture-private-secret"}
	result := b.HandleCard(action)
	var card map[string]any
	if result.Error || json.Unmarshal(result.Card, &card) != nil {
		t.Fatal("submission failed")
	}
	// Inspect decoded content, including arbitrary punctuation as plain text.
	data := string(result.Card)
	for _, want := range []string{"选择环境", "修改后的环境", "/answer " + request, "等待桥接处理", "[REDACTED]"} {
		if !strings.Contains(data+result.Message, want) {
			t.Fatalf("receipt lost %s", want)
		}
	}
	if strings.Contains(data, b.cfg.AppSecret) || strings.Contains(data, `"tag":"form"`) || len(rpc.responses) != 0 {
		t.Fatal("receipt exposed secret, stayed editable or sent RPC")
	}
	action.Text = "/answer " + request + " 偷换内容"
	replay := b.HandleCard(action)
	if string(replay.Card) != data || len(b.in) != 1 {
		t.Fatal("retry overwrote the original submission")
	}
	b.handle(context.Background(), <-b.in)
	if len(rpc.responses) != 1 {
		t.Fatal("submitted answer was not delivered once")
	}
	s := b.Diagnostics().Snapshot()
	if s.Counts["draft_send"]["queued"] != 1 || s.Counts["draft_send"]["duplicate"] != 1 || s.Counts["response"]["answered"] != 1 {
		t.Fatal("queue and RPC evidence conflated")
	}
	public, _ := json.Marshal(s)
	for _, secret := range []string{request, token, "choice", "修改后的环境", "ou_owner", "thread-1", b.cfg.AppSecret} {
		if strings.Contains(string(public), secret) {
			t.Fatal("diagnostics retained payload or identity")
		}
	}
}

func TestIsolatedApprovalAndAnswerDemosNeverRespondToRealRequests(t *testing.T) {
	b, rpc := setup(t)
	if err := b.store.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Bind("chat-1", state.Binding{OwnerID: "ou_owner", ThreadID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	// A real pending request exists while synthetic cards are exercised.
	real, _, _ := requestMenu(t, b, "item/commandExecution/requestApproval", `"command":"sensitive-real-command"`)
	if err := b.SendClickDemo(); err != nil {
		t.Fatal(err)
	}
	for len(b.out) > 0 {
		<-b.out
	}
	type sample struct{ menu, request, choice, text string }
	var samples []sample
	for token, grant := range b.drafts {
		if !grant.demo || grant.request == "" {
			continue
		}
		samples = append(samples, sample{token, grant.request, grant.replies[0].id, grant.replies[0].text})
	}
	if len(samples) != 2 {
		t.Fatal("isolated approval and question cards missing")
	}
	for _, sample := range samples {
		token, _ := openDraft(t, b, sample.menu, sample.choice)
		sibling, _ := openDraft(t, b, sample.menu, sample.choice)
		action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/approve " + real}
		if !b.HandleCard(action).Error || len(b.in) != 0 {
			t.Fatal("demo accepted real request ID")
		}
		action.Text = sample.text
		action.UserID = "ou_other"
		if !b.HandleCard(action).Error {
			t.Fatal("demo accepted another account")
		}
		action.UserID = "ou_owner"
		result := b.HandleCard(action)
		if result.Error || !strings.Contains(string(result.Card), sample.text) {
			t.Fatal("demo submission lost its record")
		}
		action.Token = sibling
		if !b.HandleCard(action).Error {
			t.Fatal("another draft replayed the same demo decision")
		}
		b.handle(context.Background(), <-b.in)
	}
	if len(rpc.calls) != 0 || len(rpc.responses) != 0 || len(b.pending) != 1 || b.pending[real].method == "" {
		t.Fatal("demo reached RPC or consumed real approval")
	}
	s := b.Diagnostics().Snapshot()
	if s.Counts["demo_send"]["queued"] != 2 || s.Counts["demo_result"]["completed"] != 2 || len(s.Counts["response"]) != 0 {
		t.Fatal("demo evidence mixed with live responses")
	}
}

func TestReviewAndCommandOutcomesAreScopedAndPayloadFree(t *testing.T) {
	b, _ := setup(t)
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	for _, body := range []string{
		`{"threadId":"other","turnId":"turn-1","review":{"status":"denied"}}`,
		`{"threadId":"thread-1","turnId":"other","review":{"status":"denied"}}`,
	} {
		b.event(notification("item/autoApprovalReview/completed", body))
	}
	if len(b.out) != 0 {
		t.Fatal("unrelated review forwarded")
	}
	b.event(notification("item/autoApprovalReview/started", `{"threadId":"thread-1","turnId":"turn-1","review":{"status":"inProgress"}}`))
	for _, status := range []string{"approved", "denied", "timedOut", "aborted", "payload-secret"} {
		body := `{"threadId":"thread-1","turnId":"turn-1","action":{"command":"payload-secret"},"review":{"status":"` + status + `","rationale":"payload-secret"}}`
		b.event(notification("item/autoApprovalReview/completed", body))
	}
	for _, status := range []string{"completed", "failed", "declined", "payload-secret"} {
		b.event(notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","status":"`+status+`","aggregatedOutput":"payload-secret"}}`))
	}
	for len(b.out) > 0 {
		if item := <-b.out; strings.Contains(item.Text, "payload-secret") {
			t.Fatal("raw review or output leaked")
		}
	}
	s := b.Diagnostics().Snapshot()
	if s.Counts["auto_review"]["started"] != 1 || s.Counts["auto_review"]["denied"] != 1 || s.Counts["auto_review"]["unknown"] != 1 || s.Counts["command"]["declined"] != 1 {
		t.Fatal("review and execution evidence missing")
	}
	data, _ := json.Marshal(s)
	if strings.Contains(string(data), "payload-secret") {
		t.Fatal("unrecognized status entered evidence")
	}
}

func TestRequestRejectionsHaveOnlyAllowlistedClassifications(t *testing.T) {
	b, rpc := setup(t)
	b.request(codex.Message{ID: json.RawMessage(`1`), Method: "payload-secret", Params: json.RawMessage(`bad`)})
	b.request(codex.Message{ID: json.RawMessage(`2`), Method: "payload-secret", Params: json.RawMessage(`{}`)})
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	b.request(codex.Message{ID: json.RawMessage(`3`), Method: "payload-secret", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1"}`)})
	s := b.Diagnostics().Snapshot()
	if len(rpc.responses) != 3 || s.Counts["request"]["received"] != 3 || s.Counts["request_rejected"]["invalid"] != 1 || s.Counts["request_rejected"]["unavailable"] != 1 || s.Counts["request_rejected"]["unsupported"] != 1 {
		t.Fatal("request rejection phase missing")
	}
}
