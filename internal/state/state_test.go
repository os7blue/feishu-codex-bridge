package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceLockExcludesLiveBridgeAndDoesNotReadState(t *testing.T) {
	dir := t.TempDir()
	if lock, err := LockForMaintenance(dir); err == nil {
		lock.Close()
		t.Fatal("wrong directory created a maintenance lock")
	}
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := LockForMaintenance(dir); err == nil {
		lock.Close()
		t.Fatal("maintenance locked a running bridge")
	}
	s.Close()
	path := filepath.Join(dir, "state.json")
	const opaque = "synthetic opaque state; must not be parsed or rewritten"
	if err := os.WriteFile(path, []byte(opaque), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := LockForMaintenance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if next, err := Open(dir, "app", "/project"); err == nil {
		next.Close()
		t.Fatal("bridge started during offline public replacement")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, []byte(opaque)) {
		t.Fatal("maintenance read/rewrote opaque state")
	}
	lock.Close()
	if next, err := LockForMaintenance(dir); err != nil {
		t.Fatal("maintenance lock was not released")
	} else {
		next.Close()
	}
}

func TestMaintenanceLockRejectsSymlinkAndNonLockEntries(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "bridge.lock")
		if symlink {
			target := filepath.Join(dir, "other")
			os.WriteFile(target, []byte("fixture"), 0600)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if lock, err := LockForMaintenance(dir); err == nil {
			lock.Close()
			t.Fatal("nonregular existing state lock accepted")
		}
	}
}

func TestBindingsOwnerAndReceiptsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind("chat", Binding{ThreadID: "thread", OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim("message", time.Now()); !ok || err != nil {
		t.Fatal("first claim failed", err)
	}
	if _, err := Open(dir, "app", "/project"); err == nil {
		t.Fatal("second process obtained state lock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.PairedID() != "ou_owner" {
		t.Fatal("owner lost")
	}
	if b, ok := s.Binding("chat"); !ok || b.ThreadID != "thread" || b.OwnerID != "ou_owner" {
		t.Fatal("binding lost")
	}
	if ok, err := s.Claim("message", time.Now()); ok || err != nil {
		t.Fatal("duplicate message was accepted", err)
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state permissions not private", err)
	}
}

func TestSessionReferencesAndReplyRoutesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RememberSession(Session{ThreadID: "first", Name: "Original", Summary: "Build the bridge", Cwd: "/project", OwnerID: "ou_owner"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimHandoff(first.ThreadID); !claimed || err != nil {
		t.Fatal("handoff claim failed", err)
	}
	if err := s.RememberRoutes([]string{"reply"}, MessageRoute{ChatID: "chat", OwnerID: "ou_owner", ThreadID: "first", Created: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	updated, err := s.RememberSession(Session{ThreadID: "first"})
	if err != nil || updated.Ref != first.Ref || updated.Name != "Original" || updated.Summary != "Build the bridge" || updated.Cwd != "/project" || updated.OwnerID != "ou_owner" || !updated.HandoffRequested {
		t.Fatal("refresh lost session identity or ownership")
	}
	if claimed, err := s.ClaimHandoff(first.ThreadID); claimed || err != nil {
		t.Fatal("handoff repeated after restart", err)
	}
	second, err := s.RememberSession(Session{ThreadID: "second"})
	if err != nil || second.Ref == first.Ref {
		t.Fatal("session reference reused after restart")
	}
	if route, ok := s.Route("reply", "chat", "ou_owner"); !ok || route.ThreadID != "first" {
		t.Fatal("reply route lost after restart")
	}
	if _, err := s.RememberSession(Session{ThreadID: "first", OwnerID: "ou_other"}); err == nil {
		t.Fatal("session reassigned to another account")
	}
}

func TestNotificationTargetsOnlyPairedOwnerWithoutWriterLock(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Bind("stranger-chat", Binding{ThreadID: "x", OwnerID: "ou_other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := NotificationChat(dir, "app", "/project"); err == nil {
		t.Fatal("notification sent before pairing")
	}
	if err := s.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := NotificationChat(dir, "app", "/project"); err == nil {
		t.Fatal("notification used a stranger's binding")
	}
	if err := s.Bind("owner-chat", Binding{ThreadID: "y", OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	chat, err := NotificationChat(dir, "app", "/project")
	if err != nil || chat != "owner-chat" {
		t.Fatal("cannot notify verified owner while bridge runs")
	}
	if _, err := NotificationChat(dir, "other-app", "/project"); err == nil {
		t.Fatal("notification crossed application boundary")
	}
}

func TestStateCannotBeReusedByAnotherApp(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(dir, "other-app", "/project"); err == nil {
		t.Fatal("another app reused owner credentials")
	}
	if _, err := Open(dir, "app", "/other-project"); err == nil {
		t.Fatal("another project reused session bindings")
	}
}

func TestCorruptStateIsNotSilentlyReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "app", "/project"); err == nil {
		t.Fatal("corrupt state accepted")
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || string(data) != `{"version":999}` {
		t.Fatal("corrupt state overwritten")
	}
}
