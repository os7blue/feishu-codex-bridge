package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/feishuui"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestParseFileAndImagePreservesResourceAndReply(t *testing.T) {
	for _, kind := range []string{"file", "image"} {
		event := receive(t, "user", "p2p", kind, time.Now())
		body := `{"file_key":"file_fixture","file_name":"report.txt","file_size":15}`
		if kind == "image" {
			body = `{"image_key":"img_fixture"}`
		}
		event.Event.Message.Content = &body
		parent := "reply-parent"
		event.Event.Message.ParentId = &parent
		in, ok := Parse(event, time.Now())
		if !ok || in.Attachment == nil || in.Attachment.Kind != kind || in.ReplyTo != parent {
			t.Fatal("attachment/reply rejected")
		}
		bad := `{"file_key":"../../credential","image_key":"../../credential"}`
		event.Event.Message.Content = &bad
		if _, ok := Parse(event, time.Now()); ok {
			t.Fatal("unsafe resource key accepted")
		}
	}
}

func TestFileSDKPipelineReceivesAndSendsWithoutModel(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	project := t.TempDir()
	stateDir := t.TempDir()
	store, err := state.Open(stateDir, "file-fixture-app", project)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	frames := make(chan map[string]string, 16)
	client := New("file-fixture-app", "fixture-secret", log)
	client.api = lark.NewClient(client.appID, client.secret, lark.WithLogger(sdkLog{log}), lark.WithHttpClient(boundedHTTP{client: fixtureHTTP{handle: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			return slashResponse(r, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
		}
		if strings.Contains(r.URL.Path, "/resources/") {
			if r.URL.Path != "/open-apis/im/v1/messages/om_test/resources/file_fixture" || r.URL.Query().Get("type") != "file" {
				t.Error("download did not bind message/resource/type")
			}
			resp := slashResponse(r, 200, "incoming fixture")
			resp.Header.Set("Content-Type", "application/octet-stream")
			return resp, nil
		}
		if r.URL.Path == "/open-apis/im/v1/files" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				return nil, err
			}
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			if r.FormValue("file_type") != "stream" || r.FormValue("file_name") != "outgoing.txt" {
				t.Error("upload metadata incorrect")
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				return nil, err
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if string(data) != "outgoing fixture" {
				t.Error("upload bytes changed")
			}
			return slashResponse(r, 200, `{"code":0,"data":{"file_key":"file_uploaded"}}`), nil
		}
		if r.URL.Path == "/open-apis/im/v1/messages" {
			var frame map[string]string
			if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
				return nil, err
			}
			frames <- frame
			return slashResponse(r, 200, `{"code":0,"data":{"message_id":"om_sent"}}`), nil
		}
		return nil, errors.New("unexpected fixture endpoint")
	}}}))
	rpc := &inertRPC{events: make(chan codex.Message), done: make(chan struct{})}
	manager := bridge.New(config.Config{AppID: client.appID, ProjectDir: project, StateDir: stateDir, AllowedOpenIDs: []string{"ou_owner"}}, rpc, client, store, log, feishuui.Renderer{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	defer func() { cancel(); <-done }()
	event := receive(t, "user", "p2p", "file", time.Now())
	body := `{"file_key":"file_fixture","file_name":"incoming.txt"}`
	event.Event.Message.Content = &body
	in, ok := Parse(event, time.Now())
	if !ok {
		t.Fatal("file event rejected")
	}
	if err := manager.Submit(in); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-frames:
		if frame["msg_type"] != "interactive" || !strings.Contains(frame["content"], "incoming.txt") {
			t.Fatal("file receipt not delivered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing file receipt")
	}
	paths, _ := filepath.Glob(filepath.Join(project, ".cache/feishu-files/*/incoming.txt"))
	if len(paths) != 1 {
		t.Fatal("incoming file not stored")
	}
	data, _ := os.ReadFile(paths[0])
	if string(data) != "incoming fixture" {
		t.Fatal("download bytes changed")
	}
	os.WriteFile(filepath.Join(project, "outgoing.txt"), []byte("outgoing fixture"), 0600)
	if err := manager.Submit(bridge.Input{ChatID: in.ChatID, UserID: in.UserID, MessageID: "send-outgoing", Text: "/file outgoing.txt"}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-frames:
		if frame["msg_type"] != "file" || !strings.Contains(frame["content"], "file_uploaded") || frame["uuid"] == "" {
			t.Fatal("file message not sent with deduplication")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing outgoing file")
	}
	if rpc.calls.Load() != 0 {
		t.Fatal("file transport launched model RPC")
	}
}

func TestFileSDKFailuresHidePayloadsAndBoundUnknownLength(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := New("file-error-app", "fixture-secret", log)
	client.api = lark.NewClient(client.appID, client.secret, lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			return slashResponse(r, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
		}
		return slashResponse(r, 403, `{"code":99991672,"msg":"sensitive fixture payload"}`), nil
	}}))
	_, err := client.DownloadFile(context.Background(), "message", bridge.Attachment{Kind: "file", Key: "key"})
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("download error leaked payload")
	}
	_, err = client.SendFile(context.Background(), "chat", "report", []byte("fixture"), "key")
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("upload error leaked payload")
	}
	transport := boundedHTTP{client: fixtureHTTP{handle: func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(make([]byte, bridge.MaxFileBytes+1))), ContentLength: -1}, nil
	}}}
	req, _ := http.NewRequest("GET", "https://fixture.invalid", nil)
	resp, err := transport.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil || len(data) > bridge.MaxFileBytes {
		t.Fatal("unbounded SDK response")
	}
}
