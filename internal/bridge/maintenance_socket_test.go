//go:build integration

package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceSocketUsesLiveProcessAndAtomicStop(t *testing.T) {
	b, rpc := setup(t)
	// Unix socket paths have a short OS limit. Keep fixtures in project TMPDIR.
	dir, err := os.MkdirTemp(os.TempDir(), "mc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	b.cfg.StateDir = dir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closeServer, err := b.ServeMaintenance(cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	if info, err := os.Stat(filepath.Join(dir, "maintenance.sock")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("control socket was not private")
	}
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer requestCancel()
	if _, err := RequestMaintenance(requestCtx, dir, "stop", os.Getpid()+1); err == nil || b.draining {
		t.Fatal("wrong process could commit shutdown")
	}
	b.mu.Lock()
	b.active = &task{thread: "private-thread"}
	b.mu.Unlock()
	if report, err := RequestMaintenance(requestCtx, dir, "stop", os.Getpid()); err != nil || report.Phase != "busy" || ctx.Err() != nil {
		t.Fatal("live turn was not preserved", report, err)
	}
	b.mu.Lock()
	b.active = nil
	b.mu.Unlock()
	if report, err := RequestMaintenance(requestCtx, dir, "status", os.Getpid()); err != nil || report.Phase != "idle" || ctx.Err() != nil {
		t.Fatal("read-only check stopped process", report, err)
	}
	if report, err := RequestMaintenance(requestCtx, dir, "stop", os.Getpid()); err != nil || report.Phase != "stopping" {
		t.Fatal("idle process did not commit shutdown", report, err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("idle stop did not cancel bridge workers")
	}
	if len(rpc.calls)+len(rpc.responses) != 0 {
		t.Fatal("maintenance started model work or replayed approval")
	}
}
