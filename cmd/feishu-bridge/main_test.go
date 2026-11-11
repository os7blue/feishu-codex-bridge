package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestReadyReceiptIsExclusiveAndContainsNoConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.json")
	if err := writeReady(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if json.Unmarshal(data, &receipt) != nil || len(receipt) != 2 || receipt["ready"] != true || receipt["pid"] != float64(os.Getpid()) {
		t.Fatal("invalid public readiness receipt")
	}
	if writeReady(path) == nil {
		t.Fatal("readiness receipt overwritten")
	}
}

func TestOfflineSlashPlanDoesNotLoadConfiguration(t *testing.T) {
	var output bytes.Buffer
	err := runArgs(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"--config", "/nonexistent-private-config", "--slash-commands", "plan"}, &output)
	var commands []commandset.Command
	if err != nil || json.Unmarshal(output.Bytes(), &commands) != nil || len(commands) != len(commandset.All()) {
		t.Fatalf("offline plan unexpectedly required private configuration: %v", err)
	}
}

func TestSlashFlagsRejectConflictsBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{"--maintenance-hold", "--check"},
		{"--maintenance-hold", "--maintenance", "status", "--maintenance-pid", "123"},
		{"--maintenance-hold", "--slash-commands", "plan"},
		{"--maintenance-hold", "--send-file", "sensitive-path"},
		{"--maintenance", "stop"},
		{"--maintenance-pid", "123"},
		{"--maintenance", "private-sensitive", "--maintenance-pid", "123"},
		{"--maintenance", "stop", "--maintenance-pid", "123", "--check"},
		{"--maintenance", "status", "--maintenance-pid", "123", "--slash-commands", "plan"},
		{"--maintenance", "stop", "--maintenance-pid", "123", "--send-file", "sensitive-path"},
		{"--send-file", "sensitive-path", "--check"},
		{"--send-file", "sensitive-path", "--notify-owner", "sensitive-value"},
		{"--send-file", "sensitive-path", "--slash-commands", "plan"},
		{"--card-probe", "--check"},
		{"--card-probe", "--slash-commands", "plan"},
		{"--card-probe", "--notify-owner", "sensitive-value"},
		{"--card-probe", "--ready-file", "sensitive-path"},
		{"--slash-commands="},
		{"--slash-commands", "invalid-sensitive"},
		{"--slash-commands", "plan", "--pair"},
		{"--slash-commands", "plan", "--click-demo"},
		{"--click-demo", "--check"},
		{"--interaction-evidence-file", "sensitive-path", "--check"},
		{"--interaction-evidence-file", "sensitive-path", "--notify-owner", "sensitive-value"},
		{"--interaction-evidence-file", "sensitive-path", "--slash-commands", "plan"},
		{"--click-demo", "--notify-owner", "sensitive-notification"},
		{"--slash-commands", "check", "--check"},
		{"--slash-commands", "sync", "--notify-owner", "sensitive-notification"},
		{"--slash-commands", "plan", "sensitive-extra"},
		{"--unknown-sensitive", "sensitive-value"},
	} {
		var output bytes.Buffer
		err := runArgs(slog.New(slog.NewTextHandler(io.Discard, nil)), args, &output)
		if err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "configuration") || output.Len() != 0 {
			t.Fatal("invalid flags reached config loading, leaked input or emitted a plan")
		}
	}
}

func TestMaintenanceHoldLocksUntilStdinClosesWithoutLoadingState(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(dir, "app", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	input, release := io.Pipe()
	output, report := io.Pipe()
	defer input.Close()
	defer output.Close()
	defer release.Close()
	done := make(chan error, 1)
	go func() { defer report.Close(); done <- holdMaintenance(dir, input, report) }()
	var received map[string]any
	if err := json.NewDecoder(output).Decode(&received); err != nil || len(received) != 3 || received["phase"] != "locked" {
		t.Fatal("invalid payload-free lock report", err)
	}
	if lock, err := state.LockForMaintenance(dir); err == nil {
		lock.Close()
		t.Fatal("helper did not hold the exclusive lock")
	}
	release.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lock, err := state.LockForMaintenance(dir)
	if err != nil {
		t.Fatal("stdin EOF did not release lock")
	}
	lock.Close()
}
