//go:build integration

package bridge

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
)

// Exercise the installed protocol with an empty home and synthetic configuration.
// No credentials, saved sessions, turn/start or model inference are used.
func TestWritableRootsProtocolWithoutInference(t *testing.T) {
	b, _ := setup(t)
	b.cfg.CodexHome = t.TempDir()
	b.cfg.WritableRoots = []string{t.TempDir(), t.TempDir()}
	if err := os.WriteFile(filepath.Join(b.cfg.CodexHome, "config.toml"), []byte("[sandbox_workspace_write]\nnetwork_access = true\nexclude_slash_tmp = true\nexclude_tmpdir_env_var = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := codex.Start(ctx, "codex", b.cfg.ProjectDir, b.cfg.CodexHome, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := client.Call(ctx, "thread/start", b.sessionParams(b.cfg.ProjectDir))
	if err != nil {
		t.Fatal("installed App Server rejected the directory configuration")
	}
	var response struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        struct {
			Type                string   `json:"type"`
			WritableRoots       []string `json:"writableRoots"`
			NetworkAccess       bool     `json:"networkAccess"`
			ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
			ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
		} `json:"sandbox"`
	}
	if json.Unmarshal(raw, &response) != nil || response.ApprovalPolicy != "on-request" || response.Sandbox.Type != "workspaceWrite" || !response.Sandbox.NetworkAccess || !response.Sandbox.ExcludeSlashTmp || !response.Sandbox.ExcludeTmpdirEnvVar {
		t.Fatal("directory override changed the existing execution policy")
	}
	for _, root := range b.cfg.WritableRoots {
		found := false
		for _, actual := range response.Sandbox.WritableRoots {
			found = found || root == actual
		}
		if !found {
			t.Fatal("installed App Server did not apply the configured writable roots")
		}
	}
}
