package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/diagnostics"
	"github.com/os7blue/feishu-codex-bridge/internal/feishuui"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type inertRPC struct {
	calls  atomic.Int64
	events chan codex.Message
	done   chan struct{}
}

func (r *inertRPC) Call(context.Context, string, any) (json.RawMessage, error) {
	r.calls.Add(1)
	return nil, errors.New("RPC forbidden in isolated demo")
}
func (r *inertRPC) Respond(json.RawMessage, any) error {
	r.calls.Add(1)
	return errors.New("RPC forbidden in isolated demo")
}
func (r *inertRPC) Reject(json.RawMessage) error {
	r.calls.Add(1)
	return errors.New("RPC forbidden in isolated demo")
}
func (r *inertRPC) Events() <-chan codex.Message { return r.events }
func (r *inertRPC) Done() <-chan struct{}        { return r.done }

func TestIsolatedDemoThroughSDKDeliveryCallbackInboxAndResult(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	project := t.TempDir()
	stateDir := t.TempDir()
	store, err := state.Open(stateDir, "fixture-pipeline-app", project)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("chat-1", state.Binding{OwnerID: "ou_owner", ThreadID: "fixture-thread"}); err != nil {
		t.Fatal(err)
	}
	frames := make(chan map[string]string, 16)
	var sequence atomic.Int64
	client := New("fixture-pipeline-app", "fixture-secret", log)
	client.api = lark.NewClient("fixture-pipeline-app", "fixture-secret", lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
		body := `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`
		if req.URL.Path == "/open-apis/im/v1/messages" {
			var frame map[string]string
			if err := json.NewDecoder(req.Body).Decode(&frame); err != nil {
				return nil, err
			}
			frames <- frame
			body = fmt.Sprintf(`{"code":0,"data":{"message_id":"fixture-%d"}}`, sequence.Add(1))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}}))
	rpc := &inertRPC{events: make(chan codex.Message), done: make(chan struct{})}
	manager := bridge.New(config.Config{AppID: "fixture-pipeline-app", ProjectDir: project, StateDir: stateDir, AllowedOpenIDs: []string{"ou_owner"}}, rpc, client, store, log, feishuui.Renderer{})
	client.SetDiagnostics(manager.Diagnostics())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("bridge run: %v", err)
		}
	}()
	if err := manager.SendClickDemo(); err != nil {
		t.Fatal(err)
	}
	var choice map[string]any
	for range 3 {
		select {
		case frame := <-frames:
			if frame["msg_type"] != "interactive" {
				t.Fatal("demo was not a card")
			}
			var card struct {
				Elements []struct {
					Behaviors []struct {
						Value map[string]any `json:"value"`
					} `json:"behaviors"`
				} `json:"elements"`
			}
			if err := json.Unmarshal([]byte(frame["content"]), &card); err != nil {
				t.Fatal(err)
			}
			for _, element := range card.Elements {
				for _, behavior := range element.Behaviors {
					if behavior.Value["command"] == "approve" {
						choice = behavior.Value
					}
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("demo cards were not delivered")
		}
	}
	if choice == nil || rpc.calls.Load() != 0 {
		t.Fatal("missing approval choice or unexpected RPC")
	}
	handler := client.eventHandler(manager.Submit, manager.HandleCard)
	makeEvent := func(tag, name string, value map[string]any, text string) []byte {
		var event callback.CardActionTriggerEvent
		if err := json.Unmarshal(cardEvent(), &event); err != nil {
			t.Fatal(err)
		}
		event.EventV2Base.Header.AppID = "fixture-pipeline-app"
		event.Event.Action.Tag, event.Event.Action.Name = tag, name
		event.Event.Action.Value = value
		event.Event.Action.FormValue = map[string]interface{}{"command_text": text}
		data, _ := json.Marshal(event)
		return data
	}
	opened, err := handler.Do(ctx, makeEvent("interactive_container", "", choice, ""))
	if err != nil {
		t.Fatal(err)
	}
	draft := opened.(*callback.CardActionTriggerResponse)
	var card struct {
		Elements []struct {
			Tag       string `json:"tag"`
			Behaviors []struct {
				Value map[string]any `json:"value"`
			} `json:"behaviors"`
			Elements []struct {
				Default string         `json:"default_value"`
				Value   map[string]any `json:"value"`
			} `json:"elements"`
		} `json:"elements"`
	}
	if draft.Card == nil {
		t.Fatal("callback did not return an editable card")
	}
	data, _ := json.Marshal(draft.Card.Data)
	if json.Unmarshal(data, &card) != nil || len(card.Elements) < 2 {
		t.Fatal("invalid draft response")
	}
	readForm := func() (string, map[string]any) {
		t.Helper()
		for _, element := range card.Elements {
			if element.Tag == "form" && len(element.Elements) == 2 {
				return element.Elements[0].Default, element.Elements[1].Value
			}
		}
		t.Fatal("editable form missing")
		return "", nil
	}
	prefill, send := readForm()
	if !strings.HasPrefix(prefill, "/approve ") || rpc.calls.Load() != 0 {
		t.Fatal("click executed work or lost prefill")
	}
	for _, command := range []string{"deny", "approve"} {
		choice = nil
		for _, element := range card.Elements {
			for _, behavior := range element.Behaviors {
				if behavior.Value["command"] == command {
					choice = behavior.Value
				}
			}
		}
		if choice == nil {
			t.Fatal("selected draft lost alternative highlighted commands")
		}
		opened, err = handler.Do(ctx, makeEvent("interactive_container", "", choice, ""))
		if err != nil {
			t.Fatal(err)
		}
		draft = opened.(*callback.CardActionTriggerResponse)
		if draft.Card == nil || draft.Toast.Type == "error" {
			t.Fatal("direct reselection failed")
		}
		data, _ = json.Marshal(draft.Card.Data)
		if json.Unmarshal(data, &card) != nil {
			t.Fatal("invalid reselected draft")
		}
		prefill, send = readForm()
		if !strings.HasPrefix(prefill, "/"+command+" ") || rpc.calls.Load() != 0 {
			t.Fatal("reselection lost command or executed work")
		}
	}
	submitted, err := handler.Do(ctx, makeEvent("button", "send_command", send, prefill))
	if err != nil {
		t.Fatal(err)
	}
	response := submitted.(*callback.CardActionTriggerResponse)
	if response.Card == nil {
		t.Fatal("submission callback returned no receipt")
	}
	receipt, _ := json.Marshal(response.Card)
	if response.Toast.Type == "error" || !strings.Contains(string(receipt), prefill) || !strings.Contains(string(receipt), "隔离审批演示") {
		t.Fatal("SDK update lost context or submitted content")
	}
	select {
	case frame := <-frames:
		if !strings.Contains(frame["content"], "隔离演示回复已接收") {
			t.Fatal("demo did not report its local result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("demo result not delivered")
	}
	if rpc.calls.Load() != 0 {
		t.Fatal("isolated approval reached real RPC")
	}
	s := manager.Diagnostics().Snapshot()
	if s.Counts["demo_delivery"]["delivered"] != 3 || s.Counts["demo_open"]["opened"] != 3 || s.Counts["demo_send"]["queued"] != 1 || s.Counts["demo_result"]["completed"] != 1 {
		t.Fatal("pipeline evidence incomplete")
	}
}

func TestMessageAPIEvidenceRetainsOnlyNumericFailureDetails(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	var attempts int
	client := New("fixture-numeric-app", "fixture-secret", log)
	d := &diagnostics.Collector{}
	client.SetDiagnostics(d)
	client.api = lark.NewClient("fixture-numeric-app", "fixture-secret", lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
		body := `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`
		status := 200
		if req.URL.Path == "/open-apis/im/v1/messages" {
			attempts++
			status = 400
			body = `{"code":230099,"msg":"Failed to create card content, ext=ErrCode: 200621; ErrPath: ROOT -> elements -> interactive_container -> padding; ErrMsg: fixture-payload-secret"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}}))
	ids, err := client.SendCardRouted(context.Background(), "fixture-chat", json.RawMessage(`{"elements":[]}`), "fixture-delivery")
	var failure *DeliveryError
	if !errors.As(err, &failure) || !failure.CardContentRejected() || failure.Kind != "api" || failure.Code != 230099 || failure.HTTPStatus != 400 || failure.CardSubcode != 200621 || strings.Join(failure.CardFields, ",") != "interactive_container,padding" {
		t.Fatal("card rejection lost its safe typed numeric details")
	}
	if attempts != 1 || len(ids) != 0 {
		t.Fatal("definitive card rejection was retried or reported a partial delivery")
	}
	s := d.Snapshot()
	data, _ := json.Marshal(s)
	serializedError, _ := json.Marshal(failure)
	if strings.Contains(err.Error()+string(serializedError)+logs.String(), "fixture-payload-secret") || strings.Contains(string(data), "fixture") || s.LastAPICode != 230099 || s.LastHTTPStatus != 400 || s.Counts["message_api"]["api"] != 1 {
		t.Fatal("API failure was lost or raw payload retained")
	}
}

func TestCardErrorDetailsRejectUnboundedOrNonNumericSubcodes(t *testing.T) {
	for _, message := range []string{"private-value", "ErrCode: private-value", "ErrCode: 1234567890; private-value", "ErrCode: -200621; private-value"} {
		code, fields := cardErrorDetails(message)
		if code != 0 || len(fields) != 0 {
			t.Fatal("unrecognized API diagnostics were retained")
		}
	}
}

func TestMessageFailuresDoNotMisclassifyCardRejection(t *testing.T) {
	for _, sample := range []struct {
		name, kind, body, category string
		status, attempts           int
		transport                  bool
	}{
		{"permission", "interactive", `{"code":230027,"msg":"payload-secret"}`, "api", 400, 3, false},
		{"DLP", "interactive", `{"code":230028,"msg":"payload-secret"}`, "api", 400, 3, false},
		{"sensitive", "interactive", `{"code":230022,"msg":"payload-secret"}`, "api", 400, 3, false},
		{"unknown", "interactive", `{"code":999999,"msg":"payload-secret"}`, "api", 400, 3, false},
		{"wrong_status", "interactive", `{"code":230099,"msg":"payload-secret"}`, "api", 500, 3, false},
		{"ambiguous_message_ID", "interactive", `{"code":230099,"msg":"payload-secret","data":{"message_id":"fixture-partial"}}`, "api", 400, 3, false},
		{"text", "text", `{"code":230099,"msg":"payload-secret"}`, "api", 400, 3, false},
		{"malformed", "interactive", `{"code":230099,"msg":"payload-secret"`, "transport", 400, 3, false},
		{"transport", "interactive", "", "transport", 0, 3, true},
		{"missing_message_ID", "interactive", `{"code":0,"data":{}}`, "invalid_response", 200, 1, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			t.Parallel() // Injected local HTTP only; no model or network requests.
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			client := New("fixture-message-"+t.Name(), "fixture-secret", log)
			d := &diagnostics.Collector{}
			client.SetDiagnostics(d)
			attempts := 0
			var uuid string
			client.api = lark.NewClient(client.appID, client.secret, lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
					return slashResponse(req, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
				}
				attempts++
				var body map[string]string
				if json.NewDecoder(req.Body).Decode(&body) != nil || body["uuid"] == "" || (uuid != "" && uuid != body["uuid"]) {
					t.Fatal("retry changed the SDK deduplication UUID")
				}
				uuid = body["uuid"]
				if sample.transport {
					return nil, errors.New("payload-secret")
				}
				return slashResponse(req, sample.status, sample.body), nil
			}}))
			_, err := client.sendContent(context.Background(), "fixture-chat", sample.kind, `{}`, "fixture-key")
			var rejected interface{ CardContentRejected() bool }
			if err == nil || (errors.As(err, &rejected) && rejected.CardContentRejected()) || attempts != sample.attempts {
				t.Fatalf("failure incorrectly triggered fallback or changed attempts: %v, %d", err, attempts)
			}
			data, _ := json.Marshal(d.Snapshot())
			if strings.Contains(err.Error()+string(data)+logs.String(), "payload-secret") || d.Snapshot().Counts["message_api"][sample.category] != uint64(sample.attempts) {
				t.Fatalf("classification or redaction failed: %s", data)
			}
		})
	}
}
