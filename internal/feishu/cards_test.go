package feishu

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
)

func cardEvent() []byte {
	return []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger","event_id":"event-test","app_id":"app-test"},"event":{"host":"im_message","operator":{"open_id":"ou_owner"},"context":{"open_chat_id":"chat-1","open_message_id":"menu-1"},"action":{"tag":"button","name":"send_command","value":{"action":"draft_send","token":"01234567890123456789012345678901"},"form_value":{"command_text":"/new 编辑名称"}}}}`)
}

func TestSDKDispatchesCardCallbackWithoutSubmittingMessage(t *testing.T) {
	client := New("app-test", "fixture-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	cards, messages := 0, 0
	handler := client.eventHandler(func(bridge.Input) error { messages++; return nil }, func(action bridge.CardAction) bridge.CardResult {
		cards++
		if action.Text != "/new 编辑名称" || action.ChatID != "chat-1" || action.UserID != "ou_owner" || action.MessageID != "menu-1" {
			t.Fatal("callback identity or edited form text lost")
		}
		return bridge.CardResult{Message: "已提交", Card: json.RawMessage(`{"elements":[]}`)}
	})
	response, err := handler.Do(context.Background(), cardEvent())
	if err != nil {
		t.Fatal(err)
	}
	result := response.(*callback.CardActionTriggerResponse)
	if cards != 1 || messages != 0 || result.Card == nil || result.Card.Type != "raw" || result.Toast.Type != "info" {
		t.Fatal("card callback did not pass through the official dispatcher")
	}
}

func TestCardParserRejectsIncompleteOrUnrelatedPayloads(t *testing.T) {
	var valid callback.CardActionTriggerEvent
	if err := json.Unmarshal(cardEvent(), &valid); err != nil {
		t.Fatal(err)
	}
	if _, ok := ParseCard(&valid, "app-test"); !ok {
		t.Fatal("valid form rejected")
	}
	for _, mutate := range []func(*callback.CardActionTriggerEvent){
		func(e *callback.CardActionTriggerEvent) { e.EventV2Base = nil },
		func(e *callback.CardActionTriggerEvent) { e.EventV2Base.Header.AppID = "other-app" },
		func(e *callback.CardActionTriggerEvent) { e.Event.Operator = nil },
		func(e *callback.CardActionTriggerEvent) { e.Event.Context.OpenMessageID = "" },
		func(e *callback.CardActionTriggerEvent) { e.Event.Host = "url_preview" },
		func(e *callback.CardActionTriggerEvent) { e.Event.Action.Tag = "input" },
		func(e *callback.CardActionTriggerEvent) { e.Event.Action.Name = "not-send" },
		func(e *callback.CardActionTriggerEvent) { e.Event.Action.Value["token"] = 123 },
	} {
		var event callback.CardActionTriggerEvent
		_ = json.Unmarshal(cardEvent(), &event)
		mutate(&event)
		if _, ok := ParseCard(&event, "app-test"); ok {
			t.Fatal("invalid card event accepted")
		}
	}
}

func TestCommandTextContainerCallbackOnlyOpensDraft(t *testing.T) {
	var event callback.CardActionTriggerEvent
	if err := json.Unmarshal(cardEvent(), &event); err != nil {
		t.Fatal(err)
	}
	event.Event.Action.Tag = "interactive_container"
	event.Event.Action.Name = ""
	event.Event.Action.Value["action"] = "draft_open"
	event.Event.Action.Value["command"] = "approve"
	action, ok := ParseCard(&event, "app-test")
	if !ok || action.Action != "draft_open" || action.Command != "approve" {
		t.Fatal("clickable command text rejected")
	}
	event.Event.Action.Value["action"] = "draft_send"
	event.Event.Action.Name = "send_command"
	if _, ok := ParseCard(&event, "app-test"); ok {
		t.Fatal("text container submitted without form Send")
	}
}

func TestBackToChoicesRequiresItsOwnButton(t *testing.T) {
	var event callback.CardActionTriggerEvent
	if err := json.Unmarshal(cardEvent(), &event); err != nil {
		t.Fatal(err)
	}
	event.Event.Action.Value["action"] = "draft_back"
	event.Event.Action.Name = "back_to_choices"
	event.Event.Action.FormValue = nil
	if action, ok := ParseCard(&event, "app-test"); !ok || action.Action != "draft_back" {
		t.Fatal("back button without form input was rejected")
	}
	event.Event.Action.Name = ""
	if action, ok := ParseCard(&event, "app-test"); !ok || action.Action != "draft_back" {
		t.Fatal("standalone back button callback without name was rejected")
	}
	event.Event.Action.Name = "send_command"
	if _, ok := ParseCard(&event, "app-test"); ok {
		t.Fatal("submit button was accepted as a back action")
	}
	event.Event.Action.Name = "back_to_choices"
	event.Event.Action.Tag = "interactive_container"
	if _, ok := ParseCard(&event, "app-test"); ok {
		t.Fatal("unrelated container was accepted as a back button")
	}
}

type fixtureHTTP struct {
	handle func(*http.Request) (*http.Response, error)
}

func (f fixtureHTTP) Do(r *http.Request) (*http.Response, error) { return f.handle(r) }

func TestInteractiveDeliveryUsesOfficialMessageAPI(t *testing.T) {
	var delivered, uuid string
	transport := fixtureHTTP{handle: func(r *http.Request) (*http.Response, error) {
		response := func(body string) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			return response(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`)
		}
		if r.URL.Path != "/open-apis/im/v1/messages" || r.URL.Query().Get("receive_id_type") != "chat_id" {
			t.Error("unexpected API endpoint")
		}
		var body struct {
			MsgType, Content, UUID, ReceiveID string
		}
		var raw map[string]string
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		body.MsgType, body.Content, body.UUID, body.ReceiveID = raw["msg_type"], raw["content"], raw["uuid"], raw["receive_id"]
		if body.MsgType != "interactive" || body.ReceiveID != "chat-1" || !json.Valid([]byte(body.Content)) {
			t.Error("invalid interactive payload")
		}
		delivered, uuid = body.Content, body.UUID
		return response(`{"code":0,"data":{"message_id":"card-message"}}`)
	}}
	client := New("fixture-app-card", "fixture-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.api = lark.NewClient("fixture-app-card", "fixture-secret", lark.WithHttpClient(transport))
	ids, err := client.SendCardRouted(context.Background(), "chat-1", json.RawMessage(`{"elements":[]}`), "delivery-1")
	if err != nil || len(ids) != 1 || ids[0] != "card-message" || delivered == "" || len(uuid) != 32 {
		t.Fatalf("interactive delivery failed: %v", err)
	}
}
