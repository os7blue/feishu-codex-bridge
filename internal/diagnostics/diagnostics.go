// Package diagnostics keeps bounded, payload-free interaction evidence.
package diagnostics

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Both axes are allowlisted: callers cannot accidentally persist an event,
// command, answer, credential, identity or request identifier as a map key.
var stages = set("request", "request_rejected", "prompt", "card_delivery", "request_delivery", "request_text_fallback", "demo_delivery", "text_delivery", "callback", "back_callback", "draft_open", "draft_send", "draft_back", "response", "approval_reviewer", "approval_policy", "auto_review", "command", "demo", "demo_open", "demo_send", "demo_result", "message_api")
var outcomes = set("received", "queued", "delivered", "failed", "expired", "invalid", "unauthorized", "unavailable", "unsupported", "capacity", "secret", "duplicate", "queue_full", "opened", "accepted", "declined", "answered", "resolved", "started", "completed", "approved", "denied", "timedOut", "aborted", "inProgress", "unknown", "transport", "api", "canceled", "invalid_response", "human_command", "command_approval", "file_approval", "user_input", "user", "auto_review", "guardian_subagent", "on-request", "untrusted", "never", "granular", "name_missing", "name_present")

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, value := range values {
		m[value] = true
	}
	return m
}

type Snapshot struct {
	Version              int                          `json:"version"`
	Counts               map[string]map[string]uint64 `json:"counts"`
	LastAPICode          int                          `json:"last_api_code"`
	LastHTTPStatus       int                          `json:"last_http_status"`
	LastApprovalReviewer string                       `json:"last_approval_reviewer"`
	LastApprovalPolicy   string                       `json:"last_approval_policy"`
}

type Collector struct {
	mu               sync.Mutex
	counts           map[string]map[string]uint64
	code, httpStatus int
	reviewer, policy string
}

// ApprovalSettings accepts only protocol enums, even when callers accidentally
// supply an unrecognized value or raw payload. This only observes settings.
func (c *Collector) ApprovalSettings(reviewer, policy string) {
	if c == nil {
		return
	}
	switch reviewer {
	case "user", "auto_review", "guardian_subagent":
	default:
		reviewer = "unknown"
	}
	switch policy {
	case "on-request", "untrusted", "never", "granular":
	default:
		policy = "unknown"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reviewer, c.policy = reviewer, policy
	c.recordLocked("approval_reviewer", reviewer)
	c.recordLocked("approval_policy", policy)
}

func (c *Collector) Record(stage, outcome string) {
	if c == nil || !stages[stage] || !outcomes[outcome] {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordLocked(stage, outcome)
}

func (c *Collector) recordLocked(stage, outcome string) {
	if c.counts == nil {
		c.counts = map[string]map[string]uint64{}
	}
	if c.counts[stage] == nil {
		c.counts[stage] = map[string]uint64{}
	}
	c.counts[stage][outcome]++
}

func (c *Collector) API(code, status int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code, c.httpStatus = code, status
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Snapshot{Version: 1, Counts: map[string]map[string]uint64{}, LastAPICode: c.code, LastHTTPStatus: c.httpStatus}
	s.LastApprovalReviewer, s.LastApprovalPolicy = c.reviewer, c.policy
	if s.LastApprovalReviewer == "" {
		s.LastApprovalReviewer = "unknown"
	}
	if s.LastApprovalPolicy == "" {
		s.LastApprovalPolicy = "unknown"
	}
	for stage, counts := range c.counts {
		s.Counts[stage] = map[string]uint64{}
		for outcome, n := range counts {
			s.Counts[stage][outcome] = n
		}
	}
	return s
}

// File exclusively reserves a new path. It never reads or overwrites existing
// files at startup. Subsequent snapshots atomically replace only that receipt.
type File struct {
	path      string
	collector *Collector
}

func (c *Collector) CreateFile(path string) (*File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("cannot create interaction evidence")
	}
	err = json.NewEncoder(f).Encode(c.Snapshot())
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, errors.New("cannot initialize interaction evidence")
	}
	return &File{path: path, collector: c}, nil
}

func (f *File) Save() error {
	return f.SaveFrom(f.collector)
}

func (f *File) SaveFrom(collector *Collector) error {
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".interaction-*.tmp")
	if err != nil {
		return errors.New("cannot save interaction evidence")
	}
	defer os.Remove(tmp.Name())
	err = json.NewEncoder(tmp).Encode(collector.Snapshot())
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot save interaction evidence")
	}
	if os.Rename(tmp.Name(), f.path) != nil {
		return errors.New("cannot publish interaction evidence")
	}
	return nil
}
