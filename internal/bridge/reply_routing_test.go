package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestReplyChainUsesKnownRootAndKeepsDefault(t *testing.T) {
	b, _ := setup(t)
	a, _ := b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	other, _ := b.store.RememberSession(state.Session{ThreadID: "thread-b", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	b.store.Bind("chat-1", state.Binding{ThreadID: other.ThreadID, OwnerID: "ou_owner"})
	if err := b.store.RememberRoutes([]string{"bot-root"}, state.MessageRoute{ChatID: "chat-1", OwnerID: "ou_owner", ThreadID: a.ThreadID, Created: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	in := input("reply-chain", "continue")
	in.ReplyTo = "unknown-parent"
	in.ReplyRoot = "bot-root"
	if !b.routeInput(&in) || in.ThreadID != a.ThreadID || !in.Explicit {
		t.Fatal("reply chain lost its known root")
	}
	binding, _ := b.store.Binding("chat-1")
	if binding.ThreadID != other.ThreadID {
		t.Fatal("reply chain changed default")
	}
}

func TestReplyRootNeverOverridesParentOrInvalidIdentity(t *testing.T) {
	for _, mode := range []string{"known-parent", "foreign-parent", "expired-parent", "missing-root"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := setup(t)
			b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
			b.store.RememberSession(state.Session{ThreadID: "thread-b", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
			b.store.Bind("chat-1", state.Binding{ThreadID: "thread-b", OwnerID: "ou_owner"})
			routes := []string{"root", "parent"}
			owner := "ou_owner"
			created := time.Now().Unix()
			if mode == "foreign-parent" {
				owner = "ou_other"
			}
			if mode == "expired-parent" {
				created = time.Now().Add(-8 * 24 * time.Hour).Unix()
			}
			// Write both together: pruning a stale route is not the subject of this check.
			if err := b.store.RememberRoutes([]string{"root"}, state.MessageRoute{ChatID: "chat-1", OwnerID: "ou_owner", ThreadID: "thread-a", Created: time.Now().Unix()}); err != nil {
				t.Fatal(err)
			}
			if mode != "missing-root" {
				if err := b.store.RememberRoutes(routes[1:], state.MessageRoute{ChatID: "chat-1", OwnerID: owner, ThreadID: "thread-b", Created: created}); err != nil {
					t.Fatal(err)
				}
			}
			in := input("reply-"+mode, "continue")
			in.ReplyTo = "parent"
			in.ReplyRoot = "root"
			if mode == "missing-root" {
				in.ReplyRoot = "unknown"
			}
			accepted := b.routeInput(&in)
			if mode == "known-parent" {
				if !accepted || in.ThreadID != "thread-b" {
					t.Fatal("root overrode direct parent")
				}
			} else if accepted {
				t.Fatal("invalid parent or root silently retargeted reply")
			}
		})
	}
}

func TestUsersOwnTaskMessageRemainsReplyableAfterDefaultSwitch(t *testing.T) {
	b, _ := setup(t)
	b.handle(context.Background(), input("user-original", "do work"))
	b.jobs.Wait()
	route, ok := b.store.Route("user-original", "chat-1", "ou_owner")
	if !ok || route.ThreadID != "thread-1" {
		t.Fatal("accepted user message has no reply route")
	}
	b.store.Bind("chat-1", state.Binding{ThreadID: "thread-other", OwnerID: "ou_owner"})
	in := input("reply-to-user", "continue")
	in.ReplyTo = "user-original"
	if !b.routeInput(&in) || in.ThreadID != "thread-1" {
		t.Fatal("reply to user task followed new default")
	}
}

func TestGlobalPanelManualSendCapturesTargetAndMakesOriginalCardReplyable(t *testing.T) {
	for _, text := range []string{"继续任务", "/commands #1", "#2 继续另一个任务"} {
		t.Run(text, func(t *testing.T) {
			b, rpc := setup(t)
			b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
			b.store.RememberSession(state.Session{ThreadID: "thread-b", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
			b.emitChoices(input("list", "/sessions"), "list", []replyChoice{{"session", "会话 #1", "/commands #1"}}, "")
			var menu string
			for token := range b.drafts {
				menu = token
			}
			token, _ := openDraft(t, b, menu, "session")
			if len(b.in) != 0 || len(rpc.calls) != 0 {
				t.Fatal("opening a global row submitted work")
			}
			if _, ok := b.store.Route("original-card", "chat-1", "ou_owner"); ok {
				t.Fatal("opening a row changed card routing")
			}
			b.store.Bind("chat-1", state.Binding{ThreadID: "thread-b", OwnerID: "ou_owner"})
			expected := "thread-a"
			if text == "#2 继续另一个任务" {
				expected = "thread-b"
			}
			b.active = &task{chat: "chat-1", owner: "ou_owner", thread: expected, turn: "turn-fixture"}
			rpc.call = func(method string, params any) (json.RawMessage, error) {
				if method != "turn/steer" || params.(map[string]any)["threadId"] != expected {
					t.Fatal("draft used a different target")
				}
				return json.RawMessage(`{}`), nil
			}
			action := CardAction{ChatID: "chat-1", UserID: "ou_owner", MessageID: "original-card", Token: token, Action: "draft_send", Text: text}
			forged := action
			forged.UserID = "ou_other"
			if !b.HandleCard(forged).Error || len(b.in) != 0 {
				t.Fatal("other owner submitted card")
			}
			if b.HandleCard(action).Error || b.HandleCard(action).Error || len(b.in) != 1 {
				t.Fatal("manual send was rejected or duplicated")
			}
			b.handle(context.Background(), <-b.in)
			route, ok := b.store.Route("original-card", "chat-1", "ou_owner")
			if !ok || route.ThreadID != expected {
				t.Fatal("submitted source card has no correct reply route")
			}
			if _, ok := b.store.Route("original-card", "chat-1", "ou_other"); ok {
				t.Fatal("source card accepted another owner")
			}
			reply := input("quoted-card", "continue")
			reply.ReplyTo = "original-card"
			if !b.routeInput(&reply) || reply.ThreadID != expected {
				t.Fatal("reply to submitted card followed default")
			}
			if binding, _ := b.store.Binding("chat-1"); binding.ThreadID != "thread-b" {
				t.Fatal("card changed default")
			}
		})
	}
}
