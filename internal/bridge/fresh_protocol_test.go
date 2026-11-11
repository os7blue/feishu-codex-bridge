//go:build integration

package bridge

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
)

// Only metadata reaches the real server. The inference boundary is intercepted;
// the empty test home never receives real configuration, credentials or history.
type metadataOnlyRPC struct {
	RPC
	calls []string
}

func (r *metadataOnlyRPC) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.calls = append(r.calls, method)
	if strings.HasPrefix(method, "turn/") {
		return json.RawMessage(`{"turn":{"id":"intercepted-no-inference"}}`), nil
	}
	return r.RPC.Call(ctx, method, params)
}

func TestFreshSessionProtocolWithoutInference(t *testing.T) {
	b, _ := setup(t)
	b.cfg.CodexHome = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := codex.Start(ctx, "codex", b.cfg.ProjectDir, b.cfg.CodexHome, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rpc := &metadataOnlyRPC{RPC: client}
	b.rpc = rpc
	b.handle(ctx, input("new", "/new"))
	binding, ok := b.store.Binding("chat-1")
	if !ok || binding.ThreadID == "" {
		t.Fatal("real thread/start failed")
	}
	b.handle(ctx, input("first", "continue from the handoff"))
	b.jobs.Wait()
	if b.active == nil || b.active.turn != "intercepted-no-inference" {
		t.Fatal("first prompt failed before inference boundary")
	}
	if got := strings.Join(rpc.calls, ","); got != "model/list,thread/start,model/list,turn/start" {
		t.Fatalf("unexpected protocol calls: %s", got)
	}
}
