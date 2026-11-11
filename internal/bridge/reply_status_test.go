package bridge

import (
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
)

func TestReplyStatusTracksTurnQueueAndApprovalWithoutRPC(t *testing.T) {
	b, rpc := setup(t)
	s := catalogSession(t, b, "thread-1")
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: s.ThreadID}
	b.mu.Lock()
	b.refreshReplyStatusLocked()
	// Rendering must also work with the task mutex held.
	if line := b.sessionLine(s.ThreadID); !strings.Contains(line, "状态：启动中") || !strings.Contains(line, "模型：待确认") {
		t.Fatal(line)
	}
	b.active.turn = "turn-1"
	b.active.modelSettings = modelsettings.Effective{Model: "actual-model", Effort: "high"}
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	if err := b.store.SetModelSettings("", "ou_owner", modelsettings.Settings{Model: "next-model", Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	b.emit("chat-1", "进度", s.ThreadID)
	item := <-b.out
	if !strings.Contains(item.Text, "状态：执行中") || !strings.Contains(item.Text, "当前轮 · 模型：actual-model · 思考强度：high") || strings.Contains(item.Text, "next-model") || strings.Contains(item.Text, "路径：") {
		t.Fatal(item.Text)
	}
	b.request(codex.Message{ID: []byte(`1`), Method: "item/commandExecution/requestApproval", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","command":"echo test"}`)})
	item = <-b.out
	if !strings.Contains(item.Text, "状态：等待审批或回答") || !strings.Contains(string(item.Card), "等待审批或回答") {
		t.Fatalf("approval status missing: %s / %s", item.Text, item.Card)
	}
	b.mu.Lock()
	for token := range b.pending {
		b.forgetPending(token)
	}
	b.active.stopping = true
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	if line := b.sessionLine(s.ThreadID); !strings.Contains(line, "状态：停止中") {
		t.Fatal(line)
	}
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"interrupted"}}`))
	item = <-b.out
	if !strings.Contains(item.Text, "状态：已停止") || !strings.Contains(item.Text, "actual-model") {
		t.Fatal(item.Text)
	}
	b.enqueue(Input{ChatID: "chat-1", UserID: "ou_owner", ThreadID: s.ThreadID, MessageID: "queued"}, "later", false)
	item = <-b.out
	if !strings.Contains(item.Text, "状态：排队中（1条）") || !strings.Contains(item.Text, "下轮设置 · 模型：next-model") {
		t.Fatal(item.Text)
	}
	b.cancelQueued(Input{ChatID: "chat-1", UserID: "ou_owner", ThreadID: s.ThreadID})
	item = <-b.out
	if !strings.Contains(item.Text, "状态：空闲") || len(rpc.calls) != 0 {
		t.Fatalf("header queried provider or retained queue: %s, %v", item.Text, rpc.calls)
	}
}

func TestReplyStatusDoesNotBorrowAnotherSessionsActiveModel(t *testing.T) {
	b, rpc := setup(t)
	a := catalogSession(t, b, "active")
	other := catalogSession(t, b, "other")
	b.mu.Lock()
	b.active = &task{thread: a.ThreadID, turn: "turn-1", modelSettings: modelsettings.Effective{Model: "active-model", Effort: "high"}}
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	b.respond(Input{ChatID: "chat-1", UserID: "ou_owner"}, "结果", other.ThreadID)
	item := <-b.out
	if strings.Contains(item.Text, "active-model") || !strings.Contains(item.Text, "状态：空闲") || !strings.Contains(string(item.Card), "继承默认") || len(rpc.calls) != 0 {
		t.Fatal(item.Text)
	}
	other.OriginHome = "/synthetic/external"
	if _, err := b.store.RememberSession(other); err != nil {
		t.Fatal(err)
	}
	if text := b.sessionLine(other.ThreadID); !strings.Contains(text, "外部只读") || strings.Contains(text, "下轮设置") {
		t.Fatal(text)
	}
}
