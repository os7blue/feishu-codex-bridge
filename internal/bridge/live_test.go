//go:build integration

package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/feishuui"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type localDelivery chan string

func (s localDelivery) Send(ctx context.Context, _, text, _ string) error {
	select {
	case s <- text:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// This test uses the installed Codex and model service, but never sends to Feishu.
// An explicitly supplied private CODEX_HOME keeps sessions out of existing apps.
func TestLiveCodexConversationSurvivesRestart(t *testing.T) {
	if os.Getenv("CODEX_HOME") == "" {
		t.Fatal("set CODEX_HOME to an isolated directory with working model credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	project := t.TempDir()
	stateDir := t.TempDir()
	marker := fmt.Sprintf("bridge_%d", time.Now().UnixNano())
	cfg := config.Config{AppID: "integration", ProjectDir: project, StateDir: stateDir,
		CodexHome:      os.Getenv("CODEX_HOME"),
		AllowedOpenIDs: []string{"ou_local_test"}, Sandbox: "read-only",
		ApprovalPolicy: "on-request", TurnTimeoutSeconds: 120}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var threadID string
	for round := 0; round < 2; round++ {
		func() {
			rpc, err := codex.Start(ctx, "codex", project, cfg.CodexHome, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer rpc.Close()
			store, err := state.Open(stateDir, cfg.AppID, project)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			deliveries := make(localDelivery, 64)
			b := New(cfg, rpc, deliveries, store, log, feishuui.Renderer{})
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- b.Run(runCtx) }()
			defer func() { stop(); <-done }()
			prompt := "Remember this exact marker: " + marker + ". Reply only with the marker. Do not use tools."
			if round == 1 {
				prompt = "Reply only with the exact marker from my previous message. Do not use tools."
			}
			if err := b.Submit(Input{ChatID: "local-chat", UserID: "ou_local_test", MessageID: fmt.Sprint(round), Text: prompt}); err != nil {
				t.Fatal(err)
			}
			for {
				select {
				case text := <-deliveries:
					if strings.Contains(text, marker) {
						binding, ok := store.Binding("local-chat")
						if !ok || (round == 1 && binding.ThreadID != threadID) {
							t.Fatal("session binding was not preserved")
						}
						threadID = binding.ThreadID
						t.Logf("round %d: real model response and persisted binding verified", round+1)
						return
					}
					if strings.Contains(text, "失败") || strings.Contains(text, "无法") || strings.Contains(text, "本轮执行结束") {
						t.Fatal("live conversation did not complete successfully; inspect isolated Codex session locally")
					}
				case err := <-done:
					// Leave a value for the deferred cleanup after an unexpected exit.
					done <- err
					t.Fatalf("bridge exited before response: %v", err)
				case <-ctx.Done():
					t.Fatal("timed out waiting for real model response")
				}
			}
		}()
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.json")); err != nil {
		t.Fatal("bridge state missing after restart")
	}
}
