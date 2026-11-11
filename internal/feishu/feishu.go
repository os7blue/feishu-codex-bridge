package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
	"github.com/os7blue/feishu-codex-bridge/internal/diagnostics"
)

type Client struct {
	api           *lark.Client
	appID, secret string
	log           *slog.Logger
	diagnostics   *diagnostics.Collector
}

// DeliveryError contains only fixed categories and numeric API diagnostics.
// In particular it never retains SDK errors, response bodies or API messages.
type DeliveryError struct {
	Kind             string
	Code, HTTPStatus int
	CardSubcode      int
	CardFields       []string
	cardRejected     bool
}

var cardSubcodePattern = regexp.MustCompile(`\bErrCode:\s*([0-9]{1,9})\b`)

// Retain only a numeric subcode and fixed field names, never the API message,
// values, error path, request identifiers or troubleshooting URL.
func cardErrorDetails(message string) (int, []string) {
	var subcode int
	if match := cardSubcodePattern.FindStringSubmatch(message); len(match) == 2 {
		subcode, _ = strconv.Atoi(match[1])
	}
	var fields []string
	for _, field := range []string{"interactive_container", "padding", "hover_tips", "text_color", "background_style", "border_color", "corner_radius", "form", "action_type", "form_action_type", "default_value", "max_length", "width"} {
		if regexp.MustCompile(`\b` + field + `\b`).MatchString(message) {
			fields = append(fields, field)
		}
	}
	return subcode, fields
}

func (e *DeliveryError) Error() string {
	return fmt.Sprintf("Feishu message delivery failed (kind=%s, code=%d, http_status=%d)", e.Kind, e.Code, e.HTTPStatus)
}

// CardContentRejected is deliberately narrow. A transport failure, incomplete
// response, permission failure or DLP rejection must not trigger a second form.
func (e *DeliveryError) CardContentRejected() bool { return e.cardRejected }

// SetDiagnostics is called before starting any event or delivery workers.
func (c *Client) SetDiagnostics(d *diagnostics.Collector) { c.diagnostics = d }

// sdkLog deliberately discards SDK payloads and connection URLs. The latter may
// contain short-lived credentials; default debug logs must not expose them.
type sdkLog struct{ log *slog.Logger }

func (l sdkLog) Debug(context.Context, ...interface{}) {}
func (l sdkLog) Info(context.Context, ...interface{})  {}
func (l sdkLog) Warn(context.Context, ...interface{}) {
	l.log.Warn("飞书 SDK 警告（原始内容已隐藏）")
}
func (l sdkLog) Error(context.Context, ...interface{}) {
	l.log.Error("飞书 SDK 错误（原始内容已隐藏）")
}

func New(appID, secret string, log *slog.Logger) *Client {
	api := lark.NewClient(appID, secret, lark.WithLogger(sdkLog{log}), lark.WithLogLevel(larkcore.LogLevelWarn), lark.WithReqTimeout(45*time.Second), lark.WithHttpClient(boundedHTTP{client: &http.Client{Timeout: 45 * time.Second}}))
	return &Client{api: api, appID: appID, secret: secret, log: log}
}

func (c *Client) eventHandler(submit func(bridge.Input) error, card func(bridge.CardAction) bridge.CardResult) *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		input, ok := Parse(event, time.Now())
		if !ok {
			return nil
		}
		return submit(input)
	}).OnP2CardActionTrigger(func(_ context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		if event != nil && event.Event != nil && event.Event.Action != nil && event.Event.Action.Value["action"] == "draft_back" {
			outcome := "name_present"
			if event.Event.Action.Name == "" {
				outcome = "name_missing"
			}
			c.diagnostics.Record("back_callback", outcome)
		}
		action, ok := ParseCard(event, c.appID)
		if !ok || card == nil {
			c.diagnostics.Record("callback", "invalid")
			return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: "无法识别草稿操作，请重新发送 /commands。"}}, nil
		}
		result := card(action)
		kind := "info"
		if result.Error {
			kind = "error"
		}
		response := &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: result.Message}}
		if len(result.Card) != 0 {
			response.Card = &callback.Card{Type: "raw", Data: result.Card}
		}
		return response, nil
	})
}

func (c *Client) Start(ctx context.Context, submit func(bridge.Input) error, card func(bridge.CardAction) bridge.CardResult, ready ...func()) error {
	handler := c.eventHandler(submit, card)
	ws := larkws.NewClient(c.appID, c.secret,
		larkws.WithEventHandler(handler),
		larkws.WithLogger(sdkLog{c.log}),
		larkws.WithLogLevel(larkcore.LogLevelWarn),
		larkws.WithOnReady(func() {
			c.log.Info("飞书长连接已建立，可保存后台长连接订阅配置")
			for _, callback := range ready {
				callback()
			}
		}),
		larkws.WithOnReconnected(func() { c.log.Info("飞书长连接已恢复") }),
		larkws.WithOnDisconnected(func() { c.log.Warn("飞书长连接已断开") }),
	)
	if err := ws.Start(ctx); err != nil && ctx.Err() == nil {
		return errors.New("Feishu long connection failed; check credentials, app publication and network")
	}
	return ctx.Err()
}

func ParseCard(event *callback.CardActionTriggerEvent, appID string) (bridge.CardAction, bool) {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil || event.EventV2Base.Header.AppID != appID || event.EventV2Base.Header.EventType != "card.action.trigger" || event.Event == nil {
		return bridge.CardAction{}, false
	}
	e := event.Event
	if e.Operator == nil || e.Context == nil || e.Action == nil || e.Host != "im_message" || e.Context.OpenMessageID == "" || (e.Action.Tag != "button" && e.Action.Tag != "interactive_container") {
		return bridge.CardAction{}, false
	}
	// Only the opaque UI grant is used. Callback update tokens and raw payloads
	// never enter the bridge queue, storage or logs.
	get := func(key string) string { value, _ := e.Action.Value[key].(string); return value }
	text, _ := e.Action.FormValue["command_text"].(string)
	action := bridge.CardAction{ChatID: e.Context.OpenChatID, UserID: e.Operator.OpenID, MessageID: e.Context.OpenMessageID,
		Token: get("token"), Action: get("action"), Command: get("command"), Text: text}
	if action.Action == "draft_send" && (e.Action.Tag != "button" || e.Action.Name != "send_command") {
		return bridge.CardAction{}, false
	}
	// Standalone 1.0 buttons can omit name in the callback. The owner-bound
	// opaque grant and action select the menu; form-only names aren't required.
	if action.Action == "draft_back" && (e.Action.Tag != "button" || (e.Action.Name != "" && e.Action.Name != "back_to_choices")) {
		return bridge.CardAction{}, false
	}
	return action, action.ChatID != "" && action.UserID != "" && len(action.Token) == 32 && (action.Action == "draft_open" || action.Action == "draft_send" || action.Action == "draft_back")
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func Parse(event *larkim.P2MessageReceiveV1, now time.Time) (bridge.Input, bool) {
	if event == nil || event.Event == nil || event.Event.Sender == nil || event.Event.Message == nil {
		return bridge.Input{}, false
	}
	sender, message := event.Event.Sender, event.Event.Message
	kind := value(message.MessageType)
	if sender.SenderId == nil || value(sender.SenderType) != "user" || value(message.ChatType) != "p2p" || (kind != "text" && kind != "file" && kind != "image") {
		return bridge.Input{}, false
	}
	// Reject old replayed commands even after deduplication history has been pruned.
	created, err := strconv.ParseInt(value(message.CreateTime), 10, 64)
	if err != nil {
		return bridge.Input{}, false
	}
	timestamp := time.UnixMilli(created)
	if timestamp.Before(now.Add(-24*time.Hour)) || timestamp.After(now.Add(5*time.Minute)) {
		return bridge.Input{}, false
	}
	content := value(message.Content)
	if len(content) > 256<<10 {
		return bridge.Input{}, false
	}
	var body struct {
		Text     string `json:"text"`
		FileKey  string `json:"file_key"`
		ImageKey string `json:"image_key"`
		FileName string `json:"file_name"`
		FileSize int64  `json:"file_size"`
	}
	if json.Unmarshal([]byte(content), &body) != nil || !utf8.ValidString(body.Text) {
		return bridge.Input{}, false
	}
	input := bridge.Input{ChatID: value(message.ChatId), UserID: value(sender.SenderId.OpenId), MessageID: value(message.MessageId), Text: strings.TrimSpace(body.Text)}
	if kind != "text" {
		key, name := body.FileKey, body.FileName
		if kind == "image" {
			key = body.ImageKey
			name = "image.png"
		}
		if key == "" || len(key) > 512 || strings.ContainsAny(key, "/\\\x00\r\n") || body.FileSize < 0 {
			return bridge.Input{}, false
		}
		input.Attachment = &bridge.Attachment{Kind: kind, Key: key, Name: name, Size: body.FileSize}
	}
	input.ReplyTo = value(message.ParentId)
	input.ReplyRoot = value(message.RootId)
	if input.ReplyTo == "" {
		input.ReplyTo = input.ReplyRoot
	}
	return input, input.ChatID != "" && input.UserID != "" && input.MessageID != "" && (input.Text != "" || input.Attachment != nil)
}

// Split preserves Unicode and stays well under Feishu's JSON content size limit,
// including worst-case JSON escaping. Numbering is added by Send, outside chunks.
func Split(text string, limit int) []string {
	if limit < utf8.UTFMax {
		limit = utf8.UTFMax
	}
	var parts []string
	for len(text) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}

func (c *Client) Send(ctx context.Context, chat, text, key string) error {
	_, err := c.SendRouted(ctx, chat, text, key)
	return err
}

func (c *Client) SendRouted(ctx context.Context, chat, text, key string) ([]string, error) {
	var ids []string
	text = strings.ReplaceAll(text, c.secret, "[REDACTED]")
	parts := Split(text, 2000)
	for i, part := range parts {
		if len(parts) > 1 {
			part = fmt.Sprintf("[%d/%d]\n%s", i+1, len(parts), part)
		}
		content, _ := json.Marshal(map[string]string{"text": part})
		id, err := c.sendContent(ctx, chat, "text", string(content), fmt.Sprintf("%s:%d", key, i))
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (c *Client) SendCardRouted(ctx context.Context, chat string, card json.RawMessage, key string) ([]string, error) {
	if !json.Valid(card) {
		return nil, errors.New("invalid outgoing card")
	}
	id, err := c.sendContent(ctx, chat, "interactive", string(card), key+":0")
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}

func (c *Client) sendContent(ctx context.Context, chat, kind, content, key string) (string, error) {
	hash := sha256.Sum256([]byte(key))
	uuid := hex.EncodeToString(hash[:16])
	req := larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").Body(
		larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chat).MsgType(kind).Content(content).Uuid(uuid).Build(),
	).Build()
	var sendErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt*2) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				c.diagnostics.Record("message_api", "canceled")
				return "", ctx.Err()
			case <-timer.C:
			}
		}
		resp, err := c.api.Im.Message.Create(ctx, req)
		status, code := 0, 0
		if resp != nil {
			code = resp.Code
			if resp.ApiResp != nil {
				status = resp.StatusCode
			}
		}
		c.diagnostics.API(code, status)
		if err == nil && resp != nil && resp.Success() {
			if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
				c.diagnostics.Record("message_api", "invalid_response")
				return "", errors.New("Feishu did not return a message ID")
			}
			c.diagnostics.Record("message_api", "delivered")
			return *resp.Data.MessageId, nil
		}
		if ctx.Err() != nil {
			c.diagnostics.Record("message_api", "canceled")
			return "", ctx.Err()
		}
		category := "invalid_response"
		if err != nil {
			category = "transport"
			var apiError *larkcore.CodeError
			if errors.As(err, &apiError) {
				category = "api"
				code = apiError.Code
				c.diagnostics.API(apiError.Code, status)
			}
		} else if resp != nil {
			category = "api"
		}
		c.diagnostics.Record("message_api", category)
		partial := resp != nil && resp.Data != nil && resp.Data.MessageId != nil && *resp.Data.MessageId != ""
		failure := &DeliveryError{Kind: category, Code: code, HTTPStatus: status,
			cardRejected: err == nil && !partial && category == "api" && code == 230099 && status == 400 && kind == "interactive"}
		if failure.cardRejected {
			failure.CardSubcode, failure.CardFields = cardErrorDetails(resp.Msg)
		}
		sendErr = failure
		if failure.CardContentRejected() {
			return "", failure
		}
	}
	return "", sendErr
}
