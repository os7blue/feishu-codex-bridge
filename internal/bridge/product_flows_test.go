package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type productSender struct{ messages chan string }

func (s productSender) Send(_ context.Context, _, text, _ string) error {
	s.messages <- text
	return nil
}

func TestSlowCatalogDoesNotBlockStopStatusOrCompletion(t *testing.T) {
	b, rpc := setup(t)
	s := catalogSession(t, b, "thread-1")
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: s.ThreadID, turn: "turn-1", started: time.Now(), modelSettings: modelsettings.Effective{Model: "test-model", Effort: "high"}}
	b.mu.Lock()
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	rpc.modelCall = func(any) (json.RawMessage, error) {
		close(entered)
		<-release
		return json.RawMessage(`{"data":[],"nextCursor":null}`), nil
	}
	rpc.call = func(method string, _ any) (json.RawMessage, error) {
		if method == "turn/interrupt" {
			stopped <- struct{}{}
		}
		return json.RawMessage(`{}`), nil
	}
	sender := productSender{make(chan string, 32)}
	b.sender = sender
	if err := b.Recover(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	var once sync.Once
	defer func() {
		once.Do(func() { close(release) })
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run did not stop")
		}
	}()
	if err := b.Submit(input("slow-models", "/models")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("catalog not entered")
	}
	if err := b.Submit(input("local-status", "/status #1")); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-sender.messages:
		if !strings.Contains(text, "本地保存值") {
			t.Fatal(text)
		}
	case <-time.After(time.Second):
		t.Fatal("status blocked by catalog")
	}
	if err := b.Submit(input("fast-stop", "/stop #1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop blocked by catalog")
	}
	rpc.events <- notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","text":"受控结果已到达"}}`)
	rpc.events <- notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"interrupted"}}`)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case text := <-sender.messages:
			if strings.Contains(text, "受控结果已到达") {
				return
			}
		case <-deadline:
			t.Fatal("completion blocked by catalog")
		}
	}
}

func TestCancelOneQueuedTaskPreservesOtherTasksAndOwner(t *testing.T) {
	b, _ := setup(t)
	s := catalogSession(t, b, "thread-1")
	for i := 0; i < 3; i++ {
		b.enqueue(Input{ChatID: "chat-1", UserID: "ou_owner", MessageID: fmt.Sprint("q", i), ThreadID: s.ThreadID}, fmt.Sprintf("task %d", i), false)
		<-b.out
	}
	middle := b.queue[1].ID
	b.cancelTask(Input{ChatID: "chat-1", UserID: "other"}, middle)
	<-b.out
	if len(b.queue) != 3 {
		t.Fatal("foreign cancellation")
	}
	b.handle(context.Background(), input("cancel-one", "/cancelone "+middle))
	<-b.out
	if len(b.queue) != 2 || b.queue[0].Text != "task 0" || b.queue[1].Text != "task 2" {
		t.Fatal(b.queue)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.StateDir, "queue-"+middle+".json")); !os.IsNotExist(err) {
		t.Fatal("cancelled task retained as runnable")
	}
	b.handle(context.Background(), input("queue-view", "/queue"))
	item := <-b.out
	if !strings.Contains(item.Text, "task 0") || !strings.Contains(string(item.Card), "仅取消此任务") || strings.Contains(item.Text, "task 1") {
		t.Fatal(item.Text)
	}
}

func TestLongResultCanBeRetrievedAndRemainsPrivateAcrossDeliveryAndRestart(t *testing.T) {
	b, _ := setup(t)
	s := catalogSession(t, b, "thread-1")
	transport := &fileFixture{}
	b.sender = transport
	text := strings.Repeat("完整成果\n", 12000)
	b.emit("chat-1", text, s.ThreadID)
	item := <-b.out
	_, tail, ok := strings.Cut(item.Text, "发送 /result ")
	if !ok {
		t.Fatal("missing result retrieval")
	}
	id, _ := splitWord(tail)
	if strings.Contains(item.Text, b.cfg.StateDir) {
		t.Fatal("private result path exposed")
	}
	b.handle(context.Background(), input("files-view", "/files #1"))
	panel := <-b.out
	found := false
	for _, grant := range b.drafts {
		for _, choice := range grant.replies {
			if choice.text == "/result "+id {
				found = true
			}
		}
	}
	if !strings.Contains(string(panel.Card), "完整结果.txt") || !found {
		t.Fatal("result not discoverable")
	}
	b.result(Input{ChatID: "other-chat", UserID: "ou_owner"}, id)
	foreign := <-b.out
	if foreign.FilePath != "" || strings.Contains(foreign.Text, text[:40]) {
		t.Fatal("cross-chat result leak")
	}
	b.handle(context.Background(), input("retrieve", "/result "+id))
	file := <-b.out
	if file.FilePath == "" || !b.deliverOne(context.Background(), file) || !strings.Contains(string(transport.sent), text) {
		t.Fatal("full result not delivered")
	}
	// Ordinary exported files also remain retrievable after transport cleanup.
	transport.key = ""
	b.queueFile(Input{ChatID: "chat-1", UserID: "ou_owner", ThreadID: s.ThreadID}, "report.txt", []byte("durable export"))
	export := <-b.out
	if !b.deliverOne(context.Background(), export) {
		t.Fatal("export failed")
	}
	if _, err := os.Stat(export.FilePath); !os.IsNotExist(err) {
		t.Fatal("transport snapshot not cleaned")
	}
	// Recreate the application object over the same synthetic repository/cache.
	copy := New(b.cfg, b.rpc, transport, b.store, b.log, b.renderer)
	copy.result(Input{ChatID: "chat-1", UserID: "ou_owner"}, export.Key)
	retrieved := <-copy.out
	transport.key = ""
	if !copy.deliverOne(context.Background(), retrieved) || string(transport.sent) != "durable export" {
		t.Fatal("artifact lost after delivery/restart")
	}
}

func TestContinuationUsesSourceProjectAndFullConfigurationDespiteDefaultSwitch(t *testing.T) {
	b, rpc := setup(t)
	source := catalogSession(t, b, "source")
	other := catalogSession(t, b, "other")
	dir := filepath.Join(b.cfg.ProjectDir, "source project")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs/HANDOFF.md"), []byte("synthetic handoff"), 0600); err != nil {
		t.Fatal(err)
	}
	source.Cwd = dir
	if _, err := b.store.RememberSession(source); err != nil {
		t.Fatal(err)
	}
	if err := b.store.SetModelSettings(source.ThreadID, "ou_owner", state.ModelSettings{Model: "test-model", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: other.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	rpc.call = func(method string, params any) (json.RawMessage, error) {
		p := params.(map[string]any)
		switch method {
		case "thread/start":
			if p["cwd"] != dir {
				t.Errorf("wrong project: %v", p)
			}
			return json.RawMessage(`{"thread":{"id":"continued"}}`), nil
		case "turn/start":
			if p["threadId"] != "continued" || p["effort"] != "high" {
				t.Errorf("lost copied settings: %v", p)
			}
			return json.RawMessage(`{"turn":{"id":"new-turn"}}`), nil
		}
		return json.RawMessage(`{}`), nil
	}
	b.handle(context.Background(), input("continue-source", fmt.Sprintf("/continue #%d", source.Ref)))
	b.jobs.Wait()
	binding, _ := b.store.Binding("chat-1")
	_, prefs := b.store.ModelPreferences()
	created, _ := b.store.Session("continued")
	if binding.ThreadID != "continued" || created.Cwd != dir || prefs["continued"] != (state.ModelSettings{Model: "test-model", Effort: "high"}) || b.active == nil || b.active.thread != "continued" {
		t.Fatal("continuation lost source scope")
	}
	before := len(rpc.calls)
	b.handle(context.Background(), input("continue-source", fmt.Sprintf("/continue #%d", source.Ref)))
	b.jobs.Wait()
	if len(rpc.calls) != before {
		t.Fatal("duplicate continuation replayed")
	}
}
