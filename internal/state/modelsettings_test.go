package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelPreferencesPersistAcrossMetadataRefreshAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir, "app", "/project")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.RememberSession(Session{ThreadID: "thread", OwnerID: "owner"}); e != nil {
		t.Fatal(e)
	}
	if e = s.SetModelSettings("", "owner", ModelSettings{Model: "model-a"}); e != nil {
		t.Fatal(e)
	}
	if e = s.SetModelSettings("thread", "other", ModelSettings{Effort: "high"}); e == nil {
		t.Fatal("foreign preference accepted")
	}
	if e = s.SetModelSettings("thread", "owner", ModelSettings{Effort: "high"}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RememberSession(Session{ThreadID: "thread", OwnerID: "owner", Name: "refreshed"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir, "app", "/project")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	g, o := s.ModelPreferences()
	if g.Model != "model-a" || o["thread"].Effort != "high" {
		t.Fatal("preferences lost", g, o)
	}
	o["thread"] = ModelSettings{}
	_, again := s.ModelPreferences()
	if again["thread"].Effort != "high" {
		t.Fatal("map not copied")
	}
	info, e := os.Stat(filepath.Join(dir, "state.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("preferences not private")
	}
	if e = s.SetModelSettings("thread", "owner", ModelSettings{}); e != nil {
		t.Fatal(e)
	}
	_, again = s.ModelPreferences()
	if len(again) != 0 {
		t.Fatal("inheritance did not clear override")
	}
}

func TestModelDefaultsAndFrozenSessionsCommitOrRollbackTogether(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RememberSession(Session{ThreadID: "thread", OwnerID: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelSettings("thread", "owner", ModelSettings{Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	backup := filepath.Join(dir, "retained-state.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	freeze := map[string]ModelSettings{"thread": {Model: "model-a", Effort: "high"}}
	if err := s.SetModelDefaults("owner", ModelSettings{Model: "model-b"}, freeze); err == nil {
		t.Fatal("failed save accepted")
	}
	g, o := s.ModelPreferences()
	if g != (ModelSettings{}) || o["thread"].Model != "" {
		t.Fatal("partial in-memory commit", g, o)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelDefaults("owner", ModelSettings{Model: "model-b"}, freeze); err != nil {
		t.Fatal(err)
	}
	freeze["thread"] = ModelSettings{}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "app", "/project")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, o = s.ModelPreferences()
	if g.Model != "model-b" || o["thread"] != (ModelSettings{Model: "model-a", Effort: "high"}) {
		t.Fatal("durable transaction or copy lost", g, o)
	}
}
