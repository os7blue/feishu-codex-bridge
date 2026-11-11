package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestInputReceiptPrecedesAcknowledgementAndRecoversWithoutReplay(t *testing.T) {
	b, rpc := setup(t)
	x := input("fixture-receive", "/approve fixture-old-request")
	if err := b.Submit(x); err != nil {
		t.Fatal(err)
	}
	receipt, err := b.readInput(b.inputPath(x.MessageID))
	if err != nil || receipt.Status != "received" || receipt.Input.Text != x.Text {
		t.Fatal("acknowledged input was not persisted")
	}
	if info, err := os.Stat(b.inputPath(x.MessageID)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt was not private")
	}
	// A fresh manager has the same synthetic state directory, but no old channels.
	next := New(b.cfg, rpc, fakeSender{}, b.store, b.log, b.renderer)
	if err := next.recoverInputs(); err != nil {
		t.Fatal(err)
	}
	if len(next.in) != 0 || len(rpc.calls) != 0 || len(rpc.responses) != 0 {
		t.Fatal("recovery replayed saved input or an approval")
	}
	notice := <-next.out
	if !strings.Contains(notice.Text, "1 条") || strings.Contains(notice.Text, x.Text) || !strings.Contains(notice.Text, "旧审批") {
		t.Fatal("recovery notice was missing or exposed saved content")
	}
	receipt, _ = next.readInput(next.inputPath(x.MessageID))
	if receipt.Status != "interrupted" {
		t.Fatal("uncertainty was not retained")
	}
	if err := next.recoverInputs(); err != nil || len(next.out) != 0 {
		t.Fatal("recovery notification repeated")
	}
	if err := next.Submit(x); err != nil || len(next.in) != 0 {
		t.Fatal("transport retry replayed an interrupted approval")
	}
}

func TestFullInputQueueRetainsRetryAndHandledDuplicatesStayClosed(t *testing.T) {
	b, _ := setup(t)
	for range cap(b.in) {
		b.in <- input("filler", "/status")
	}
	x := input("fixture-full", "/status")
	if err := b.Submit(x); err == nil {
		t.Fatal("full queue was acknowledged")
	}
	if receipt, err := b.readInput(b.inputPath(x.MessageID)); err != nil || receipt.Status != "received" {
		t.Fatal("full queue lost the durable receipt")
	}
	for len(b.in) > 0 {
		<-b.in
	}
	if err := b.Submit(x); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), <-b.in)
	if receipt, _ := b.readInput(b.inputPath(x.MessageID)); receipt.Status != "handled" {
		t.Fatal("handled command remained uncertain")
	}
	if err := b.Submit(x); err != nil || len(b.in) != 0 {
		t.Fatal("handled transport retry was enqueued again")
	}
}

func TestStartupRecoveryFinishesBeforeFreshTransportMessages(t *testing.T) {
	b, _ := setup(t)
	if err := b.Submit(input("fixture-before-restart", "/status")); err != nil {
		t.Fatal(err)
	}
	next := New(b.cfg, b.rpc, fakeSender{}, b.store, b.log, b.renderer)
	if err := next.Recover(); err != nil {
		t.Fatal(err)
	}
	fresh := input("fixture-after-recovery", "/status")
	if err := next.Submit(fresh); err != nil {
		t.Fatal(err)
	}
	if err := next.Recover(); err != nil {
		t.Fatal(err)
	}
	if receipt, _ := next.readInput(next.inputPath(fresh.MessageID)); receipt.Status != "received" {
		t.Fatal("fresh transport input was mistaken for an interrupted receipt")
	}
	next.handle(context.Background(), <-next.in)
	if receipt, _ := next.readInput(next.inputPath(fresh.MessageID)); receipt.Status != "handled" {
		t.Fatal("fresh input did not finish after startup recovery")
	}
}

func TestInputReceiptWriteFailureDoesNotAcknowledge(t *testing.T) {
	b, _ := setup(t)
	x := input("fixture-write-failure", "/status")
	if err := os.Mkdir(b.inputPath(x.MessageID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := b.Submit(x); err == nil || len(b.in) != 0 {
		t.Fatal("unpersisted input was acknowledged")
	}
}

func TestClaimedQueueCrashStillHasRecoveryReceipt(t *testing.T) {
	b, rpc := setup(t)
	x := input("fixture-claimed", "fixture task body")
	x.ThreadID = "thread-1"
	if err := b.Submit(x); err != nil {
		t.Fatal(err)
	}
	<-b.in
	b.enqueue(x, x.Text, false)
	b.markInput(x.MessageID, "pending")
	q := b.queue[0]
	// Simulate the existing claim removing its queue file before RPC dispatch.
	if err := os.Remove(filepath.Join(b.cfg.StateDir, "queue-"+q.ID+".json")); err != nil {
		t.Fatal(err)
	}
	next := New(b.cfg, rpc, fakeSender{}, b.store, b.log, b.renderer)
	if err := next.recoverInputs(); err != nil || len(next.out) != 1 || len(next.in) != 0 || len(rpc.calls) != 0 {
		t.Fatal("claimed task disappeared or replayed after restart")
	}
}

func TestPendingReceiptSettlesOnlyWithTurnResult(t *testing.T) {
	b, _ := setup(t)
	x := input("fixture-steer", "fixture steering text")
	x.ThreadID = "thread-1"
	b.active = &task{chat: x.ChatID, owner: x.UserID, thread: x.ThreadID, turn: "turn-1"}
	if err := b.Submit(x); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), <-b.in)
	if receipt, _ := b.readInput(b.inputPath(x.MessageID)); receipt.Status != "pending" {
		t.Fatal("dispatched task was prematurely settled")
	}
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	if receipt, _ := b.readInput(b.inputPath(x.MessageID)); receipt.Status != "handled" {
		t.Fatal("persisted final result did not settle input")
	}
}

func TestRecoveryPrunesOldSettledReceiptsAndRejectsCorruption(t *testing.T) {
	b, _ := setup(t)
	x := input("fixture-old", "/status")
	if err := state.WritePrivate(b.inputPath(x.MessageID), inputReceipt{Version: 1, Status: "handled", Created: time.Now().Add(-8 * 24 * time.Hour).Unix(), Input: x}); err != nil {
		t.Fatal(err)
	}
	if err := b.recoverInputs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.inputPath(x.MessageID)); !os.IsNotExist(err) {
		t.Fatal("old settled receipt retained")
	}
	if err := os.WriteFile(b.inputPath("fixture-invalid"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.recoverInputs(); err == nil {
		t.Fatal("corrupt recovery state accepted")
	}
}
