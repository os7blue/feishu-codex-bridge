// Package modelsettings is a frontend-independent application service. It knows
// neither Feishu commands/cards nor App Server JSON. Adapters authenticate the
// caller; Repository must additionally enforce session ownership.
package modelsettings

import (
	"context"
	"errors"
	"strings"
	"sync"
)

type Settings struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}
type Model struct {
	ID, Model, DefaultEffort string
	Efforts                  []string
	Default                  bool
}
type Effective struct{ Model, Effort, ModelSource, EffortSource string }
type Scope struct{ ThreadID, OwnerID string }
type Repository interface {
	ModelPreferences() (Settings, map[string]Settings)
	SetModelSettings(thread, owner string, value Settings) error
	SetModelDefaults(owner string, value Settings, overrides map[string]Settings) error
	CheckModelScope(thread, owner string) error
}
type Catalog interface {
	Models(context.Context) ([]Model, error)
}
type CatalogFunc func(context.Context) ([]Model, error)

func (f CatalogFunc) Models(ctx context.Context) ([]Model, error) { return f(ctx) }

type View struct {
	Models     []Model
	Effective  Effective
	Configured Settings
	// A reset may succeed while another stale field still needs correction.
	Problem error
}
type Service struct {
	mu           sync.Mutex
	repo         Repository
	catalog      Catalog
	startupModel func() string
}

func New(repo Repository, catalog Catalog, startupModel func() string) *Service {
	return &Service{repo: repo, catalog: catalog, startupModel: startupModel}
}
func (s *Service) Models(ctx context.Context) ([]Model, error) { return s.catalog.Models(ctx) }

func ValidToken(value string) bool {
	if len(value) == 0 || len(value) > 200 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:/-", c)) {
			return false
		}
	}
	return true
}
func (m Model) Supports(effort string) bool {
	for _, e := range m.Efforts {
		if e == effort && e != "" {
			return true
		}
	}
	return false
}
func Find(models []Model, name string) (Model, bool) {
	for _, m := range models {
		if m.Model == name || m.ID == name {
			return m, true
		}
	}
	return Model{}, false
}
func Resolve(models []Model, startup string, global, session Settings) (Effective, error) {
	e := Effective{Model: session.Model, ModelSource: "会话覆盖", Effort: session.Effort, EffortSource: "会话覆盖"}
	if e.Model == "" {
		e.Model, e.ModelSource = global.Model, "桥接全局默认"
	}
	if e.Model == "" {
		e.Model, e.ModelSource = startup, "桥接启动配置"
	}
	if e.Model == "" {
		for _, m := range models {
			if m.Default {
				if e.Model != "" {
					return e, errors.New("模型目录没有唯一默认模型。")
				}
				e.Model, e.ModelSource = m.Model, "账号模型目录默认"
			}
		}
	}
	m, ok := Find(models, e.Model)
	if !ok {
		return e, errors.New("有效模型不在当前可用目录中，请重新选择。")
	}
	e.Model = m.Model
	if e.Effort == "" {
		e.Effort, e.EffortSource = global.Effort, "桥接全局默认"
	}
	if e.Effort == "" {
		e.Effort, e.EffortSource = m.DefaultEffort, "所选模型默认"
	}
	if !m.Supports(e.Effort) {
		return e, errors.New("所选模型不支持当前思考强度；请先恢复继承，或选择兼容值。")
	}
	return e, nil
}
func view(models []Model, startup string, global Settings, overrides map[string]Settings, scope Scope) View {
	prefs := global
	if scope.ThreadID != "" {
		prefs = overrides[scope.ThreadID]
	}
	effective, err := Resolve(models, startup, global, overrides[scope.ThreadID])
	return View{Models: models, Configured: prefs, Effective: effective, Problem: err}
}
func (s *Service) Inspect(ctx context.Context, scope Scope) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.CheckModelScope(scope.ThreadID, scope.OwnerID); err != nil {
		return View{}, err
	}
	models, err := s.catalog.Models(ctx)
	if err != nil {
		return View{}, err
	}
	global, overrides := s.repo.ModelPreferences()
	return view(models, s.startupModel(), global, overrides, scope), nil
}
func (s *Service) Snapshot(ctx context.Context, scope Scope) (Effective, error) {
	v, err := s.Inspect(ctx, scope)
	if err != nil {
		return Effective{}, err
	}
	return v.Effective, v.Problem
}

// Session customization stores a complete pair. Either session reset command
// restores inheritance for the whole pair. Global writes first freeze legacy
// partial overrides against the OLD defaults, in the same durable transaction.
func (s *Service) Change(ctx context.Context, scope Scope, field, value string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.CheckModelScope(scope.ThreadID, scope.OwnerID); err != nil {
		return View{}, err
	}
	if field != "model" && field != "effort" || value != "" && !ValidToken(value) {
		return View{}, errors.New("模型设置参数无效。")
	}
	models, err := s.catalog.Models(ctx)
	if err != nil {
		return View{}, err
	}
	global, overrides := s.repo.ModelPreferences()
	prefs := global
	if scope.ThreadID != "" {
		prefs = overrides[scope.ThreadID]
	}
	if scope.ThreadID != "" && value == "" {
		prefs = Settings{}
	} else {
		if field == "model" {
			if value != "" {
				m, ok := Find(models, value)
				if !ok {
					return View{}, errors.New("模型不在当前可用目录中，未修改设置。")
				}
				value = m.Model
			}
			prefs.Model = value
		} else {
			prefs.Effort = value
		}
	}
	candidateGlobal, candidateSession := global, prefs
	if scope.ThreadID == "" {
		candidateGlobal, candidateSession = prefs, Settings{}
	}
	resolved, err := Resolve(models, s.startupModel(), candidateGlobal, candidateSession)
	if err != nil && value != "" {
		return View{}, err
	}
	if scope.ThreadID != "" && value != "" {
		prefs = Settings{Model: resolved.Model, Effort: resolved.Effort}
	}
	if scope.ThreadID == "" {
		for thread, override := range overrides {
			if override == (Settings{}) || override.Model != "" && override.Effort != "" {
				continue
			}
			previous, err := Resolve(models, s.startupModel(), global, override)
			if err != nil {
				return View{}, errors.New("已有会话的旧自定义配置无法固定，未修改全局；请先修正该会话设置或恢复继承。")
			}
			overrides[thread] = Settings{Model: previous.Model, Effort: previous.Effort}
		}
		if err := s.repo.SetModelDefaults(scope.OwnerID, prefs, overrides); err != nil {
			return View{}, errors.New("设置保存失败，原设置保持。")
		}
		global = prefs
	} else {
		if err := s.repo.SetModelSettings(scope.ThreadID, scope.OwnerID, prefs); err != nil {
			return View{}, errors.New("设置保存失败，原设置保持。")
		}
		overrides[scope.ThreadID] = prefs
	}
	return view(models, s.startupModel(), global, overrides, scope), nil
}
