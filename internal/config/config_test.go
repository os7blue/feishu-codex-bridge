package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, edits map[string]any) string {
	t.Helper()
	t.Setenv("FEISHU_APP_ID", "")
	t.Setenv("FEISHU_APP_SECRET", "")
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	object := map[string]any{"app_id": "cli_test", "app_secret": "secret-do-not-print", "project_dir": project}
	for key, value := range edits {
		object[key] = value
	}
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExternalStateAndDefaults(t *testing.T) {
	path := writeConfig(t, map[string]any{"state_dir": "private/state"})
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != filepath.Join(filepath.Dir(path), "private", "state") || c.Sandbox != "workspace-write" || c.ApprovalPolicy != "on-request" {
		t.Fatalf("incorrect defaults or external state: %+v", struct{ State, Sandbox, Approval string }{c.StateDir, c.Sandbox, c.ApprovalPolicy})
	}
}

func TestCodexHomeIsResolvedAndMustBeDirectory(t *testing.T) {
	for _, home := range []string{"project", "missing", "config.json"} {
		path := writeConfig(t, map[string]any{"codex_home": home})
		c, err := Load(path)
		if home == "project" {
			if err != nil || c.CodexHome != filepath.Join(filepath.Dir(path), home) {
				t.Fatal("relative codex_home did not resolve to the existing directory")
			}
		} else if err == nil {
			t.Fatal("accepted a missing directory or regular file as codex_home")
		}
	}
}

func TestExternalCatalogHomesAreExistingReadOnlySources(t *testing.T) {
	path := writeConfig(t, map[string]any{"catalog_homes": []string{"project"}})
	c, err := Load(path)
	if err != nil || len(c.CatalogHomes) != 1 || c.CatalogHomes[0] != filepath.Join(filepath.Dir(path), "project") {
		t.Fatal("catalog path did not resolve")
	}
	path = writeConfig(t, map[string]any{"catalog_homes": []string{"missing"}})
	if _, err := Load(path); err == nil {
		t.Fatal("accepted a missing external catalog")
	}
}

func TestUnsafeOrUnknownConfigurationRejected(t *testing.T) {
	for _, edit := range []map[string]any{{"approval_policy": "never"}, {"sandbox": "danger-full-access"}, {"turn_timeout_seconds": 0}, {"unknown": true}, {"allowed_open_ids": []string{"not-an-open-id"}}, {"app_secret": ""}} {
		path := writeConfig(t, edit)
		_, err := Load(path)
		if err == nil {
			t.Fatalf("accepted invalid configuration: %v", edit)
		}
		if strings.Contains(err.Error(), "secret-do-not-print") {
			t.Fatal("configuration error leaked secret")
		}
	}
}

func TestCredentialsFileMustBePrivate(t *testing.T) {
	path := writeConfig(t, nil)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted public credentials file")
	}
}

func TestStateSymlinkResolvesOutsideProject(t *testing.T) {
	path := writeConfig(t, nil)
	root := filepath.Dir(path)
	if err := os.Mkdir(filepath.Join(root, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "private"), filepath.Join(root, "project", "link")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	c["state_dir"] = filepath.Join(root, "project", "link", "state")
	data, _ = json.Marshal(c)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StateDir != filepath.Join(root, "private", "state") {
		t.Fatal("state symlink not resolved")
	}
}
