package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type conversationSender struct {
	roots, mains, copies int
	failCopy             bool
	targets              []string
	texts                []string
}

func (s *conversationSender) Send(context.Context, string, string, string) error {
	return errors.New("unexpected legacy delivery")
}
func (s *conversationSender) SendRouted(_ context.Context, chat, text, key string) ([]string, error) {
	s.roots++
	return []string{fmt.Sprintf("root-%d", s.roots)}, nil
}
func (*conversationSender) ConversationLink(chat, root string) string {
	return "fixture://" + chat + "/" + root
}
func (s *conversationSender) SendTextWithConversation(_ context.Context, chat, root, text, key string) ([]string, error) {
	s.mains++
	s.targets = append(s.targets, root)
	s.texts = append(s.texts, text)
	return []string{fmt.Sprintf("main-%d", s.mains)}, nil
}
func (s *conversationSender) SendCardWithConversation(_ context.Context, chat, root string, card json.RawMessage, key string) ([]string, error) {
	s.mains++
	s.targets = append(s.targets, root)
	return []string{fmt.Sprintf("card-%d", s.mains)}, nil
}
func (s *conversationSender) SendConversationText(_ context.Context, chat, root, text, key string) ([]string, error) {
	s.copies++
	s.targets = append(s.targets, root)
	if s.failCopy {
		return nil, errors.New("synthetic topic offline")
	}
	return []string{fmt.Sprintf("copy-%d", s.copies)}, nil
}

func TestConversationRetainsMainReplyAndRetryDoesNotRepeatIt(t *testing.T) {
	b, _ := setup(t)
	s := &conversationSender{failCopy: true}
	b.sender = s
	b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	b.emit("chat-1", "complete current answer", "thread-a")
	item := <-b.out
	if b.deliverOne(context.Background(), item) || s.roots != 1 || s.mains != 1 {
		t.Fatal("main reply or root missing")
	}
	if got := readFixtureOutbox(t, b, item); len(got.DeliveredIDs) != 1 {
		t.Fatal("main receipt not saved before topic retry")
	}
	s.failCopy = false
	if !b.deliverOne(context.Background(), item) || s.roots != 1 || s.mains != 1 || s.copies != 2 {
		t.Fatal("retry repeated main reply or replaced root")
	}
	if s.texts[0] == "" || !strings.Contains(s.texts[0], "complete current answer") {
		t.Fatal("main reply became a summary")
	}
	b.store.RememberSession(state.Session{ThreadID: "thread-b", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	b.emit("chat-1", "other session", "thread-b")
	if !b.deliverOne(context.Background(), <-b.out) || s.roots != 2 {
		t.Fatal("sessions shared a root")
	}
	root, _ := b.store.ConversationRoot("chat-1", "ou_owner", "thread-a")
	in := input("inside", "continue")
	in.ReplyTo = "unknown-child"
	in.ReplyRoot = root
	b.store.Bind("chat-1", state.Binding{OwnerID: "ou_owner", ThreadID: "thread-b"})
	if !b.routeInput(&in) || in.ThreadID != "thread-a" {
		t.Fatal("topic reply followed another default session")
	}
	if _, ok := b.store.Route(root, "another-chat", "ou_owner"); ok {
		t.Fatal("topic crossed chats")
	}
	if _, ok := b.store.Route(root, "chat-1", "ou_other"); ok {
		t.Fatal("topic crossed owners")
	}
}

func TestConversationCopiesMainInputsOnceWithoutStartingModel(t *testing.T) {
	b, rpc := setup(t)
	s := &conversationSender{}
	b.sender = s
	b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	credential := "github_pat_" + strings.Repeat("a", 50)
	in := input("human-message", "full user request "+credential)
	in.ThreadID = "thread-a"
	if !b.rememberInputRoute(in) || !b.rememberInputRoute(in) {
		t.Fatal("input routing failed")
	}
	if len(b.out) != 1 {
		t.Fatal("input transcript was duplicated")
	}
	item := <-b.out
	if strings.Contains(item.Text, credential) || !strings.Contains(item.Text, "凭据已隐藏") {
		t.Fatal("credential copied into topic transcript")
	}
	if !item.Transcript || !b.deliverOne(context.Background(), item) || s.mains != 0 || s.copies != 1 {
		t.Fatal("input was echoed in main chat")
	}
	root, _ := b.store.ConversationRoot(in.ChatID, in.UserID, in.ThreadID)
	in.MessageID = "native-child"
	in.ReplyRoot = root
	if !b.rememberInputRoute(in) || len(b.out) != 0 || len(rpc.calls) != 0 {
		t.Fatal("native topic input copied or model called")
	}
}
