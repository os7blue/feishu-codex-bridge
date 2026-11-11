package codex

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStartIsolatesHomeAndRemovesFeishuCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test subprocess uses a POSIX shell")
	}
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "explicit"}[override], func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", filepath.Join(dir, "inherited"))
			t.Setenv("FEISHU_APP_ID", "test-app")
			t.Setenv("FEISHU_APP_SECRET", "test-secret")
			home, expected := "", os.Getenv("CODEX_HOME")
			if override {
				home = filepath.Join(dir, "explicit")
				expected = home
			}
			t.Setenv("FEISHU_BRIDGE_EXPECTED_HOME", expected)
			initialization := filepath.Join(dir, "initialize.json")
			t.Setenv("FEISHU_BRIDGE_TEST_INITIALIZATION", initialization)
			script := "#!/bin/sh\n" +
				"test -z \"$FEISHU_APP_ID\" || { echo fixture_app_id_present >&2; exit 1; }\n" +
				"test -z \"$FEISHU_APP_SECRET\" || { echo fixture_app_secret_present >&2; exit 1; }\n" +
				"test \"$CODEX_HOME\" = \"$FEISHU_BRIDGE_EXPECTED_HOME\" || { echo fixture_home_mismatch >&2; exit 1; }\n" +
				"IFS= read -r request || exit 1\n" +
				"printf '%s\\n' \"$request\" > \"$FEISHU_BRIDGE_TEST_INITIALIZATION\" || exit 1\n" +
				"printf '%s\\n' '{\"id\":1,\"result\":{}}'\n" +
				// Stay alive using shell builtins; an extra exec can exit early
				// and race initialization when subprocess execution is restricted.
				"while IFS= read -r request; do :; done\n"
			binary := filepath.Join(dir, "codex-test")
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Start(ctx, binary, dir, home, io.Discard)
			if err != nil {
				t.Fatalf("subprocess environment check or initialization failed: %v", err)
			}
			client.Close()
			data, err := os.ReadFile(initialization)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Method string `json:"method"`
				Params struct {
					Capabilities struct {
						ExperimentalAPI       bool `json:"experimentalApi"`
						RequiredHumanApproval bool `json:"requiredHumanApproval"`
					} `json:"capabilities"`
				} `json:"params"`
			}
			if json.Unmarshal(data, &request) != nil || request.Method != "initialize" || !request.Params.Capabilities.ExperimentalAPI || !request.Params.Capabilities.RequiredHumanApproval {
				t.Fatal("client cannot receive required human approval checkpoints")
			}
		})
	}
}

func testClient(t *testing.T) (*Client, *json.Decoder, *json.Encoder) {
	t.Helper()
	serverOut, clientIn := io.Pipe()
	clientOut, serverIn := io.Pipe()
	c := newClient(serverOut, serverIn, func() { _ = clientIn.Close(); _ = clientOut.Close() })
	t.Cleanup(c.Close)
	return c, json.NewDecoder(clientOut), json.NewEncoder(clientIn)
}

func TestResponseCorrelationAndEvents(t *testing.T) {
	c, read, write := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		name string
		data json.RawMessage
		err  error
	}
	results := make(chan result, 2)
	go func() { data, err := c.Call(ctx, "first", map[string]string{}); results <- result{"first", data, err} }()
	var first Message
	if err := read.Decode(&first); err != nil {
		t.Fatal(err)
	}
	go func() {
		data, err := c.Call(ctx, "second", map[string]string{})
		results <- result{"second", data, err}
	}()
	var second Message
	if err := read.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if err := write.Encode(map[string]any{"method": "turn/started", "params": map[string]string{"threadId": "thread"}}); err != nil {
		t.Fatal(err)
	}
	if err := write.Encode(map[string]any{"id": second.ID, "result": map[string]string{"name": "second"}}); err != nil {
		t.Fatal(err)
	}
	if err := write.Encode(map[string]any{"id": first.ID, "result": map[string]string{"name": "first"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatal(r.err)
			}
			var value struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(r.data, &value) != nil || value.Name != r.name {
				t.Fatal("RPC responses were delivered to the wrong caller")
			}
		case <-ctx.Done():
			t.Fatal("RPC responses lost")
		}
	}
	select {
	case event := <-c.Events():
		if event.Method != "turn/started" {
			t.Fatal("wrong notification")
		}
	case <-ctx.Done():
		t.Fatal("event lost")
	}
}

func TestServerRequestsAndApprovalRepliesPreserveStringID(t *testing.T) {
	c, read, write := testClient(t)
	if err := write.Encode(map[string]any{"id": "approval-1", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "thread"}}); err != nil {
		t.Fatal(err)
	}
	var request Message
	select {
	case request = <-c.Events():
	case <-time.After(time.Second):
		t.Fatal("server request lost")
	}
	done := make(chan error, 1)
	go func() { done <- c.Respond(request.ID, map[string]string{"decision": "decline"}) }()
	var response Message
	if err := read.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != `"approval-1"` || string(response.Result) != `{"decision":"decline"}` {
		t.Fatal("request id or decision corrupted")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectUnblocksPendingCall(t *testing.T) {
	c, read, _ := testClient(t)
	done := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "thread/start", nil); done <- err }()
	var request Message
	if err := read.Decode(&request); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnect reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("pending call stuck")
	}
}

func TestMalformedAndOversizedFramesCloseConnection(t *testing.T) {
	for _, frame := range []string{"not-json\n", string(make([]byte, (8<<20)+1))} {
		reader, writer := io.Pipe()
		client := newClient(reader, discardCloser{}, func() { _ = reader.Close() })
		go func() { _, _ = io.WriteString(writer, frame); _ = writer.Close() }()
		select {
		case <-client.Done():
		case <-time.After(2 * time.Second):
			client.Close()
			t.Fatal("bad frame accepted")
		}
	}
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }
