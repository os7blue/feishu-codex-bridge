package bridge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestMaintenancePreservesUnfinishedWork(t *testing.T) {
	cases := []struct {
		name, reason string
		prepare      func(*testing.T, *Bridge)
	}{
		{"turn", "active_turn", func(_ *testing.T, b *Bridge) { b.active = &task{thread: "private-session"} }},
		{"approval", "pending_request", func(_ *testing.T, b *Bridge) {
			b.pending["private-token"] = pending{method: "item/fileChange/requestApproval"}
		}},
		{"question", "pending_request", func(_ *testing.T, b *Bridge) {
			b.pending["private-token"] = pending{method: "item/tool/requestUserInput"}
		}},
		{"queue", "queued_task", func(_ *testing.T, b *Bridge) { b.queue = []queued{{}} }},
		{"file", "file_transfer", func(_ *testing.T, b *Bridge) { b.transfers = map[string]bool{"private-file": true} }},
		{"callback", "operation_in_progress", func(_ *testing.T, b *Bridge) { b.beginOperation() }},
		{"durable_input", "unconfirmed_input", func(t *testing.T, b *Bridge) {
			if _, err := b.retainInput(input("private-message", "private-body")); err != nil {
				t.Fatal(err)
			}
		}},
		{"durable_outbox", "durable_backlog", func(t *testing.T, b *Bridge) {
			if err := state.WritePrivate(filepath.Join(b.cfg.StateDir, "outbox-fixture.json"), map[string]string{"text": "private-body"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"queue_claim_window", "durable_backlog", func(t *testing.T, b *Bridge) {
			if err := state.WritePrivate(filepath.Join(b.cfg.StateDir, "queue-fixture.json"), map[string]string{"text": "private-body"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, rpc := setup(t)
			tc.prepare(t, b)
			r := b.maintenanceReport(true)
			if r.Phase != "busy" || r.Reason != tc.reason || b.draining {
				t.Fatalf("unfinished work was not preserved: %+v", r)
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), "private-") || len(rpc.calls)+len(rpc.responses) != 0 {
				t.Fatal("control exposed payload or ran RPC")
			}
		})
	}
}

func TestMaintenanceRejectsUncertainState(t *testing.T) {
	for _, failure := range []string{"receipt", "failed"} {
		t.Run(failure, func(t *testing.T) {
			b, _ := setup(t)
			if failure == "receipt" {
				os.WriteFile(b.inputPath("bad"), []byte("private-malformed"), 0600)
			} else {
				b.failed.Store(true)
			}
			if r := b.maintenanceReport(true); r.Phase != "unknown" || b.draining {
				t.Fatalf("uncertainty was treated as idle: %+v", r)
			}
		})
	}
}

func TestMaintenanceSubmissionRaceEitherRetainsInputOrCommitsShutdown(t *testing.T) {
	for range 40 {
		b, _ := setup(t)
		var group sync.WaitGroup
		group.Add(2)
		var submitErr error
		var report MaintenanceReport
		go func() { defer group.Done(); submitErr = b.Submit(input("racing-input", "keep this task")) }()
		go func() { defer group.Done(); report = b.maintenanceReport(true) }()
		group.Wait()
		if submitErr == nil {
			if report.Phase != "busy" || b.draining || len(b.in) != 1 {
				t.Fatal("acknowledged input crossed idle shutdown")
			}
			if r, err := b.readInput(b.inputPath("racing-input")); err != nil || r.Status != "received" {
				t.Fatal("acknowledged input was not retained")
			}
		} else if report.Phase != "stopping" || !b.draining || len(b.in) != 0 {
			t.Fatal("shutdown did not reject ingress")
		}
	}
}

func TestMaintenanceFinalDeliveryAndReceiptFinishBeforeIdle(t *testing.T) {
	b, _ := setup(t)
	x := input("last-turn", "work")
	if err := b.Submit(x); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), <-b.in)
	b.jobs.Wait()
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	if r := b.maintenanceReport(false); r.Phase != "busy" {
		t.Fatal("undelivered final result was treated as idle")
	}
	for len(b.out) > 0 {
		b.deliverOne(context.Background(), <-b.out)
	}
	if r := b.maintenanceReport(false); r.Phase != "idle" || b.draining {
		t.Fatalf("finished turn did not become idle: %+v", r)
	}
	if r := b.maintenanceReport(true); r.Phase != "stopping" {
		t.Fatal("idle shutdown was not committed")
	}
	if result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: "old", Action: "draft_send"}); !result.Error {
		t.Fatal("card submission was acknowledged during shutdown")
	}
}

func TestMaintenanceHTTPProtocolDoesNotStopBusyOrWrongProcess(t *testing.T) {
	b, rpc := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := b.maintenanceHandler(cancel)
	request := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return response
	}
	valid, _ := json.Marshal(map[string]int{"pid": os.Getpid()})
	for _, body := range []string{`{"pid":1}`, `{"pid":1,"private":"secret"}`, string(valid) + ` {}`, strings.Repeat("x", 200)} {
		if response := request("/stop", body); response.Code != 409 || b.draining || ctx.Err() != nil {
			t.Fatal("invalid request committed shutdown")
		}
	}
	if response := request("/unknown", string(valid)); response.Code != 404 {
		t.Fatal("unknown endpoint accepted")
	}
	b.active = &task{thread: "private-thread"}
	if response := request("/stop", string(valid)); !strings.Contains(response.Body.String(), `"phase":"busy"`) || b.draining || ctx.Err() != nil {
		t.Fatal("busy request was stopped")
	}
	b.active = nil
	if response := request("/status", string(valid)); !strings.Contains(response.Body.String(), `"phase":"idle"`) || b.draining || ctx.Err() != nil {
		t.Fatal("status request stopped bridge")
	}
	if response := request("/stop", string(valid)); !strings.Contains(response.Body.String(), `"phase":"stopping"`) || !b.draining || ctx.Err() == nil {
		t.Fatal("idle shutdown did not cancel workers")
	}
	if len(rpc.calls)+len(rpc.responses) != 0 {
		t.Fatal("maintenance started model work")
	}
}

type maintenanceBlockedSender struct{ entered, release chan struct{} }

func (s maintenanceBlockedSender) Send(context.Context, string, string, string) error {
	close(s.entered)
	<-s.release
	return nil
}

func TestMaintenanceWaitsForActualDeliveryAndCommandHandler(t *testing.T) {
	b, rpc := setup(t)
	sender := maintenanceBlockedSender{make(chan struct{}), make(chan struct{})}
	b.sender = sender
	b.emit("chat-1", "final result")
	item := <-b.out
	done := make(chan bool, 1)
	go func() { done <- b.deliverOne(context.Background(), item) }()
	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("delivery did not enter transport")
	}
	if r := b.maintenanceReport(true); r.Phase != "busy" || b.draining {
		t.Fatal("in-flight delivery was interrupted")
	}
	close(sender.release)
	if !<-done || b.maintenanceReport(false).Phase != "idle" {
		t.Fatal("delivered result did not become idle")
	}
	entered, release, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first sync.Once
	rpc.call = func(method string, _ any) (json.RawMessage, error) {
		first.Do(func() { close(entered); <-release })
		if method != "thread/start" {
			return json.RawMessage(`{}`), nil
		}
		return json.RawMessage(`{"thread":{"id":"created"}}`), nil
	}
	go func() { defer close(handled); b.handle(context.Background(), input("new", "/new test")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("command handler did not enter RPC")
	}
	if r := b.maintenanceReport(true); r.Phase != "busy" || b.draining {
		t.Fatal("session command was interrupted")
	}
	close(release)
	<-handled
}
