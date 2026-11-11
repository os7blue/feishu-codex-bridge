package modelsettings

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type memoryRepo struct {
	global    Settings
	overrides map[string]Settings
	fail      bool
}

func (r *memoryRepo) ModelPreferences() (Settings, map[string]Settings) {
	m := map[string]Settings{}
	for k, v := range r.overrides {
		m[k] = v
	}
	return r.global, m
}
func (r *memoryRepo) CheckModelScope(thread, owner string) error {
	if owner != "owner" || thread != "" && thread != "session" {
		return errors.New("forbidden")
	}
	return nil
}
func (r *memoryRepo) SetModelSettings(thread, owner string, v Settings) error {
	if r.fail {
		return errors.New("disk error")
	}
	if thread == "" {
		r.global = v
	} else {
		r.overrides[thread] = v
	}
	return nil
}
func (r *memoryRepo) SetModelDefaults(owner string, v Settings, overrides map[string]Settings) error {
	if r.fail {
		return errors.New("disk error")
	}
	r.global, r.overrides = v, overrides
	return nil
}
func fixture() (*Service, *memoryRepo) {
	r := &memoryRepo{overrides: map[string]Settings{}}
	models := []Model{{ID: "alias-a", Model: "model-a", Default: true, DefaultEffort: "medium", Efforts: []string{"medium", "high"}}, {ID: "model-b", Model: "model-b", DefaultEffort: "low", Efforts: []string{"low", "medium"}}}
	s := New(r, CatalogFunc(func(context.Context) ([]Model, error) { return models, nil }), func() string { return "" })
	return s, r
}
func TestInheritanceAndResetsWithoutFrontend(t *testing.T) {
	s, _ := fixture()
	ctx := context.Background()
	global := Scope{OwnerID: "owner"}
	session := Scope{ThreadID: "session", OwnerID: "owner"}
	if _, e := s.Change(ctx, global, "model", "alias-a"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Change(ctx, global, "effort", "medium"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Change(ctx, session, "model", "model-b"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Change(ctx, session, "effort", "low"); e != nil {
		t.Fatal(e)
	}
	v, e := s.Snapshot(ctx, session)
	if e != nil || v.Model != "model-b" || v.Effort != "low" || v.ModelSource != "会话覆盖" {
		t.Fatalf("bad session snapshot: %+v %v", v, e)
	}
	if _, e = s.Change(ctx, session, "effort", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Change(ctx, session, "model", ""); e != nil {
		t.Fatal(e)
	}
	v, e = s.Snapshot(ctx, session)
	if e != nil || v.Model != "model-a" || v.Effort != "medium" || v.ModelSource != "桥接全局默认" {
		t.Fatal(v, e)
	}
	if _, e = s.Change(ctx, global, "model", ""); e != nil {
		t.Fatal(e)
	}
	v, e = s.Snapshot(ctx, session)
	if e != nil || v.ModelSource != "账号模型目录默认" {
		t.Fatal(v, e)
	}
}
func TestInvalidChangesAndScopeDoNotPersist(t *testing.T) {
	s, r := fixture()
	ctx := context.Background()
	scope := Scope{OwnerID: "owner"}
	for _, test := range []struct{ field, value string }{{"model", "unavailable"}, {"effort", "ultra"}, {"mode", "plan"}} {
		if _, e := s.Change(ctx, scope, test.field, test.value); e == nil {
			t.Fatal("invalid change accepted")
		}
		if r.global != (Settings{}) {
			t.Fatal("invalid change saved")
		}
	}
	if _, e := s.Inspect(ctx, Scope{ThreadID: "session", OwnerID: "stranger"}); e == nil {
		t.Fatal("ownership bypass")
	}
	if _, e := s.Change(ctx, Scope{ThreadID: "session", OwnerID: "owner"}, "model", "model-b"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Change(ctx, scope, "effort", "high"); e != nil {
		t.Fatal("independent session incorrectly blocked global effort", e)
	}
	before := r.global
	r.fail = true
	if _, e := s.Change(ctx, scope, "effort", "medium"); e == nil || r.global != before {
		t.Fatal("failed persistence claimed success")
	}
}
func TestConcurrentChangesAndSnapshotsAreSerialized(t *testing.T) {
	s, _ := fixture()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope := Scope{OwnerID: "owner"}
			if _, e := s.Change(context.Background(), scope, "effort", "medium"); e != nil {
				t.Error(e)
			}
			if _, e := s.Snapshot(context.Background(), scope); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
}

func TestCustomizationPinsBothFieldsAndEitherResetRestoresInheritance(t *testing.T) {
	for _, field := range []string{"model", "effort"} {
		t.Run(field, func(t *testing.T) {
			s, r := fixture()
			ctx := context.Background()
			global := Scope{OwnerID: "owner"}
			session := Scope{ThreadID: "session", OwnerID: "owner"}
			value := "model-a"
			if field == "effort" {
				value = "medium"
			}
			if _, err := s.Change(ctx, session, field, value); err != nil {
				t.Fatal(err)
			}
			if r.overrides["session"] != (Settings{Model: "model-a", Effort: "medium"}) {
				t.Fatal(r.overrides)
			}
			if _, err := s.Change(ctx, global, "model", "model-b"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Change(ctx, global, "effort", "low"); err != nil {
				t.Fatal(err)
			}
			actual, err := s.Snapshot(ctx, session)
			if err != nil || actual.Model != "model-a" || actual.Effort != "medium" {
				t.Fatal(actual, err)
			}
			if _, err := s.Change(ctx, session, field, ""); err != nil {
				t.Fatal(err)
			}
			actual, err = s.Snapshot(ctx, session)
			if err != nil || actual.Model != "model-b" || actual.Effort != "low" || r.overrides["session"] != (Settings{}) {
				t.Fatal(actual, err)
			}
		})
	}
}

func TestGlobalWriteFreezesLegacyAgainstOldDefaultsAtomically(t *testing.T) {
	for _, fail := range []bool{false, true} {
		s, r := fixture()
		r.global = Settings{Model: "model-a", Effort: "medium"}
		r.overrides = map[string]Settings{"model-only": {Model: "model-b"}, "effort-only": {Effort: "high"}, "inherited": {}}
		r.fail = fail
		_, err := s.Change(context.Background(), Scope{OwnerID: "owner"}, "model", "model-b")
		if fail {
			if err == nil || r.global.Model != "model-a" || r.overrides["model-only"].Effort != "" || r.overrides["effort-only"].Model != "" {
				t.Fatal("partial save on failure", r, err)
			}
			continue
		}
		if err != nil || r.global.Model != "model-b" || r.overrides["model-only"] != (Settings{Model: "model-b", Effort: "medium"}) || r.overrides["effort-only"] != (Settings{Model: "model-a", Effort: "high"}) || r.overrides["inherited"] != (Settings{}) {
			t.Fatal(r, err)
		}
		if _, err := s.Change(context.Background(), Scope{OwnerID: "owner"}, "effort", "low"); err != nil {
			t.Fatal(err)
		}
	}
}
