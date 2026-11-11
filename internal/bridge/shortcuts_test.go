package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestSessionListShortcutsKeepTargetsAndRequireManualSend(t *testing.T) {
	b, rpc := setup(t)
	rpc.call = func(method string, _ any) (json.RawMessage, error) {
		if method != "thread/list" {
			t.Fatalf("unexpected RPC %s", method)
		}
		return json.RawMessage(`{"data":[{"id":"thread-a","name":"A"},{"id":"thread-b","name":"B"}],"nextCursor":"next"}`), nil
	}
	b.handle(context.Background(), input("list", "/sessions"))
	item := <-b.out
	if len(item.Card) == 0 || !strings.Contains(string(item.Card), "下一页") {
		t.Fatal("session list did not offer shortcuts")
	}
	var menu string
	var choice replyChoice
	for token, g := range b.drafts {
		if g.menu {
			menu = token
			for _, c := range g.replies {
				if strings.HasPrefix(c.id, "session_") {
					choice = c
					break
				}
			}
		}
	}
	if choice.id == "" {
		t.Fatal("session row has no shortcut")
	}
	_, card := openDraft(t, b, menu, choice.id)
	if !strings.Contains(string(card.Card), choice.text) || len(b.in) != 0 || len(rpc.calls) != 1 {
		t.Fatal("click performed operation rather than opening draft")
	}
	for _, g := range b.drafts {
		if !g.menu {
			session, ok := b.store.Session(g.thread)
			if !ok || choice.text != fmt.Sprintf("/commands #%d", session.Ref) {
				t.Fatal("list action failed to capture its selected session")
			}
		}
	}
	if !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_other", Token: menu, Action: "draft_open", Command: choice.id}).Error {
		t.Fatal("shortcut opened for another account")
	}
}

func TestScopedStatusAndQueueShortcutsUseExactSessions(t *testing.T) {
	b, _ := setup(t)
	s, _ := b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	b.handle(context.Background(), input("status", fmt.Sprintf("/status #%d", s.Ref)))
	status := <-b.out
	if status.ThreadID != s.ThreadID || len(status.Card) == 0 {
		t.Fatal("scoped status lost routing")
	}
	for _, g := range b.drafts {
		for _, c := range g.replies {
			if c.id == "history" && c.text != fmt.Sprintf("/history #%d", s.Ref) {
				t.Fatal("status shortcut has wrong target")
			}
		}
	}
	b.queue = []queued{{Input: Input{ChatID: "chat-1", UserID: "ou_owner", ThreadID: s.ThreadID}}, {Input: Input{ChatID: "chat-2", UserID: "ou_other", ThreadID: "other"}}}
	b.handle(context.Background(), input("queue", "/queue"))
	q := <-b.out
	if len(q.Card) == 0 || !strings.Contains(string(q.Card), fmt.Sprintf("/cancel #%d", s.Ref)) || strings.Contains(q.Text, "other") {
		t.Fatal("queue lacks exact scoped cancellation or leaks another chat")
	}
}

func TestRecoveryShortcutsRetainNoticeAndNeverReplayWork(t *testing.T) {
	b, rpc := setup(t)
	if !b.recoveryReply("chat-1", "ou_owner", "重启后尚未确认；不会自动重新执行。") {
		t.Fatal("recovery reply not retained")
	}
	item := <-b.out
	if len(item.Card) == 0 || !strings.Contains(string(item.Card), "不会自动") || len(rpc.calls) != 0 {
		t.Fatal("recovery notice changed")
	}
	// Restart removes ephemeral UI grants: delivery must use original text.
	b.drafts = nil
	if !b.deliverOne(context.Background(), item) {
		t.Fatal("stale shortcut did not fall back to text")
	}
}
