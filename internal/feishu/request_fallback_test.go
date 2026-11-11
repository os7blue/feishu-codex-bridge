package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/feishuui"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

// This RPC never starts an App Server or calls a model. It only establishes a
// synthetic active turn so that real Bridge and SDK delivery code can run.
type fallbackRPC struct {
	events    chan codex.Message
	done      chan struct{}
	started   chan struct{}
	responses chan codex.Message
	calls     atomic.Int64
}

func (r *fallbackRPC) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	r.calls.Add(1)
	switch method {
	case "model/list":
		return json.RawMessage(`{"data":[{"id":"fixture-model","model":"fixture-model","isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}]}`), nil
	case "thread/start":
		return json.RawMessage(`{"thread":{"id":"fixture-thread"}}`), nil
	case "turn/start":
		close(r.started)
		return json.RawMessage(`{"turn":{"id":"fixture-turn"}}`), nil
	default:
		return nil, errors.New("unexpected synthetic RPC")
	}
}
func (r *fallbackRPC) Respond(id json.RawMessage, result any) error {
	body, err := json.Marshal(result)
	if err == nil {
		r.responses <- codex.Message{ID: id, Params: body}
	}
	return err
}
func (r *fallbackRPC) Reject(id json.RawMessage) error { return r.Respond(id, "rejected") }
func (r *fallbackRPC) Events() <-chan codex.Message    { return r.events }
func (r *fallbackRPC) Done() <-chan struct{}           { return r.done }

func TestRealRequestFallbackThroughSDKAndManualReply(t *testing.T) {
	for _, sample := range []struct{ name, method, fields, command, preview, expected string }{
		{"approval", "item/commandExecution/requestApproval", `"command":"fixture-command","kind":"humanCommand"`, "/approve", "fixture-command", `{"decision":"accept"}`},
		{"answer", "item/tool/requestUserInput", `"questions":[{"id":"q","question":"fixture-question"}]`, "/answer", "fixture-question", `{"answers":{"q":{"answers":["fixture-answer"]}}}`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			project, stateDir := t.TempDir(), t.TempDir()
			appID := "fixture-sdk-fallback-" + sample.name
			store, err := state.Open(stateDir, appID, project)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			frames := make(chan map[string]string, 32)
			client := New(appID, "fixture-secret", log)
			client.api = lark.NewClient(appID, "fixture-secret", lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/open-apis/auth/v3/tenant_access_token/internal":
					return slashResponse(req, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
				case "/open-apis/im/v1/messages":
					var frame map[string]string
					if err := json.NewDecoder(req.Body).Decode(&frame); err != nil {
						return nil, err
					}
					frames <- frame
					if frame["msg_type"] == "interactive" {
						return slashResponse(req, 400, `{"code":230099,"msg":"fixture-response-secret"}`), nil
					}
					// Unique message IDs let the test verify reply routing as well.
					body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]string{"message_id": frame["uuid"]}})
					return slashResponse(req, 200, string(body)), nil
				default:
					if req.URL.Path == "/open-apis/im/v1/threads/omt_fixture/forward" {
						return slashResponse(req, 200, `{"code":0,"data":{"message_id":"entry-`+req.URL.Query().Get("uuid")+`"}}`), nil
					}
					if strings.HasPrefix(req.URL.Path, "/open-apis/im/v1/messages/") && strings.HasSuffix(req.URL.Path, "/reply") {
						var frame struct {
							InThread bool   `json:"reply_in_thread"`
							UUID     string `json:"uuid"`
						}
						if err := json.NewDecoder(req.Body).Decode(&frame); err != nil || !frame.InThread {
							return nil, errors.New("invalid synthetic topic reply")
						}
						body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]string{"message_id": "topic-" + frame.UUID, "thread_id": "omt_fixture", "chat_id": "chat-1"}})
						return slashResponse(req, 200, string(body)), nil
					}
					return nil, errors.New("unexpected SDK endpoint")
				}
			}}))
			rpc := &fallbackRPC{events: make(chan codex.Message, 8), done: make(chan struct{}), started: make(chan struct{}), responses: make(chan codex.Message, 8)}
			manager := bridge.New(config.Config{AppID: appID, AppSecret: "fixture-secret", ProjectDir: project, StateDir: stateDir, AllowedOpenIDs: []string{"ou_owner"}, TurnTimeoutSeconds: 3600}, rpc, client, store, log, feishuui.Renderer{})
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
			submit := func(id, text string) {
				t.Helper()
				if err := manager.Submit(bridge.Input{ChatID: "chat-1", UserID: "ou_owner", MessageID: id, Text: text}); err != nil {
					t.Fatal(err)
				}
			}
			submit("start", "synthetic task")
			select {
			case <-rpc.started:
			case <-time.After(3 * time.Second):
				t.Fatal("synthetic turn did not start")
			}
			rpc.events <- codex.Message{ID: json.RawMessage(`77`), Method: sample.method, Params: json.RawMessage(`{"threadId":"fixture-thread","turnId":"fixture-turn","reason":"fixture-secret",` + sample.fields + `}`)}
			var cardUUID string
			cards := 0
			waitText := func(marker string) (string, string) {
				t.Helper()
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				for {
					select {
					case frame := <-frames:
						if frame["receive_id"] != "chat-1" || strings.Contains(frame["content"], "fixture-secret") {
							t.Fatal("wrong recipient or secret leaked")
						}
						if frame["msg_type"] == "interactive" {
							cards++
							cardUUID = frame["uuid"]
							continue
						}
						var content struct {
							Text string `json:"text"`
						}
						if json.Unmarshal([]byte(frame["content"]), &content) != nil {
							t.Fatal("invalid text payload")
						}
						if strings.Contains(content.Text, marker) {
							return content.Text, frame["uuid"]
						}
					case <-timer.C:
						t.Fatalf("missing SDK text delivery: %s", marker)
					}
				}
			}
			text, textUUID := waitText("文字审批/回答备用入口")
			if cards != 1 || cardUUID == "" || textUUID == "" || cardUUID == textUUID || !strings.Contains(text, sample.preview) || !strings.Contains(text, "十分钟内有效") || !strings.Contains(text, "尚未允许任何操作") {
				t.Fatal("fallback lost original expiry, explicit manual confirmation or independent UUID")
			}
			_, suffix, found := strings.Cut(text, sample.command+" ")
			if !found || len(strings.Fields(suffix)) == 0 {
				t.Fatal("fallback lost manual command")
			}
			token := strings.Fields(suffix)[0]
			select {
			case <-rpc.responses:
				t.Fatal("delivery automatically decided the request")
			default:
			}
			reply := sample.command + " " + token
			if sample.command == "/answer" {
				reply += " fixture-answer"
			}
			submit("manual", reply)
			select {
			case result := <-rpc.responses:
				if string(result.ID) != "77" || string(result.Params) != sample.expected {
					t.Fatalf("wrong one-shot response: %s", result.Params)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("manual response did not reach RPC")
			}
			waitText("回复已回传 Codex") // Also waits until reply routes were persisted.
			if route, ok := store.Route(textUUID, "chat-1", "ou_owner"); !ok || route.ThreadID != "fixture-thread" {
				t.Fatal("fallback reply route lost")
			}
			submit("replay", reply)
			waitText("请求不存在、已过期")
			select {
			case <-rpc.responses:
				t.Fatal("manual response replayed")
			default:
			}
			snapshot := manager.Diagnostics().Snapshot()
			// The rejected replay now has a recovery shortcut card of its own.
			// Its text fallback must not count as another approval delivery.
			if cards != 2 || rpc.calls.Load() != 3 || snapshot.Counts["message_api"]["api"] != 2 || snapshot.Counts["request_delivery"]["failed"] != 1 || snapshot.Counts["request_delivery"]["delivered"] != 0 || snapshot.Counts["request_text_fallback"]["delivered"] != 1 {
				t.Fatal("incorrect delivery evidence or unexpected RPC calls")
			}
		})
	}
}
