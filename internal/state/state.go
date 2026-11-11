package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Binding struct {
	ThreadID string `json:"thread_id"`
	OwnerID  string `json:"owner_id"`
}

// An empty session pair inherits; a custom pair pins both fields. Partial pairs
// from older releases are frozen atomically before global defaults change.
type ModelSettings = modelsettings.Settings

type Session struct {
	Ref              int    `json:"ref"`
	ThreadID         string `json:"thread_id"`
	Name             string `json:"name,omitempty"`
	Summary          string `json:"summary,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	OwnerID          string `json:"owner_id,omitempty"`
	OriginHome       string `json:"origin_home,omitempty"`
	RolloutPath      string `json:"rollout_path,omitempty"`
	Archived         bool   `json:"archived,omitempty"`
	HandoffRequested bool   `json:"handoff_requested,omitempty"`
}

type MessageRoute struct {
	ChatID   string `json:"chat_id"`
	OwnerID  string `json:"owner_id"`
	ThreadID string `json:"thread_id"`
	Created  int64  `json:"created_at"`
}

type Data struct {
	ModelDefaults  ModelSettings            `json:"model_defaults,omitempty"`
	ModelOverrides map[string]ModelSettings `json:"model_overrides,omitempty"`
	Version        int                      `json:"version"`
	AppID          string                   `json:"app_id"`
	Project        string                   `json:"project"`
	PairedID       string                   `json:"paired_open_id,omitempty"`
	Bindings       map[string]Binding       `json:"bindings"`
	Seen           map[string]int64         `json:"seen"`
	Sessions       map[string]Session       `json:"sessions,omitempty"`
	Routes         map[string]MessageRoute  `json:"message_routes,omitempty"`
	NextRef        int                      `json:"next_ref,omitempty"`
}

// ModelPreferences returns copies, never the mutable saved maps.
func (s *Store) ModelPreferences() (ModelSettings, map[string]ModelSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	overrides := make(map[string]ModelSettings, len(s.data.ModelOverrides))
	for id, value := range s.data.ModelOverrides {
		overrides[id] = value
	}
	return s.data.ModelDefaults, overrides
}

func (s *Store) CheckModelScope(thread, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" {
		return errors.New("模型设置需要已认证账号。")
	}
	if thread == "" {
		return nil
	}
	session, ok := s.data.Sessions[thread]
	if !ok || session.OwnerID != owner {
		return errors.New("无法确认会话归属。")
	}
	return nil
}

// SetModelDefaults commits legacy session snapshots and new defaults together.
// This application-only port is called after authentication and validation.
func (s *Store) SetModelDefaults(owner string, value ModelSettings, overrides map[string]ModelSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" {
		return errors.New("model settings owner required")
	}
	copyOverrides := make(map[string]ModelSettings, len(overrides))
	for thread, prefs := range overrides {
		copyOverrides[thread] = prefs
	}
	oldGlobal, oldOverrides := s.data.ModelDefaults, s.data.ModelOverrides
	s.data.ModelDefaults, s.data.ModelOverrides = value, copyOverrides
	if err := s.save(); err != nil {
		s.data.ModelDefaults, s.data.ModelOverrides = oldGlobal, oldOverrides
		return err
	}
	return nil
}

func (s *Store) SetModelSettings(thread, owner string, value ModelSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" {
		return errors.New("model settings owner required")
	}
	if thread == "" {
		old := s.data.ModelDefaults
		s.data.ModelDefaults = value
		if err := s.save(); err != nil {
			s.data.ModelDefaults = old
			return err
		}
		return nil
	}
	session, ok := s.data.Sessions[thread]
	if !ok || session.OwnerID == "" || session.OwnerID != owner {
		return errors.New("session owner required")
	}
	if s.data.ModelOverrides == nil {
		s.data.ModelOverrides = map[string]ModelSettings{}
	}
	old, existed := s.data.ModelOverrides[thread]
	if value == (ModelSettings{}) {
		delete(s.data.ModelOverrides, thread)
	} else {
		s.data.ModelOverrides[thread] = value
	}
	if err := s.save(); err != nil {
		if existed {
			s.data.ModelOverrides[thread] = old
		} else {
			delete(s.data.ModelOverrides, thread)
		}
		return err
	}
	return nil
}

type Store struct {
	mu   sync.Mutex
	dir  string
	data Data
	lock *os.File
}

// LockForMaintenance holds the same OS lock as Open without loading or
// rewriting saved sessions. Offline upgrades require an existing state lock;
// a wrong directory must not silently create a new, unrelated lock.
func LockForMaintenance(dir string) (*os.File, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("maintenance state directory unavailable")
	}
	path := filepath.Join(dir, "bridge.lock")
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("existing maintenance state lock required")
	}
	f, err := acquireLock(path)
	if err != nil {
		return nil, errors.New("maintenance state lock unavailable; bridge may still be running")
	}
	after, err := f.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(after, current) {
		f.Close()
		return nil, errors.New("maintenance state lock identity changed")
	}
	return f, nil
}

// Open also obtains an OS advisory lock; a crashed process does not leave a stale lock.
func Open(dir, appID, project string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	f, err := acquireLock(filepath.Join(dir, "bridge.lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: f, data: Data{Version: 1, AppID: appID, Project: project, Bindings: map[string]Binding{}, Seen: map[string]int64{}}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		var loaded Data
		if json.Unmarshal(b, &loaded) != nil || loaded.Version != 1 || loaded.Bindings == nil || loaded.Seen == nil {
			s.Close()
			return nil, errors.New("invalid bridge state; do not replace it automatically")
		}
		s.data = loaded
		if s.data.AppID != appID || s.data.Project != project {
			s.Close()
			return nil, errors.New("state belongs to another Feishu app or project; use a separate state_dir")
		}
	} else if !os.IsNotExist(err) {
		s.Close()
		return nil, err
	}
	if s.data.Sessions == nil {
		s.data.Sessions = map[string]Session{}
	}
	if s.data.Routes == nil {
		s.data.Routes = map[string]MessageRoute{}
	}
	return s, nil
}

func (s *Store) RememberSession(session Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.ThreadID == "" {
		return Session{}, errors.New("empty session ID")
	}
	old, exists := s.data.Sessions[session.ThreadID]
	if exists {
		session.Ref = old.Ref
		session.HandoffRequested = session.HandoffRequested || old.HandoffRequested
		if old.OwnerID != "" {
			if session.OwnerID != "" && session.OwnerID != old.OwnerID {
				return Session{}, errors.New("session belongs to another owner")
			}
			session.OwnerID = old.OwnerID
		}
		if session.Cwd == "" {
			session.Cwd = old.Cwd
		}
		if session.Name == "" {
			session.Name = old.Name
		}
		if session.Summary == "" {
			session.Summary = old.Summary
		}
		if old.OriginHome != "" && session.OriginHome == "" {
			session.OriginHome = old.OriginHome
			session.RolloutPath = old.RolloutPath
			session.Archived = old.Archived
		}
	} else {
		// Preserve ownership from states written by older bridge versions.
		for _, binding := range s.data.Bindings {
			if binding.ThreadID == session.ThreadID && binding.OwnerID != "" {
				if session.OwnerID != "" && session.OwnerID != binding.OwnerID {
					return Session{}, errors.New("session belongs to another owner")
				}
				session.OwnerID = binding.OwnerID
			}
		}
		s.data.NextRef++
		session.Ref = s.data.NextRef
	}
	s.data.Sessions[session.ThreadID] = session
	if err := s.save(); err != nil {
		if exists {
			s.data.Sessions[session.ThreadID] = old
		} else {
			delete(s.data.Sessions, session.ThreadID)
			s.data.NextRef--
		}
		return Session{}, err
	}
	return session, nil
}

// ClaimHandoff persists a per-session latch so later turns and restarts cannot
// repeatedly steer the same conversation away from the user's current task.
func (s *Store) ClaimHandoff(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.data.Sessions[id]
	if !ok {
		return false, errors.New("unknown handoff session")
	}
	if old.HandoffRequested {
		return false, nil
	}
	updated := old
	updated.HandoffRequested = true
	s.data.Sessions[id] = updated
	if err := s.save(); err != nil {
		s.data.Sessions[id] = old
		return false, err
	}
	return true, nil
}

func (s *Store) Session(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.data.Sessions[id]
	return value, ok
}

func (s *Store) Sessions(owner string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sessions []Session
	for _, session := range s.data.Sessions {
		if session.OwnerID == owner {
			sessions = append(sessions, session)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Ref < sessions[j].Ref })
	return sessions
}

func (s *Store) SessionByRef(ref int) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.data.Sessions {
		if value.Ref == ref {
			return value, true
		}
	}
	return Session{}, false
}

func (s *Store) RememberRoutes(ids []string, route MessageRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.data.Routes
	next := make(map[string]MessageRoute, len(old)+len(ids))
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	for key, value := range old {
		if value.Created >= cutoff {
			next[key] = value
		}
	}
	if len(next)+len(ids) > 20000 {
		return errors.New("reply routing history capacity reached")
	}
	for _, id := range ids {
		if id != "" {
			next[id] = route
		}
	}
	s.data.Routes = next
	if err := s.save(); err != nil {
		s.data.Routes = old
		return err
	}
	return nil
}

func (s *Store) Route(id, chat, owner string) (MessageRoute, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.data.Routes[id]
	return route, ok && route.ChatID == chat && route.OwnerID == owner && route.Created >= time.Now().Add(-7*24*time.Hour).Unix()
}

func (s *Store) OwnerChat() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for chat, binding := range s.data.Bindings {
		if binding.OwnerID == s.data.PairedID {
			return chat, true
		}
	}
	return "", false
}

// NotificationChat reads the atomically replaced state without taking the
// running bridge's writer lock. It only targets the explicitly paired owner.
func NotificationChat(dir, appID, project string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return "", errors.New("cannot read owner notification target")
	}
	var stored Data
	if json.Unmarshal(data, &stored) != nil || stored.Version != 1 || stored.AppID != appID || stored.Project != project || stored.PairedID == "" {
		return "", errors.New("no verified owner notification target")
	}
	var chats []string
	for chat, binding := range stored.Bindings {
		if chat != "" && binding.OwnerID == stored.PairedID {
			chats = append(chats, chat)
		}
	}
	if len(chats) == 0 {
		return "", errors.New("paired owner has no chat binding yet")
	}
	sort.Strings(chats)
	return chats[0], nil
}

func (s *Store) Close() error     { return s.lock.Close() }
func (s *Store) PairedID() string { s.mu.Lock(); defer s.mu.Unlock(); return s.data.PairedID }
func (s *Store) Pair(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.PairedID != "" {
		return errors.New("already paired")
	}
	s.data.PairedID = id
	if err := s.save(); err != nil {
		s.data.PairedID = ""
		return err
	}
	return nil
}
func (s *Store) Binding(chat string) (Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data.Bindings[chat]
	return b, ok
}
func (s *Store) Bind(chat string, b Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.data.Bindings[chat]
	s.data.Bindings[chat] = b
	if err := s.save(); err != nil {
		if ok {
			s.data.Bindings[chat] = old
		} else {
			delete(s.data.Bindings, chat)
		}
		return err
	}
	return nil
}
func (s *Store) Unbind(chat string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.data.Bindings[chat]
	delete(s.data.Bindings, chat)
	if err := s.save(); err != nil {
		if ok {
			s.data.Bindings[chat] = old
		}
		return err
	}
	return nil
}

// Claim is persisted before executing a message. After a crash an uncertain task
// is never replayed automatically. Users can inspect status and send a new instruction.
func (s *Store) Claim(id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Seen[id]; ok {
		return false, nil
	}
	cutoff := now.Add(-7 * 24 * time.Hour).Unix()
	for key, timestamp := range s.data.Seen {
		if timestamp < cutoff {
			delete(s.data.Seen, key)
		}
	}
	if len(s.data.Seen) >= 20000 {
		return false, errors.New("message history capacity reached")
	}
	s.data.Seen[id] = now.Unix()
	if err := s.save(); err != nil {
		delete(s.data.Seen, id)
		return false, err
	}
	return true, nil
}

func (s *Store) save() error { return WritePrivate(filepath.Join(s.dir, "state.json"), s.data) }

func WritePrivate(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".bridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace private state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
