package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritableRootsResolveSymlinksAndDeduplicate(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, map[string]any{"writable_roots": []string{root, alias, root}})
	c, err := Load(path)
	if err != nil || len(c.WritableRoots) != 1 || c.WritableRoots[0] != root {
		t.Fatal("directory authorization did not resolve and deduplicate")
	}
}

func TestInvalidWritableRootsFailWithoutExposingPaths(t *testing.T) {
	file := filepath.Join(t.TempDir(), "path-do-not-print")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []map[string]any{
		{"writable_roots": []string{"relative-path-do-not-print"}},
		{"writable_roots": []string{filepath.Join(t.TempDir(), "missing-path-do-not-print")}},
		{"writable_roots": []string{file}},
		{"writable_roots": []string{""}},
		{"writable_roots": "path-do-not-print"},
		{"writable_roots": []string{t.TempDir()}, "sandbox": "read-only"},
	} {
		_, err := Load(writeConfig(t, edit))
		if err == nil || strings.Contains(err.Error(), "do-not-print") {
			t.Fatal("invalid authorization was accepted or leaked a configured path")
		}
	}
}
