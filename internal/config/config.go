package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	ConfigPath         string   `json:"-"`
	AppID              string   `json:"app_id"`
	AppSecret          string   `json:"app_secret"`
	ProjectDir         string   `json:"project_dir"`
	StateDir           string   `json:"state_dir"`
	AllowedOpenIDs     []string `json:"allowed_open_ids"`
	CodexBinary        string   `json:"codex_binary"`
	CodexHome          string   `json:"codex_home"`
	CatalogHomes       []string `json:"catalog_homes,omitempty"`
	Model              string   `json:"model"`
	ApprovalPolicy     string   `json:"approval_policy"`
	Sandbox            string   `json:"sandbox"`
	WritableRoots      []string `json:"writable_roots,omitempty"`
	TurnTimeoutSeconds int      `json:"turn_timeout_seconds"`
}

// Load never includes configuration contents in an error: it can contain credentials.
func Load(path string) (Config, error) {
	c := Config{CodexBinary: "codex", ApprovalPolicy: "on-request", Sandbox: "workspace-write", TurnTimeoutSeconds: 3600}
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open configuration file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return c, errors.New("configuration must be a regular file smaller than 1 MiB")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return c, errors.New("configuration must be private (chmod 600)")
	}
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("invalid configuration JSON or unknown configuration field")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	c.ConfigPath, err = filepath.EvalSymlinks(path)
	if err != nil {
		return c, errors.New("cannot resolve configuration file")
	}
	c.ConfigPath, err = filepath.Abs(c.ConfigPath)
	if err != nil {
		return c, errors.New("cannot resolve configuration file")
	}
	if value := os.Getenv("FEISHU_APP_ID"); value != "" {
		c.AppID = value
	}
	if value := os.Getenv("FEISHU_APP_SECRET"); value != "" {
		c.AppSecret = value
	}
	if strings.TrimSpace(c.AppID) == "" || strings.TrimSpace(c.AppSecret) == "" {
		return c, errors.New("set app_id and app_secret in the private config or FEISHU_APP_ID / FEISHU_APP_SECRET")
	}
	if c.ApprovalPolicy != "on-request" && c.ApprovalPolicy != "untrusted" {
		return c, errors.New("approval_policy must be on-request or untrusted")
	}
	if c.Sandbox != "workspace-write" && c.Sandbox != "read-only" {
		return c, errors.New("sandbox must be workspace-write or read-only")
	}
	if len(c.WritableRoots) > 0 && c.Sandbox != "workspace-write" {
		return c, errors.New("writable_roots requires workspace-write sandbox")
	}
	seenRoots := make(map[string]bool)
	roots := make([]string, 0, len(c.WritableRoots))
	for _, root := range c.WritableRoots {
		if !filepath.IsAbs(root) {
			return c, errors.New("writable_roots must contain absolute existing directories")
		}
		resolved, resolveErr := filepath.EvalSymlinks(root)
		if resolveErr != nil {
			return c, errors.New("writable_roots must contain absolute existing directories")
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.IsDir() {
			return c, errors.New("writable_roots must contain absolute existing directories")
		}
		if !seenRoots[resolved] {
			seenRoots[resolved] = true
			roots = append(roots, resolved)
		}
	}
	c.WritableRoots = roots
	if c.TurnTimeoutSeconds < 60 || c.TurnTimeoutSeconds > 86400 {
		return c, errors.New("turn_timeout_seconds must be between 60 and 86400")
	}
	if c.CodexBinary == "" {
		return c, errors.New("codex_binary is required")
	}
	if c.ProjectDir == "" {
		return c, errors.New("project_dir is required")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, errors.New("cannot resolve configuration directory")
	}
	if c.CodexHome != "" {
		if !filepath.IsAbs(c.CodexHome) {
			c.CodexHome = filepath.Join(base, c.CodexHome)
		}
		c.CodexHome, err = filepath.EvalSymlinks(c.CodexHome)
		if err != nil {
			return c, errors.New("codex_home must be an existing directory")
		}
		info, err = os.Stat(c.CodexHome)
		if err != nil || !info.IsDir() {
			return c, errors.New("codex_home must be an existing directory")
		}
	}
	for i, home := range c.CatalogHomes {
		if !filepath.IsAbs(home) {
			home = filepath.Join(base, home)
		}
		resolved, resolveErr := filepath.EvalSymlinks(home)
		if resolveErr != nil {
			return c, errors.New("catalog_homes must contain existing directories")
		}
		stat, statErr := os.Stat(resolved)
		if statErr != nil || !stat.IsDir() {
			return c, errors.New("catalog_homes must contain existing directories")
		}
		c.CatalogHomes[i] = resolved
	}
	if !filepath.IsAbs(c.ProjectDir) {
		c.ProjectDir = filepath.Join(base, c.ProjectDir)
	}
	c.ProjectDir, err = filepath.EvalSymlinks(c.ProjectDir)
	if err != nil {
		return c, errors.New("project_dir must exist")
	}
	info, err = os.Stat(c.ProjectDir)
	if err != nil || !info.IsDir() {
		return c, errors.New("project_dir must be a directory")
	}
	// The bridge can be installed separately from the coding workspace. Keep its
	// credentials and state beside its private configuration, not in the target repo.
	if c.StateDir == "" {
		c.StateDir = filepath.Join(base, "state")
	}
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(base, c.StateDir)
	}
	c.StateDir = filepath.Clean(c.StateDir)
	// Resolve existing ancestors so a symlink cannot redirect private state outside the project.
	ancestor := c.StateDir
	for {
		resolved, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			rel, _ := filepath.Rel(ancestor, c.StateDir)
			c.StateDir = filepath.Join(resolved, rel)
			break
		}
		if !os.IsNotExist(resolveErr) || filepath.Dir(ancestor) == ancestor {
			return c, errors.New("cannot resolve state_dir")
		}
		ancestor = filepath.Dir(ancestor)
	}
	if c.StateDir == c.ProjectDir {
		return c, errors.New("state_dir must be separate from project_dir")
	}
	for _, id := range c.AllowedOpenIDs {
		if !strings.HasPrefix(id, "ou_") || len(id) < 4 {
			return c, errors.New("allowed_open_ids must contain Feishu open IDs")
		}
	}
	return c, nil
}
