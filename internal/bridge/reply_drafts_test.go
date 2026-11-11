package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
)

func requestMenu(t *testing.T, b *Bridge, method, fields string) (string, string, outbound) {
	t.Helper()
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	b.request(codex.Message{ID: json.RawMessage(`77`), Method: method, Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1",` + fields + `}`)})
	var request, menu string
	for key := range b.pending {
		request = key
	}
	for key, grant := range b.drafts {
		if grant.menu && grant.request == request {
			menu = key
		}
	}
	select {
	case item := <-b.out:
		return request, menu, item
	default:
		t.Fatal("request prompt not emitted")
		return "", "", outbound{}
	}
}

func TestHumanCommandRequiresOwnerManualOneTimeDecision(t *testing.T) {
	for _, decision := range []struct{ choice, wire string }{{"approve", "accept"}, {"deny", "decline"}} {
		t.Run(decision.choice, func(t *testing.T) {
			b, rpc := setup(t)
			request, menu, prompt := requestMenu(t, b, "item/commandExecution/requestApproval", `"kind":"humanCommand","itemId":"checkpoint","approvalId":"callback-1","command":"echo checked","reason":"需要本人确认"`)
			if !strings.Contains(prompt.Text, "必须由你逐次确认") || request == "" || menu == "" || len(rpc.responses) != 0 {
				t.Fatal("human checkpoint was not presented for a fresh decision")
			}
			token, _ := openDraft(t, b, menu, decision.choice)
			if len(rpc.responses) != 0 || len(b.in) != 0 {
				t.Fatal("opening a human approval draft submitted a decision")
			}
			action := CardAction{ChatID: "chat-1", UserID: "ou_other", Token: token, Action: "draft_send", Text: "/" + decision.choice + " " + request}
			if !b.HandleCard(action).Error || len(b.in) != 0 {
				t.Fatal("another account decided the human checkpoint")
			}
			action.UserID = "ou_owner"
			if b.HandleCard(action).Error || len(b.in) != 1 || len(rpc.responses) != 0 {
				t.Fatal("manual decision did not enter the normal inbox")
			}
			b.handle(context.Background(), <-b.in)
			if len(rpc.responses) != 1 || len(b.pending) != 0 {
				t.Fatal("human checkpoint was not consumed exactly once")
			}
			body, _ := json.Marshal(rpc.responseResults[0])
			if string(body) != `{"decision":"`+decision.wire+`"}` || string(rpc.responses[0]) != "77" {
				t.Fatal("human checkpoint response changed its RPC ID or granted a persistent decision")
			}
			b.answer(input("replay", ""), "/"+decision.choice, request)
			if !b.HandleCard(action).Error || len(rpc.responses) != 1 {
				t.Fatal("human checkpoint decision was replayed")
			}
		})
	}
}

func TestApprovalDetailsSurviveHighlightSelectionAndReselection(t *testing.T) {
	for _, method := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"} {
		t.Run(method, func(t *testing.T) {
			b, rpc := setup(t)
			b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
			b.event(codex.Message{Method: "item/started", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"preview-item","type":"fileChange","changes":[{"path":"docs/example.md","diff":"+fixture approval preview"}]}}`)})
			b.request(codex.Message{ID: json.RawMessage(`78`), Method: method, Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"preview-item","command":"echo fixture-review","cwd":"/fixture/project","reason":"确认本次具体操作"}`)})
			item := <-b.out
			var menu string
			for key, grant := range b.drafts {
				if grant.menu {
					menu = key
				}
			}
			assertDetails := func(card json.RawMessage) {
				t.Helper()
				var rendered any
				if json.Unmarshal(card, &rendered) != nil {
					t.Fatal("invalid approval card")
				}
				var texts []string
				var walk func(any)
				walk = func(value any) {
					switch v := value.(type) {
					case map[string]any:
						if text, ok := v["content"].(string); ok {
							texts = append(texts, text)
						}
						for _, child := range v {
							walk(child)
						}
					case []any:
						for _, child := range v {
							walk(child)
						}
					}
				}
				walk(rendered)
				visible := strings.Join(texts, "\n")
				for _, detail := range []string{"确认本次具体操作", "命令：echo fixture-review", "目录：/fixture/project", "操作预览：", "文件：docs/example.md", "+fixture approval preview"} {
					if !strings.Contains(visible, detail) {
						t.Fatalf("visible approval detail missing: %s", detail)
					}
				}
			}
			assertDetails(item.Card)
			first, selected := openDraft(t, b, menu, "approve")
			assertDetails(selected.Card)
			_, reselected := openDraft(t, b, first, "deny")
			assertDetails(reselected.Card)
			if len(b.in) != 0 || len(rpc.responses) != 0 {
				t.Fatal("reviewing details submitted an approval")
			}
		})
	}
}

func TestRequestCommandTextOpensDraftAndOnlyManualSendAnswers(t *testing.T) {
	b, rpc := setup(t)
	request, menu, item := requestMenu(t, b, "item/commandExecution/requestApproval", `"command":"cargo test","reason":"运行检查"`)
	if request == "" || menu == "" || !strings.Contains(string(item.Card), `"tag":"interactive_container"`) || strings.Contains(string(item.Card), `"tag":"button"`) || strings.Count(string(item.Card), "/approve "+request) != 1 || !strings.Contains(string(item.Card), `"text_color":"blue"`) {
		t.Fatal("reply instructions were not replaced by clickable command text")
	}
	token, card := openDraft(t, b, menu, "approve")
	if len(rpc.responses) != 0 || len(rpc.calls) != 0 || len(b.in) != 0 || !strings.Contains(string(card.Card), `"default_value":"/approve `+request+`"`) {
		t.Fatal("click executed an approval or lost its request ID")
	}
	for _, invalid := range []string{"/new", "/approve other", "/answer " + request + " yes", "/approve " + request + " extra"} {
		if !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: invalid}).Error {
			t.Fatal("request-bound draft accepted another command/request")
		}
	}
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/deny " + request}
	var jobs sync.WaitGroup
	for range 8 {
		jobs.Add(1)
		go func() { defer jobs.Done(); b.HandleCard(action) }()
	}
	jobs.Wait()
	if len(b.in) != 1 || len(rpc.responses) != 0 {
		t.Fatal("send repeated or callback answered RPC")
	}
	b.handle(context.Background(), <-b.in)
	if len(rpc.responses) != 1 || len(b.pending) != 0 {
		t.Fatal("edited denial did not consume request once")
	}
	body, _ := json.Marshal(rpc.responseResults[0])
	if string(body) != `{"decision":"decline"}` {
		t.Fatal("edited approval decision lost")
	}
	if !b.HandleCard(action).Error || len(b.drafts) != 0 {
		t.Fatal("consumed request grants stayed live")
	}
}

func TestRequestHighlightsOpenDraftWithoutChoiceButtons(t *testing.T) {
	for _, sample := range []struct{ method, fields, choice, command string }{
		{"item/commandExecution/requestApproval", `"command":"cargo test"`, "approve", "/approve"},
		{"item/tool/requestUserInput", `"questions":[{"id":"q","question":"选择环境","options":[{"label":"测试环境"}]}]`, "option_0", "/answer"},
	} {
		t.Run(sample.command, func(t *testing.T) {
			b, rpc := setup(t)
			request, _, item := requestMenu(t, b, sample.method, sample.fields)
			var root any
			if json.Unmarshal(item.Card, &root) != nil {
				t.Fatal("invalid request card")
			}
			choices := map[string]map[string]any{}
			var walk func(any)
			walk = func(value any) {
				switch node := value.(type) {
				case []any:
					for _, child := range node {
						walk(child)
					}
				case map[string]any:
					tag, _ := node["tag"].(string)
					if tag == "button" {
						if v, ok := node["value"].(map[string]any); ok && v["command"] == sample.choice {
							choices[tag] = v
						}
					}
					if tag == "interactive_container" {
						if actions, ok := node["behaviors"].([]any); ok {
							for _, action := range actions {
								if a, ok := action.(map[string]any); ok {
									if v, ok := a["value"].(map[string]any); ok && v["command"] == sample.choice {
										choices[tag] = v
									}
								}
							}
						}
					}
					for _, child := range node {
						walk(child)
					}
				}
			}
			walk(root)
			if len(choices) != 1 || choices["interactive_container"] == nil || strings.Contains(string(item.Card), `"tag":"button"`) {
				t.Fatal("request should contain highlighted choices without extra buttons")
			}
			for _, choice := range choices {
				result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: choice["token"].(string), Action: choice["action"].(string), Command: choice["command"].(string)})
				if result.Error || !strings.Contains(string(result.Card), sample.command+" "+request) || len(rpc.responses) != 0 || len(b.in) != 0 {
					t.Fatal("click lost the request or submitted a reply")
				}
			}
		})
	}
}

func TestRequestCanReselectHighlightWithoutReturning(t *testing.T) {
	for _, sample := range []struct{ name, method, fields, first, second, expected string }{
		{"approval", "item/commandExecution/requestApproval", `"command":"cargo test"`, "approve", "deny", "/deny "},
		{"answer", "item/tool/requestUserInput", `"questions":[{"id":"q","question":"选择环境","options":[{"label":"甲"},{"label":"乙"}]}]`, "option_0", "option_1", "/answer "},
	} {
		t.Run(sample.name, func(t *testing.T) {
			b, rpc := setup(t)
			request, menu, _ := requestMenu(t, b, sample.method, sample.fields)
			first, _ := openDraft(t, b, menu, sample.first)
			deadline := b.drafts[first].expires
			next, card := openDraft(t, b, first, sample.second)
			if _, exists := b.drafts[first]; exists {
				t.Fatal("old selected form stayed live")
			}
			if !b.drafts[next].expires.Equal(deadline) || !strings.Contains(string(card.Card), sample.expected+request) || !strings.Contains(string(card.Card), `"tag":"interactive_container"`) || strings.Contains(string(card.Card), "返回选项") || len(b.in) != 0 || len(rpc.responses) != 0 {
				t.Fatal("reselection lost choices, request, deadline or submitted a reply")
			}
			if !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: first, Action: "draft_send", Text: "/approve " + request}).Error {
				t.Fatal("old form submitted after reselection")
			}
		})
	}
}

func TestQuestionCommandTextPrefillsOptionAndAcceptsEditedAnswer(t *testing.T) {
	b, rpc := setup(t)
	request, menu, _ := requestMenu(t, b, "item/tool/requestUserInput", `"questions":[{"id":"choice","question":"选择环境","options":[{"label":"测试环境","description":"隔离验证"}]}]`)
	token, card := openDraft(t, b, menu, "option_0")
	if !strings.Contains(string(card.Card), `"default_value":"/answer `+request+` 测试环境"`) || len(rpc.responses) != 0 {
		t.Fatal("option was not drafted without execution")
	}
	result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/answer " + request + " 自定义环境"})
	if result.Error {
		t.Fatal(result.Message)
	}
	b.handle(context.Background(), <-b.in)
	body, _ := json.Marshal(rpc.responseResults[0])
	if string(body) != `{"answers":{"choice":{"answers":["自定义环境"]}}}` {
		t.Fatal("edited option did not reach ordinary answer handler")
	}
}

func TestRequestDraftBackKeepsChoicesRequestAndDeadline(t *testing.T) {
	for _, sample := range []struct{ name, method, fields, first, second, expected string }{
		{"approval", "item/commandExecution/requestApproval", `"command":"cargo test"`, "approve", "deny", "/deny "},
		{"answer", "item/tool/requestUserInput", `"questions":[{"id":"q","question":"选择环境","options":[{"label":"甲"},{"label":"乙"}]}]`, "option_0", "option_1", "/answer "},
	} {
		t.Run(sample.name, func(t *testing.T) {
			b, rpc := setup(t)
			request, menu, _ := requestMenu(t, b, sample.method, sample.fields)
			token, _ := openDraft(t, b, menu, sample.first)
			deadline := b.drafts[token].expires
			result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_back"})
			if result.Error || !strings.Contains(string(result.Card), sample.expected+request) || len(rpc.responses) != 0 || len(b.in) != 0 {
				t.Fatal("returning lost the request or submitted a reply")
			}
			next, card := openDraft(t, b, token, sample.second)
			if !b.drafts[next].expires.Equal(deadline) || !strings.Contains(string(card.Card), sample.expected+request) {
				t.Fatal("reselection changed the request or deadline")
			}
			grant := b.drafts[next]
			grant.expires = time.Now().Add(-time.Second)
			b.drafts[next] = grant
			if !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: next, Action: "draft_back"}).Error || len(rpc.responses) != 0 {
				t.Fatal("returning revived an expired request")
			}
		})
	}
}

func TestMultiQuestionDraftRequiresAllAnswersAndKeepsOriginalDeadline(t *testing.T) {
	b, rpc := setup(t)
	request, menu, _ := requestMenu(t, b, "item/tool/requestUserInput", `"questions":[{"id":"one","question":"第一题"},{"id":"two","question":"第二题"}]`)
	deadline := b.drafts[menu].expires
	token, card := openDraft(t, b, menu, "custom")
	if !b.drafts[token].expires.Equal(deadline) || strings.Contains(string(card.Card), "30 分钟") {
		t.Fatal("opening draft extended request deadline")
	}
	for _, text := range []string{"/answer " + request, "/answer " + request + ` {"one":"a"}`, "/answer " + request + ` {"one":"a","two":""}`} {
		if !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: text}).Error {
			t.Fatal("incomplete multi-question answer consumed draft")
		}
	}
	result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/answer " + request + ` {"one":"a","two":"b"}`})
	if result.Error || len(rpc.responses) != 0 {
		t.Fatal("valid multi-answer was not queued")
	}
	b.handle(context.Background(), <-b.in)
	if len(rpc.responses) != 1 {
		t.Fatal("multi-answer not delivered")
	}
}

func TestRequestDraftsRejectOwnerChatExpiryResolvedAndRestart(t *testing.T) {
	for _, how := range []string{"owner", "chat", "expired", "resolved", "manual", "turn_end", "restart"} {
		t.Run(how, func(t *testing.T) {
			b, rpc := setup(t)
			request, menu, _ := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"修改文件"`)
			token, _ := openDraft(t, b, menu, "approve")
			action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/approve " + request}
			switch how {
			case "owner":
				action.UserID = "ou_other"
			case "chat":
				action.ChatID = "other-chat"
			case "expired":
				grant := b.drafts[token]
				grant.expires = time.Now().Add(-time.Second)
				b.drafts[token] = grant
			case "resolved":
				b.event(notification("serverRequest/resolved", `{"threadId":"thread-1","requestId":77}`))
			case "manual":
				b.answer(input("manual", ""), "/deny", request)
			case "turn_end":
				b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
			case "restart":
				b.drafts = nil
			}
			before := len(rpc.responses)
			if !b.HandleCard(action).Error || len(b.in) != 0 || len(rpc.responses) != before {
				t.Fatal("invalid request draft submitted")
			}
		})
	}
}

func TestReplyDraftQueueFullKeepsEditedReplyRetryable(t *testing.T) {
	b, rpc := setup(t)
	request, menu, _ := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"检查"`)
	token, _ := openDraft(t, b, menu, "deny")
	for len(b.in) < cap(b.in) {
		b.in <- input("filler", "task")
	}
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/deny " + request}
	if !b.HandleCard(action).Error || b.drafts[token].submitted || len(rpc.responses) != 0 {
		t.Fatal("full queue consumed reply grant")
	}
	<-b.in
	if b.HandleCard(action).Error || !b.drafts[token].submitted {
		t.Fatal("retry did not submit reply")
	}
}

func TestRequestResolutionRacingWithSendCannotAnswerResolvedRequest(t *testing.T) {
	b, rpc := setup(t)
	request, menu, _ := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"检查"`)
	token, _ := openDraft(t, b, menu, "approve")
	var jobs sync.WaitGroup
	jobs.Add(2)
	go func() {
		defer jobs.Done()
		b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/approve " + request})
	}()
	go func() {
		defer jobs.Done()
		b.event(notification("serverRequest/resolved", `{"threadId":"thread-1","requestId":77}`))
	}()
	jobs.Wait()
	if len(b.in) > 0 {
		b.handle(context.Background(), <-b.in)
	}
	if len(rpc.responses) != 0 || len(b.pending) != 0 {
		t.Fatal("resolved request answered by queued draft")
	}
}

func TestReplyPromptsKeepTextFallbackAndSecretRequestsHaveNoDraft(t *testing.T) {
	b, rpc := setup(t)
	_, menu, item := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"`+strings.Repeat("x", 25000)+`"`)
	if len(item.Card) != 0 || menu != "" || !strings.Contains(item.Text, "/approve ") || len(b.pending) != 1 {
		t.Fatal("long prompt lost manual text fallback")
	}
	b2, rpc2 := setup(t)
	_, menu2, secret := requestMenu(t, b2, "item/tool/requestUserInput", `"questions":[{"id":"secret","question":"password","isSecret":true}]`)
	if len(secret.Card) != 0 || menu2 != "" || len(b2.pending) != 0 || len(b2.drafts) != 0 || len(rpc.responses) != 0 || len(rpc2.responses) != 1 || rpc2.responseResults[0] != "rejected" {
		t.Fatal("secret request exposed interactive draft")
	}
}

func TestReplyCardAndPrefilledOptionRedactConfiguredSecret(t *testing.T) {
	b, _ := setup(t)
	b.cfg.AppSecret = "fixture-private-secret"
	_, menu, item := requestMenu(t, b, "item/tool/requestUserInput", `"questions":[{"id":"choice","question":"fixture-private-secret","options":[{"label":"fixture-private-secret","description":"fixture-private-secret"}]}]`)
	_, draft := openDraft(t, b, menu, "option_0")
	if strings.Contains(item.Text+string(item.Card)+string(draft.Card), b.cfg.AppSecret) {
		t.Fatal("configured credential escaped in prompt or option draft")
	}
}
