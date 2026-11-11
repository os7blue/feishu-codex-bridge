package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
)

func TestSettingsCommandsApplyToEveryNewTurnAndResetLoadedSession(t *testing.T) {
	b, rpc := setup(t)
	ctx := context.Background()
	catalogSession(t, b, "thread-1")
	var turns []map[string]any
	rpc.call = func(method string, p any) (json.RawMessage, error) {
		switch method {
		case "thread/resume":
			return json.RawMessage(`{"thread":{"id":"thread-1"}}`), nil
		case "turn/start":
			turns = append(turns, p.(map[string]any))
			return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
		default:
			return json.RawMessage(`{}`), nil
		}
	}
	for i, text := range []string{"/model global test-model", "/effort global medium", "/effort #1 high", "/model #1"} {
		b.handle(ctx, input(fmt.Sprint("settings-", i), text))
	}
	for _, method := range rpc.calls {
		if method != "model/list" {
			t.Fatal("setting started inference", rpc.calls)
		}
	}
	b.handle(ctx, input("first", "#1 work"))
	b.jobs.Wait()
	if len(turns) != 1 || turns[0]["model"] != "test-model" || turns[0]["effort"] != "high" {
		t.Fatal("settings absent from turn/start", turns)
	}
	b.handle(ctx, input("change-active", "/effort #1 default"))
	if len(turns) != 1 || b.active.modelSettings.Effort != "high" {
		t.Fatal("active turn changed")
	}
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	b.handle(ctx, input("second", "#1 continue"))
	b.jobs.Wait()
	if len(turns) != 2 || turns[1]["effort"] != "medium" {
		t.Fatal("loaded thread retained stale effort", turns)
	}
}

func TestSettingsRequireScopeAndKnownCapabilities(t *testing.T) {
	b, rpc := setup(t)
	ctx := context.Background()
	catalogSession(t, b, "thread-1")
	for i, text := range []string{"/model missing", "/model global unsupported", "/effort #1 ultra", "/model #999 test-model"} {
		b.handle(ctx, input(fmt.Sprint("invalid-", i), text))
	}
	global, overrides := b.store.ModelPreferences()
	if global != (modelsettings.Settings{}) || len(overrides) != 0 {
		t.Fatal("invalid preferences persisted")
	}
	foreign := input("foreign", "/model global test-model")
	foreign.UserID = "unauthorized"
	before := len(rpc.calls)
	b.handle(ctx, foreign)
	if len(rpc.calls) != before {
		t.Fatal("unauthorized catalog query")
	}
	x := input("unknown-reply", "/model test-model")
	x.ReplyTo = "unknown"
	b.handle(ctx, x)
	global, overrides = b.store.ModelPreferences()
	if global != (modelsettings.Settings{}) || len(overrides) != 0 {
		t.Fatal("unknown reply changed defaults")
	}
	rpc.modelCall = func(any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	b.handle(ctx, input("no-catalog", "#1 start"))
	b.jobs.Wait()
	if b.active != nil {
		t.Fatal("invalid catalog left active task")
	}
	for _, method := range rpc.calls {
		if method == "turn/start" {
			t.Fatal("inference with unverified settings")
		}
	}
}

func TestSettingsCardsOnlyDraftAndPreserveExplicitTarget(t *testing.T) {
	b, rpc := setup(t)
	catalogSession(t, b, "thread-1")
	b.handle(context.Background(), input("panel", "/effort #1"))
	var menu string
	for token, grant := range b.drafts {
		if grant.menu && strings.Contains(grant.context, "思考强度") {
			menu = token
		}
	}
	if menu == "" {
		t.Fatal("missing settings panel")
	}
	before := len(rpc.calls)
	token, _ := openDraft(t, b, menu, "effort_1")
	_, overrides := b.store.ModelPreferences()
	if len(overrides) != 0 || len(rpc.calls) != before || len(b.in) != 0 {
		t.Fatal("opening a choice changed settings")
	}
	result := b.HandleAction(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/effort #1 high"})
	if result.Error {
		t.Fatal(result.Message)
	}
	b.handle(context.Background(), <-b.in)
	_, overrides = b.store.ModelPreferences()
	if overrides["thread-1"].Effort != "high" {
		t.Fatal("manual setting not saved")
	}
}
