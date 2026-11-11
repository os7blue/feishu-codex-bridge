package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestStatusShowsBridgeDirectoryAuthorizationWithoutCallingCodex(t *testing.T) {
	b, rpc := setup(t)
	b.cfg.WritableRoots = []string{t.TempDir(), t.TempDir()}
	b.handle(context.Background(), input("status", "/status"))
	reply := <-b.out
	for _, root := range b.cfg.WritableRoots {
		if !strings.Contains(reply.Text, root) {
			t.Fatal("status did not identify the configured bridge directories")
		}
	}
	if len(rpc.calls) != 0 {
		t.Fatal("directory status invoked Codex")
	}
}

func TestWritableRootsReachNewResumedAndForkedThreads(t *testing.T) {
	for _, mode := range []string{"direct", "new-command", "resume", "fork"} {
		t.Run(mode, func(t *testing.T) {
			b, rpc := setup(t)
			b.cfg.WritableRoots = []string{t.TempDir(), t.TempDir()}
			threadCalls, turns := 0, 0
			rpc.call = func(method string, params any) (json.RawMessage, error) {
				p := params.(map[string]any)
				switch method {
				case "thread/start", "thread/resume", "thread/fork":
					threadCalls++
					settings, ok := p["config"].(map[string]any)
					if !ok || len(settings) != 1 || !reflect.DeepEqual(settings["sandbox_workspace_write.writable_roots"], b.cfg.WritableRoots) {
						t.Error("thread did not receive the durable directory authorization")
					}
					if p["sandbox"] != "workspace-write" || p["approvalPolicy"] != "on-request" || p["developerInstructions"] != sessionInstructions {
						t.Error("directory authorization changed other execution rules")
					}
					id := "thread-1"
					if method == "thread/fork" {
						id = "forked"
					}
					return json.RawMessage(`{"thread":{"id":"` + id + `"}}`), nil
				case "turn/start":
					turns++
					if _, replaced := p["sandboxPolicy"]; replaced {
						t.Error("turn replaced the inherited policy")
					}
					return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
				}
				return json.RawMessage(`{}`), nil
			}
			switch mode {
			case "new-command":
				b.handle(context.Background(), input("new", "/new"))
			case "resume":
				catalogSession(t, b, "thread-1")
				if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-1", OwnerID: "ou_owner"}); err != nil {
					t.Fatal(err)
				}
			case "fork":
				catalogSession(t, b, "source")
				b.handle(context.Background(), input("fork", "/fork #1"))
			}
			text := "continue"
			if mode == "fork" {
				text = "#2 continue"
			}
			b.handle(context.Background(), input("first", text))
			b.jobs.Wait()
			if b.active == nil || threadCalls != 1 || turns != 1 {
				t.Fatal("expected one configured thread lifecycle and a started task")
			}
			thread := b.active.thread
			b.event(notification("turn/completed", `{"threadId":"`+thread+`","turn":{"id":"turn-1","status":"completed"}}`))
			if mode == "fork" {
				text = "#2 continue again"
			}
			b.handle(context.Background(), input("second", text))
			b.jobs.Wait()
			if b.active == nil || threadCalls != 1 || turns != 2 {
				t.Fatal("loaded thread did not retain its configured policy for the next task")
			}
		})
	}
}

func TestNoWritableRootOverrideForLegacyOrReadOnlyConfiguration(t *testing.T) {
	b, _ := setup(t)
	if _, ok := b.sessionParams(b.cfg.ProjectDir)["config"]; ok {
		t.Fatal("legacy configuration acquired an override")
	}
	b.cfg.Sandbox = "read-only"
	b.cfg.WritableRoots = []string{t.TempDir()}
	if _, ok := b.sessionParams(b.cfg.ProjectDir)["config"]; ok {
		t.Fatal("read-only session acquired write authorization")
	}
}

func TestAuthorizedWorkspaceRootsDoNotAllowSiblingOrSymlinkEscape(t *testing.T) {
	b, _ := setup(t)
	base := t.TempDir()
	allowed, outside := filepath.Join(base, "allowed"), filepath.Join(base, "allowed-other")
	for _, path := range []string{allowed, outside, filepath.Join(allowed, "nested")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	b.cfg.WritableRoots = []string{allowed}
	if !b.workspaceAllowed(allowed) || !b.workspaceAllowed(filepath.Join(allowed, "nested")) || b.workspaceAllowed(outside) || b.workspaceAllowed(link) {
		t.Fatal("workspace directory authorization escaped its configured roots")
	}
	b.cfg.Sandbox = "read-only"
	if b.workspaceAllowed(allowed) {
		t.Fatal("read-only configuration expanded the workspace")
	}
}
