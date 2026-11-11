// Package codex implements the official App Server JSONL protocol over stdio.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error deliberately omits provider text; callers may log the code safely.
func (e *RPCError) Error() string { return fmt.Sprintf("Codex RPC error %d", e.Code) }

type Client struct {
	writer  io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan Message
	nextID  atomic.Uint64
	events  chan Message
	done    chan struct{}
	once    sync.Once
	stop    func()
}

func Start(ctx context.Context, binary, cwd, home string, stderr io.Writer) (*Client, error) {
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	cmd.Dir = cwd
	// Feishu credentials must not be inherited by the coding agent or its tools.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FEISHU_APP_SECRET=") && !strings.HasPrefix(entry, "FEISHU_APP_ID=") && !(home != "" && strings.HasPrefix(entry, "CODEX_HOME=")) {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	if home != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+home)
	}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("cannot create Codex stdin")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, errors.New("cannot create Codex stdout")
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, errors.New("cannot start codex app-server; check codex_binary")
	}
	c := newClient(out, in, func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait(); c.fail() }()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = c.Call(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "codecv_feishu_bridge", "title": "CodeCV Feishu Bridge", "version": "0.1.0"},
		// humanCommand checkpoints are delivered only to clients that promise
		// to show them to a person and return a one-time decision. The bridge's
		// owner-bound /approve and /deny flow never grants persistent approval.
		"capabilities": map[string]bool{"experimentalApi": true, "requiredHumanApproval": true},
	})
	if err == nil {
		err = c.Notify("initialized", map[string]any{})
	}
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("Codex initialization failed: %w", err)
	}
	return c, nil
}

// newClient is also used with in-memory pipes by protocol tests.
func newClient(reader io.Reader, writer io.WriteCloser, stop func()) *Client {
	c := &Client{writer: writer, pending: map[string]chan Message{}, events: make(chan Message, 256), done: make(chan struct{}), stop: stop}
	go c.readLoop(reader)
	return c
}

func (c *Client) Events() <-chan Message { return c.events }
func (c *Client) Done() <-chan struct{}  { return c.done }
func (c *Client) Close()                 { c.fail() }

func (c *Client) fail() {
	c.once.Do(func() {
		close(c.done)
		if c.stop != nil {
			c.stop()
		}
		_ = c.writer.Close()
	})
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	id := c.nextID.Add(1)
	key := strconv.FormatUint(id, 10)
	reply := make(chan Message, 1)
	c.mu.Lock()
	if len(c.pending) >= 64 {
		c.mu.Unlock()
		return nil, errors.New("too many pending RPCs")
	}
	c.pending[key] = reply
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case message := <-reply:
		if message.Error != nil {
			return nil, message.Error
		}
		return message.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("Codex connection closed")
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.send(map[string]any{"method": method, "params": params})
}

func (c *Client) Respond(id json.RawMessage, result any) error {
	return c.send(map[string]any{"id": id, "result": result})
}

func (c *Client) Reject(id json.RawMessage) error {
	return c.send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Unsupported request in Feishu bridge"}})
}

func (c *Client) send(message any) error {
	select {
	case <-c.done:
		return errors.New("Codex connection closed")
	default:
	}
	c.writeMu.Lock()
	err := json.NewEncoder(c.writer).Encode(message)
	c.writeMu.Unlock()
	if err != nil {
		c.fail()
		return errors.New("Codex write failed")
	}
	return nil
}

func (c *Client) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		var message Message
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			c.fail()
			return
		}
		if message.Method == "" && len(message.ID) > 0 {
			c.mu.Lock()
			ch := c.pending[string(message.ID)]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- message:
				default:
				}
			}
			continue
		}
		// Completed agentMessage items carry the full text. The bridge forwards
		// those and tool lifecycle events, not token/stdout/reasoning deltas.
		if len(message.ID) == 0 && (strings.HasSuffix(strings.ToLower(message.Method), "delta") || strings.Contains(message.Method, "reasoning")) {
			continue
		}
		select {
		case c.events <- message:
		case <-c.done:
			return
		default:
			c.fail()
			return // Never silently drop completion or approval events.
		}
	}
	c.fail()
}
