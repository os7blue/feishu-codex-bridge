package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/feishuui"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type fakeRPC struct {
	mu              sync.Mutex
	calls           []string
	responses       []json.RawMessage
	responseResults []any
	events          chan codex.Message
	done            chan struct{}
	call            func(string, any) (json.RawMessage, error)
	modelCall       func(any) (json.RawMessage, error)
}

func (f *fakeRPC) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	if method == "model/list" {
		if f.modelCall != nil {
			return f.modelCall(params)
		}
		return json.RawMessage(`{"data":[{"id":"test-model","model":"test-model","isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"},{"reasoningEffort":"high"}]}],"nextCursor":null}`), nil
	}
	if f.call != nil {
		return f.call(method, params)
	}
	switch method {
	case "thread/start", "thread/resume":
		return json.RawMessage(`{"thread":{"id":"thread-1"}}`), nil
	case "turn/start":
		return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}
func (f *fakeRPC) Respond(id json.RawMessage, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, id)
	f.responseResults = append(f.responseResults, result)
	return nil
}
func (f *fakeRPC) Reject(id json.RawMessage) error { return f.Respond(id, "rejected") }
func (f *fakeRPC) Events() <-chan codex.Message    { return f.events }
func (f *fakeRPC) Done() <-chan struct{}           { return f.done }

type fakeSender struct{}

func (fakeSender) Send(context.Context, string, string, string) error { return nil }

func setup(t *testing.T) (*Bridge, *fakeRPC) {
	t.Helper()
	dir := t.TempDir()
	project := t.TempDir()
	s, err := state.Open(dir, "app", project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fakeRPC{events: make(chan codex.Message, 32), done: make(chan struct{})}
	cfg := config.Config{AppID: "app", ProjectDir: project, StateDir: dir, AllowedOpenIDs: []string{"ou_owner"}, Sandbox: "workspace-write", ApprovalPolicy: "on-request", TurnTimeoutSeconds: 3600}
	b := New(cfg, f, fakeSender{}, s, slog.New(slog.NewTextHandler(io.Discard, nil)), feishuui.Renderer{})
	return b, f
}

func input(id, text string) Input {
	return Input{ChatID: "chat-1", UserID: "ou_owner", MessageID: id, Text: text}
}
func notification(method, body string) codex.Message {
	return codex.Message{Method: method, Params: json.RawMessage(body)}
}

func TestUnauthorizedInputNeverReachesQueue(t *testing.T) {
	b, _ := setup(t)
	x := input("m1", "edit files")
	x.UserID = "ou_stranger"
	if err := b.Submit(x); err != nil {
		t.Fatal(err)
	}
	if len(b.in) != 0 {
		t.Fatal("unauthorized message queued")
	}
}

func TestPairingRequiresCodeAndPersistsOnlyOneOwner(t *testing.T) {
	b, _ := setup(t)
	b.cfg.AllowedOpenIDs = nil
	code, err := b.EnablePairing()
	if err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), input("m1", "/pair wrong"))
	if b.authorized("ou_owner") {
		t.Fatal("incorrect code accepted")
	}
	b.handle(context.Background(), input("m2", "/pair "+code))
	if !b.authorized("ou_owner") {
		t.Fatal("correct code rejected")
	}
	x := input("m3", "/pair "+code)
	x.UserID = "ou_stranger"
	b.handle(context.Background(), x)
	if b.authorized("ou_stranger") {
		t.Fatal("pairing reused for second owner")
	}
}

func TestDuplicatesDoNotRepeatTaskAndLoadedSessionIsReused(t *testing.T) {
	b, f := setup(t)
	ctx := context.Background()
	b.handle(ctx, input("m1", "do work"))
	b.jobs.Wait()
	b.handle(ctx, input("m1", "do work"))
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	b.handle(ctx, input("m2", "continue"))
	b.jobs.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.calls, ",") != "model/list,thread/start,turn/start,model/list,turn/start" {
		t.Fatalf("unexpected task calls: %v", f.calls)
	}
}

func TestActiveInputSteersAndOtherChatCannotStop(t *testing.T) {
	b, f := setup(t)
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-1", OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1", started: time.Now()}
	b.handle(context.Background(), input("m1", "also check logs"))
	x := input("m2", "/stop")
	x.ChatID = "chat-2"
	b.handle(context.Background(), x)
	b.handle(context.Background(), input("m3", "/stop"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.calls, ",") != "turn/steer,turn/interrupt" {
		t.Fatalf("incorrect steering or stop scope: %v", f.calls)
	}
	if b.active == nil {
		t.Fatal("stop acknowledgement was treated as completion")
	}
}

func TestApprovalIsBoundToOwnerAndConsumedOnce(t *testing.T) {
	b, f := setup(t)
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	b.request(codex.Message{ID: json.RawMessage(`77`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","command":"cargo test"}`)})
	var token string
	for key := range b.pending {
		token = key
	}
	if token == "" {
		t.Fatal("approval request lost")
	}
	x := input("m1", "")
	x.UserID = "ou_other"
	b.answer(x, "/approve", token)
	if len(f.responses) != 0 {
		t.Fatal("approval allowed another owner")
	}
	b.answer(input("m2", ""), "/approve", token)
	b.answer(input("m3", ""), "/approve", token)
	if len(f.responses) != 1 {
		t.Fatal("approval was replayed")
	}
	result, _ := json.Marshal(f.responseResults[0])
	if string(result) != `{"decision":"accept"}` {
		t.Fatalf("wrong approval response: %s", result)
	}
}

func TestContextThresholdRequestsHandoffOnlyOnce(t *testing.T) {
	b, _ := setup(t)
	catalogSession(t, b, "thread-1")
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	event := notification("thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"modelContextWindow":100000,"last":{"inputTokens":76000},"total":{"inputTokens":9000000}}}`)
	b.event(event)
	b.event(event)
	if len(b.in) != 1 {
		t.Fatal("handoff missing or duplicated")
	}
	if request := <-b.in; request.Text != "/handoff" {
		t.Fatal("wrong handoff instruction")
	}
	// A later turn in the same conversation must not inject another handoff.
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-2"}
	b.event(notification("thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":"turn-2","tokenUsage":{"modelContextWindow":100000,"last":{"inputTokens":80000}}}`))
	if len(b.in) != 0 {
		t.Fatal("handoff repeated on a later turn")
	}
}

func TestNewSessionFirstPromptDoesNotResumeUnpersistedThread(t *testing.T) {
	b, f := setup(t)
	f.call = func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "thread/start":
			return json.RawMessage(`{"thread":{"id":"new-thread"}}`), nil
		case "thread/resume":
			return nil, &codex.RPCError{Code: -32600, Message: "no rollout found"}
		case "turn/start":
			return json.RawMessage(`{"turn":{"id":"first-turn"}}`), nil
		default:
			return json.RawMessage(`{}`), nil
		}
	}
	b.handle(context.Background(), input("new", "/new"))
	b.handle(context.Background(), input("first", "continue from docs/HANDOFF.md"))
	b.jobs.Wait()
	if b.active == nil || b.active.turn != "first-turn" {
		t.Fatal("first prompt did not start")
	}
	if got := strings.Join(f.calls, ","); got != "model/list,thread/start,model/list,turn/start" {
		t.Fatalf("wrong lifecycle: %s", got)
	}
}

func TestSavedAndClosedThreadsAreResumed(t *testing.T) {
	for _, closed := range []bool{false, true} {
		b, f := setup(t)
		catalogSession(t, b, "thread-1")
		if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-1", OwnerID: "ou_owner"}); err != nil {
			t.Fatal(err)
		}
		if closed {
			b.loaded["thread-1"] = true
			b.event(notification("thread/closed", `{"threadId":"thread-1"}`))
		}
		b.handle(context.Background(), input("continue", "continue"))
		b.jobs.Wait()
		if got := strings.Join(f.calls, ","); got != "model/list,thread/resume,turn/start" {
			t.Fatalf("saved thread lifecycle: %s", got)
		}
	}
}

func TestResumeFailurePreservesBindingAndDoesNotStartTurn(t *testing.T) {
	b, f := setup(t)
	catalogSession(t, b, "thread-1")
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-1", OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	f.call = func(string, any) (json.RawMessage, error) {
		return nil, &codex.RPCError{Code: -32600, Message: "secret provider diagnostics"}
	}
	b.handle(context.Background(), input("retry", "continue"))
	b.jobs.Wait()
	if b.active != nil || strings.Join(f.calls, ",") != "model/list,thread/resume" {
		t.Fatal("failed resume started a task")
	}
	if binding, _ := b.store.Binding("chat-1"); binding.ThreadID != "thread-1" {
		t.Fatal("binding changed")
	}
	var messages string
	for len(b.out) > 0 {
		messages += (<-b.out).Text
	}
	if !strings.Contains(messages, "RPC -32600") || strings.Contains(messages, "secret") {
		t.Fatal("unsafe or missing failure diagnostic")
	}
}

func TestSessionStartErrorsExposeOnlyAllowlistedDetails(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "请求超时"},
		{errors.New("secret"), "连接失败"},
		{&codex.RPCError{Code: -32600, Message: "no rollout found for thread id secret"}, "尚无可恢复"},
		{&codex.RPCError{Code: -32600, Message: "secret", Data: json.RawMessage(`{"type":"modelProviderMismatch","secret":"secret"}`)}, "模型提供商"},
		{&codex.RPCError{Code: -32603, Message: "secret", Data: json.RawMessage(`{"type":"secret"}`)}, "RPC -32603"},
	}
	for _, tc := range cases {
		got := sessionStartError("thread/resume", tc.err)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "secret") {
			t.Fatalf("unsafe error: %s", got)
		}
	}
}

func TestFailureAndInterruptionAreNotReportedAsSuccess(t *testing.T) {
	for _, status := range []string{"failed", "interrupted"} {
		b, _ := setup(t)
		b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
		body, _ := json.Marshal(map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": status}})
		b.event(codex.Message{Method: "turn/completed", Params: body})
		result := <-b.out
		if strings.Contains(result.Text, "执行结束") || b.active != nil {
			t.Fatal("non-success turn reported as successful or left active")
		}
	}
}

func TestExpiredApprovalDeclinesRatherThanAllows(t *testing.T) {
	b, f := setup(t)
	b.pending["token"] = pending{id: json.RawMessage(`5`), method: "item/fileChange/requestApproval", chat: "chat-1", created: time.Now().Add(-11 * time.Minute)}
	b.expire(context.Background())
	result, _ := json.Marshal(f.responseResults[0])
	if string(result) != `{"decision":"decline"}` || len(b.pending) != 0 {
		t.Fatal("expired approval was not declined")
	}
}

type recordingSender struct {
	calls     int
	key, text string
	fail      bool
}

func (s *recordingSender) Send(_ context.Context, _, text, key string) error {
	s.calls++
	s.key = key
	s.text = text
	if s.fail {
		return errors.New("offline")
	}
	return nil
}

func TestOutboxRetainsFailureAndRetriesWithSameID(t *testing.T) {
	b, _ := setup(t)
	sender := &recordingSender{fail: true}
	b.sender = sender
	b.cfg.AppSecret = "private-app-secret"
	b.emit("chat-1", "result with private-app-secret")
	item := <-b.out
	if b.deliverOne(context.Background(), item) {
		t.Fatal("failed delivery reported success")
	}
	if _, err := os.Stat(b.outboxPath(item)); err != nil {
		t.Fatal("failed delivery lost", err)
	}
	if strings.Contains(sender.text, "private-app-secret") {
		t.Fatal("secret was sent")
	}
	sender.fail = false
	b.retryOutbox(context.Background())
	if sender.calls != 2 || sender.key != item.Key {
		t.Fatal("retry changed delivery ID")
	}
	if _, err := os.Stat(b.outboxPath(item)); !os.IsNotExist(err) {
		t.Fatal("delivered message not acknowledged")
	}
	b.deliverOne(context.Background(), item)
	if sender.calls != 2 {
		t.Fatal("queued replay sent acknowledged message again")
	}
}

func TestOldUncertainDeliveryIsRetainedWithoutResending(t *testing.T) {
	b, _ := setup(t)
	sender := &recordingSender{}
	b.sender = sender
	item := outbound{Chat: "chat-1", Text: "uncertain reply", Key: "old", Created: time.Now().Add(-51 * time.Minute).UnixNano()}
	if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
		t.Fatal(err)
	}
	b.retryOutbox(context.Background())
	if sender.calls != 0 {
		t.Fatal("delivery retried outside de-duplication window")
	}
	if _, err := os.Stat(filepath.Join(b.cfg.StateDir, "undelivered-old.json")); err != nil {
		t.Fatal("uncertain reply was not retained", err)
	}
}

func TestStopWithoutCompletionFailsClosed(t *testing.T) {
	b, _ := setup(t)
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1", stopping: true, stopAt: time.Now().Add(-31 * time.Second)}
	b.expire(context.Background())
	select {
	case <-b.fatal:
	default:
		t.Fatal("unconfirmed stop left bridge waiting forever")
	}
}
