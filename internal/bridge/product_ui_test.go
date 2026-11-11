package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestHomeProjectsAndResetDraftExposeScopeWithoutSubmitting(t *testing.T) {
	b, rpc := setup(t)
	s := catalogSession(t, b, "thread-1")
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: s.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if err := b.store.SetModelSettings(s.ThreadID, "ou_owner", state.ModelSettings{Model: "test-model", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), input("home", "/start"))
	home := <-b.out
	if !strings.Contains(home.Text, "当前项目：") || !strings.Contains(string(home.Card), "选择项目") || strings.Contains(home.Text, "/archive [") {
		t.Fatal("home is still a command dump")
	}
	b.handle(context.Background(), input("projects", "/projects"))
	projects := <-b.out
	if !strings.Contains(string(projects.Card), b.cfg.ProjectDir) || len(rpc.calls) != 0 {
		t.Fatal("project choice scanned upstream or missing")
	}
	b.handle(context.Background(), input("settings", "/effort #1"))
	<-b.out
	var menu string
	for token, g := range b.drafts {
		for _, c := range g.replies {
			if c.id == "inherit" && strings.Contains(c.label, "模型和强度") {
				menu = token
			}
		}
	}
	if menu == "" {
		t.Fatal("whole reset label absent")
	}
	before := len(rpc.calls)
	_, draft := openDraft(t, b, menu, "inherit")
	if !strings.Contains(string(draft.Card), "模型和思考强度都会变化") || !strings.Contains(string(draft.Card), "test-model / high") || len(rpc.calls) != before || len(b.in) != 0 {
		t.Fatal("reset scope missing or click submitted")
	}
	_, prefs := b.store.ModelPreferences()
	if prefs[s.ThreadID].Effort != "high" {
		t.Fatal("opening reset changed settings")
	}
}

func TestModelSelectionPagesBeyondEighteenAndShowsSavedValuesOnFailure(t *testing.T) {
	b, rpc := setup(t)
	catalogSession(t, b, "thread-1")
	rpc.modelCall = func(any) (json.RawMessage, error) {
		var data []map[string]any
		for i := 0; i < 22; i++ {
			name := fmt.Sprintf("model-%02d", i)
			data = append(data, map[string]any{"id": name, "model": name, "isDefault": i == 0, "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []map[string]string{{"reasoningEffort": "medium"}}})
		}
		return json.Marshal(map[string]any{"data": data, "nextCursor": nil})
	}
	b.handle(context.Background(), input("page-two", "/model #1 page 2"))
	item := <-b.out
	if !strings.Contains(string(item.Card), "model-18") || strings.Contains(string(item.Card), `"content":"model-00"`) {
		t.Fatal("models beyond 18 inaccessible")
	}
	var menu string
	for token, g := range b.drafts {
		for _, c := range g.replies {
			if c.text == "/model #1 model-18" {
				menu = token
			}
		}
	}
	if menu == "" {
		t.Fatal("page lost exact target")
	}
	if err := b.store.SetModelSettings("thread-1", "ou_owner", state.ModelSettings{Model: "saved-model", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	rpc.modelCall = func(any) (json.RawMessage, error) { return nil, fmt.Errorf("fixture-private-error") }
	b.handle(context.Background(), input("offline-settings", "/model #1"))
	item = <-b.out
	if !strings.Contains(item.Text, "已保存配置（未校验）") || !strings.Contains(item.Text, "saved-model") || strings.Contains(item.Text, "fixture-private") {
		t.Fatal("saved config lost or error exposed")
	}
}

func TestActionsDependOnTaskStateAndOldDraftReasonsRemainOwnerScoped(t *testing.T) {
	b, _ := setup(t)
	s := catalogSession(t, b, "thread-1")
	b.respond(Input{ChatID: "chat-1", UserID: "ou_owner"}, "任务已停止，已执行修改保留，排队执行失败。", s.ThreadID)
	<-b.out
	var menu string
	for token, g := range b.drafts {
		if !g.menu {
			continue
		}
		menu = token
		for _, c := range g.replies {
			if c.id == "stop" || c.id == "cancel" {
				t.Fatal("wording manufactured an unavailable action")
			}
		}
	}
	first, _ := openDraft(t, b, menu, "history")
	_, _ = openDraft(t, b, first, "status")
	closed := b.HandleAction(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: first, Action: "draft_send", Text: "/history #1"})
	if !closed.Error || !strings.Contains(closed.Message, "替换") {
		t.Fatal(closed.Message)
	}
	foreign := b.HandleAction(CardAction{ChatID: "chat-1", UserID: "other", Token: first, Action: "draft_send", Text: "/history #1"})
	if strings.Contains(foreign.Message, "替换") {
		t.Fatal("closed reason disclosed to foreign identity")
	}
	b.draftMu.Lock()
	token, ok := b.newDraft(commandDraft{chat: "chat-1", owner: "ou_owner", expires: time.Now().Add(-time.Minute)})
	b.draftMu.Unlock()
	if !ok {
		t.Fatal("fixture draft")
	}
	expired := b.HandleAction(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_open", Command: "status"})
	if !expired.Error || !strings.Contains(expired.Message, "有效期") || len(b.in) != 0 {
		t.Fatal("expired draft replayed or reason lost")
	}
}

func TestRecoveryPointsToOriginalOwnedSessionWithoutReplaying(t *testing.T) {
	b, rpc := setup(t)
	source := catalogSession(t, b, "source")
	other := catalogSession(t, b, "other")
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: other.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if !b.recoveryReply("chat-1", "ou_owner", "重启后结果未知，未自动重新执行。", inputReceipt{Input: Input{ChatID: "chat-1", UserID: "ou_owner", ThreadID: source.ThreadID}}, inputReceipt{Input: Input{ChatID: "other-chat", UserID: "other", ThreadID: other.ThreadID}}) {
		t.Fatal("recovery not retained")
	}
	item := <-b.out
	if !strings.Contains(item.Text, "待确认对象：会话 #1") || strings.Contains(item.Text, "待确认对象：会话 #2") || len(rpc.calls) != 0 || len(b.in) != 0 {
		t.Fatal(item.Text)
	}
	found := false
	for _, g := range b.drafts {
		for _, c := range g.replies {
			if c.text == "/history #1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("recovery lost original target")
	}
}
