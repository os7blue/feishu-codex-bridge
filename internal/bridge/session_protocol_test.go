//go:build integration

package bridge

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

// Metadata commands exercise the real installed App Server without turn/start,
// turn/steer, or any model inference. All mutations use temporary private copies.
func TestMetadataSessionManagementWithoutInference(t *testing.T) {
	sourcePath, sourceHome := os.Getenv("FEISHU_METADATA_SOURCE"), os.Getenv("CODEX_HOME")
	if sourcePath == "" || sourceHome == "" {
		t.Fatal("set FEISHU_METADATA_SOURCE to a saved test rollout and CODEX_HOME to its configured home")
	}
	b, _ := setup(t)
	b.cfg.CodexHome = t.TempDir()
	external := t.TempDir()
	b.cfg.CatalogHomes = []string{external}
	for _, name := range []string{"config.toml", "auth.json"} {
		data, err := os.ReadFile(filepath.Join(sourceHome, name))
		if err != nil {
			t.Fatal("cannot read isolated test Codex configuration")
		}
		if err := os.WriteFile(filepath.Join(b.cfg.CodexHome, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal("saved test rollout unavailable")
	}
	first, rest, ok := strings.Cut(string(data), "\n")
	if !ok {
		t.Fatal("test rollout has no metadata")
	}
	var header map[string]json.RawMessage
	if json.Unmarshal([]byte(first), &header) != nil {
		t.Fatal("invalid test rollout")
	}
	var payload map[string]any
	if json.Unmarshal(header["payload"], &payload) != nil {
		t.Fatal("invalid test metadata")
	}
	id, _ := payload["id"].(string)
	payload["cwd"] = b.cfg.ProjectDir
	header["payload"], _ = json.Marshal(payload)
	firstBytes, _ := json.Marshal(header)
	dir := filepath.Join(external, "sessions", "2026", "11", "09")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, filepath.Base(sourcePath))
	snapshot := append(append(firstBytes, '\n'), []byte(rest)...)
	// A client may still be appending a frame; import only complete JSONL records.
	snapshot = append(snapshot, []byte(`{"partial":`)...)
	if err := os.WriteFile(fixture, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	session, err := b.store.RememberSession(state.Session{ThreadID: id, Cwd: b.cfg.ProjectDir, OwnerID: "ou_owner", OriginHome: external, RolloutPath: fixture})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	rpc, err := codex.Start(ctx, "codex", b.cfg.ProjectDir, b.cfg.CodexHome, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	b.rpc = rpc
	x := input("fork", "/fork #1")
	x.ThreadID = session.ThreadID
	x.Explicit = true
	b.forkSession(ctx, x)
	created, ok := b.store.SessionByRef(2)
	if !ok || created.ThreadID == id {
		t.Fatal("real App Server could not import and fork the saved rollout")
	}
	x.ThreadID = created.ThreadID
	if _, ok := b.readSession(ctx, x); !ok {
		t.Fatal("real thread/read failed")
	}
	for _, operation := range []struct {
		method string
		params map[string]any
	}{
		{"thread/items/list", map[string]any{"threadId": created.ThreadID, "limit": 12, "sortDirection": "desc"}},
		{"thread/name/set", map[string]any{"threadId": created.ThreadID, "name": "metadata test"}},
		{"thread/archive", map[string]any{"threadId": created.ThreadID}},
		{"thread/unarchive", map[string]any{"threadId": created.ThreadID}},
	} {
		if _, err := b.rpcCall(ctx, operation.method, operation.params); err != nil {
			t.Fatalf("metadata RPC %s failed: %v", operation.method, err)
		}
	}
	after, err := os.ReadFile(fixture)
	if err != nil || string(after) != string(snapshot) {
		t.Fatal("external source changed during fork")
	}
	t.Log("real metadata list/import/fork/read/items/name/archive/unarchive passed; no inference turns were started")
}
