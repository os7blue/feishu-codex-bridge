package state

import (
	"testing"
	"time"
)

func TestConversationRootsRemainScopedAndSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	s.RememberSession(Session{ThreadID: "a", OwnerID: "owner", Cwd: "/project"})
	route := MessageRoute{ChatID: "chat", OwnerID: "owner", ThreadID: "a", Created: time.Now().Add(-30 * 24 * time.Hour).Unix()}
	if err := s.RememberConversationRoot("root", route); err != nil {
		t.Fatal(err)
	}
	if err := s.RememberConversationRoot("other-root", route); err == nil {
		t.Fatal("root was replaced")
	}
	wrong := route
	wrong.OwnerID = "other-owner"
	if err := s.RememberConversationRoot("another", wrong); err == nil {
		t.Fatal("wrong owner bound root")
	}
	s.Close()
	s, err = Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if root, ok := s.ConversationRoot("chat", "owner", "a"); !ok || root != "root" {
		t.Fatal("restart lost root")
	}
	if route, ok := s.Route("root", "chat", "owner"); !ok || route.ThreadID != "a" {
		t.Fatal("old root expired")
	}
	if _, ok := s.Route("root", "chat", "other-owner"); ok {
		t.Fatal("wrong owner read root")
	}
	if _, ok := s.ConversationRoot("other-chat", "owner", "a"); ok {
		t.Fatal("wrong chat read root")
	}
}
