package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestSessionDetailsDistinguishActiveAndNextModelSettings(t *testing.T) {
	for _, command := range []string{"/show #1", "/status #1"} {
		t.Run(command, func(t *testing.T) {
			b, rpc := setup(t)
			catalogSession(t, b, "thread-1")
			sessionRPC(b, rpc)
			if err := b.store.SetModelSettings("thread-1", "ou_owner", state.ModelSettings{Effort: "high"}); err != nil {
				t.Fatal(err)
			}
			b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1", started: time.Now(), modelSettings: modelsettings.Effective{Model: "test-model", Effort: "medium", ModelSource: "账号模型目录默认", EffortSource: "所选模型默认"}}
			b.handle(context.Background(), input(command, command))
			item := <-b.out
			for _, text := range []string{"当前轮实际使用：", "思考强度：medium", "下一轮模型配置", "思考强度：high（来源：会话覆盖）"} {
				if !strings.Contains(item.Text, text) {
					t.Fatalf("missing %q in %s", text, item.Text)
				}
			}
			if item.ThreadID != "thread-1" || b.active.modelSettings.Effort != "medium" {
				t.Fatal("view changed task or routing")
			}
			for _, method := range rpc.calls {
				if method != "thread/read" && method != "model/list" {
					t.Fatal("details started inference", rpc.calls)
				}
			}
			var choices []replyChoice
			for _, grant := range b.drafts {
				if grant.menu {
					choices = grant.replies
				}
			}
			found := map[string]bool{}
			for _, choice := range choices {
				found[choice.text] = true
			}
			if !found["/model #1"] || !found["/effort #1"] {
				t.Fatal("settings shortcuts lost session target")
			}
		})
	}
}

func TestSessionStatusKeepsActualModelWhenCatalogIsUnavailable(t *testing.T) {
	b, rpc := setup(t)
	catalogSession(t, b, "thread-1")
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1", started: time.Now(), modelSettings: modelsettings.Effective{Model: "test-model", Effort: "high"}}
	rpc.modelCall = func(any) (json.RawMessage, error) { return nil, errors.New("synthetic-private-provider-text") }
	b.handle(context.Background(), input("status-failure", "/status #1"))
	item := <-b.out
	if !strings.Contains(item.Text, "当前轮实际使用") || !strings.Contains(item.Text, "思考强度：high") || !strings.Contains(item.Text, "本地保存值") || strings.Contains(item.Text, "synthetic-private") || len(rpc.calls) != 0 {
		t.Fatal("status lost actual model or exposed provider error", item.Text)
	}
}

func TestReadOnlySessionDoesNotClaimBridgeModelConfiguration(t *testing.T) {
	b, rpc := setup(t)
	if _, err := b.store.RememberSession(state.Session{ThreadID: "external", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir, OriginHome: "/synthetic/external"}); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), input("external-info", "/show #1"))
	item := <-b.out
	if !strings.Contains(item.Text, "未读取原客户端的模型配置") || strings.Contains(item.Text, "下一轮模型配置") || len(rpc.calls) != 0 {
		t.Fatal("read-only session used bridge defaults")
	}
}

func TestDetailsCatalogReadDoesNotBlockTaskMutex(t *testing.T) {
	b, rpc := setup(t)
	catalogSession(t, b, "thread-1")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	rpc.modelCall = func(any) (json.RawMessage, error) {
		close(entered)
		<-release
		return nil, errors.New("fixture unavailable")
	}
	x := input("slow-status", "/status #1")
	x.ThreadID = "thread-1"
	go func() { defer close(done); b.sessionModelInfo(context.Background(), x) }()
	<-entered
	defer func() { close(release); <-done }()
	available := make(chan struct{})
	go func() { b.mu.Lock(); b.mu.Unlock(); close(available) }()
	select {
	case <-available:
	case <-time.After(time.Second):
		t.Fatal("metadata lookup holds task mutex")
	}
}
