package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// MaintenanceReport contains fixed phases/reasons and a process ID only. Input,
// account, session, configuration and command contents never leave the process.
type MaintenanceReport struct {
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
	Phase    string `json:"phase"`
	Reason   string `json:"reason,omitempty"`
}

func (b *Bridge) beginOperation() bool {
	b.maintenanceMu.Lock()
	defer b.maintenanceMu.Unlock()
	if b.draining {
		return false
	}
	b.operations++
	return true
}

func (b *Bridge) endOperation() {
	b.maintenanceMu.Lock()
	b.operations--
	b.maintenanceMu.Unlock()
}

// The same lock guards ingress and the idle-to-stopping decision. There is no
// interval in which a new submission can be acknowledged after the idle check.
func (b *Bridge) maintenanceReport(stop bool) MaintenanceReport {
	b.maintenanceMu.Lock()
	defer b.maintenanceMu.Unlock()
	r := MaintenanceReport{Protocol: 1, PID: os.Getpid(), Phase: "busy"}
	if b.draining {
		r.Phase, r.Reason = "stopping", "shutdown_committed"
		return r
	}
	if b.failed.Load() {
		r.Phase, r.Reason = "unknown", "bridge_failed"
		return r
	}
	if b.operations != 0 {
		r.Reason = "operation_in_progress"
		return r
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.active != nil:
		r.Reason = "active_turn"
	case len(b.pending) != 0:
		r.Reason = "pending_request"
	case len(b.queue) != 0:
		r.Reason = "queued_task"
	case len(b.transfers) != 0:
		r.Reason = "file_transfer"
	case len(b.in) != 0 || len(b.rpc.Events()) != 0:
		r.Reason = "incoming_work"
	case len(b.out) != 0:
		r.Reason = "outgoing_work"
	default:
		// Full transport queues and uncertain RPCs can leave durable receipts
		// without an in-memory task. A missing in-memory task is not idle proof.
		paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, "inbox-*.json"))
		if err != nil || len(paths) > 20000 {
			r.Phase, r.Reason = "unknown", "receipt_check_failed"
			return r
		}
		for _, path := range paths {
			receipt, err := b.readInput(path)
			if err != nil {
				r.Phase, r.Reason = "unknown", "receipt_check_failed"
				return r
			}
			if receipt.Status != "handled" && receipt.Status != "interrupted" {
				r.Reason = "unconfirmed_input"
				return r
			}
		}
		for _, pattern := range []string{"queue-*.json", "outbox-*.json"} {
			paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, pattern))
			if err != nil {
				r.Phase, r.Reason = "unknown", "backlog_check_failed"
				return r
			}
			if len(paths) != 0 {
				r.Reason = "durable_backlog"
				return r
			}
		}
		r.Phase = "idle"
		if stop {
			b.draining = true
			r.Phase = "stopping"
		}
	}
	return r
}

// ServeMaintenance must be called after state.Open has obtained the exclusive
// bridge lock and Recover has finished. Only then may a stale socket be removed.
// The socket is private, local control; it is not a remote update authorization.
func (b *Bridge) ServeMaintenance(cancel context.CancelFunc) (func(), error) {
	path := filepath.Join(b.cfg.StateDir, "maintenance.sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || os.Remove(path) != nil {
			return nil, errors.New("cannot reserve maintenance socket")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot inspect maintenance socket")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("cannot listen for local maintenance")
	}
	if os.Chmod(path, 0600) != nil {
		listener.Close()
		return nil, errors.New("cannot protect maintenance socket")
	}
	server := &http.Server{Handler: b.maintenanceHandler(cancel), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.fail(errors.New("local maintenance listener failed"))
		}
	}()
	return func() { server.Close() }, nil
}

func (b *Bridge) maintenanceHandler(cancel context.CancelFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || (req.URL.Path != "/status" && req.URL.Path != "/stop") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var target struct {
			PID int `json:"pid"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128))
		d.DisallowUnknownFields()
		var extra any
		if d.Decode(&target) != nil || d.Decode(&extra) != io.EOF || target.PID != os.Getpid() {
			w.WriteHeader(http.StatusConflict)
			return
		}
		r := b.maintenanceReport(req.URL.Path == "/stop")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(r)
		if req.URL.Path == "/stop" && r.Phase == "stopping" {
			// Flush the decision before canceling workers/transport. Even a lost
			// response never reopens ingress after shutdown has been committed.
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			cancel()
		}
	})
}

// RequestMaintenance loads no state. The normal CLI supplies the configured
// directory and expected process, and talks to the existing bridge only.
func RequestMaintenance(ctx context.Context, dir, mode string, pid int) (MaintenanceReport, error) {
	var report MaintenanceReport
	if (mode != "status" && mode != "stop") || pid < 2 {
		return report, errors.New("invalid maintenance request")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "maintenance.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]int{"pid": pid})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://bridge/"+mode, bytes.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		return report, errors.New("live maintenance control unavailable; running bridge was not forced to stop")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&report) != nil || report.Protocol != 1 || report.PID != pid {
		return MaintenanceReport{}, errors.New("live maintenance response not confirmed")
	}
	switch report.Phase {
	case "idle", "busy", "unknown", "stopping":
		return report, nil
	default:
		return MaintenanceReport{}, errors.New("live maintenance phase not confirmed")
	}
}
