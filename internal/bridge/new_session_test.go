package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestNewSessionUsesSelectedDirectoryAndKeepsTaskPolicy(t *testing.T) {
	for _, mode := range []string{"default", "binding", "reply", "explicit", "quoted"} {
		t.Run(mode, func(t *testing.T) {
			b, rpc := setup(t)
			dir := filepath.Join(b.cfg.ProjectDir, "another project")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s := catalogSession(t, b, "old")
			s.Cwd = dir
			if _, err := b.store.RememberSession(s); err != nil {
				t.Fatal(err)
			}
			in := input("new-directory", "/new 新名称")
			want := dir
			switch mode {
			case "default":
				want = b.cfg.ProjectDir
			case "binding":
				if err := b.store.Bind(in.ChatID, state.Binding{ThreadID: s.ThreadID, OwnerID: in.UserID}); err != nil {
					t.Fatal(err)
				}
			case "reply":
				in.ReplyTo = "old-message"
				if err := b.store.RememberRoutes([]string{in.ReplyTo}, state.MessageRoute{ThreadID: s.ThreadID, ChatID: in.ChatID, OwnerID: in.UserID, Created: time.Now().Unix()}); err != nil {
					t.Fatal(err)
				}
			case "explicit":
				want = b.cfg.ProjectDir
				in.Text = "/new --cwd " + want + " 新名称"
			case "quoted":
				in.Text = "/new --cwd " + strconv.Quote(dir) + " 新名称"
			}
			rpc.call = func(method string, params any) (json.RawMessage, error) {
				if method == "thread/start" {
					p := params.(map[string]any)
					if p["cwd"] != want || p["sandbox"] != b.cfg.Sandbox || p["approvalPolicy"] != b.cfg.ApprovalPolicy {
						t.Errorf("incorrect directory or policy: %#v", p)
					}
					return json.RawMessage(`{"thread":{"id":"new-thread"}}`), nil
				}
				return json.RawMessage(`{}`), nil
			}
			b.handle(context.Background(), in)
			item := <-b.out
			created, ok := b.store.Session("new-thread")
			binding, _ := b.store.Binding(in.ChatID)
			route, routed := b.store.Route(in.MessageID, in.ChatID, in.UserID)
			if !ok || created.Cwd != want || created.Name != "新名称" || binding.ThreadID != created.ThreadID || !routed || route.ThreadID != created.ThreadID || !strings.Contains(item.Text, "项目："+want) {
				t.Fatalf("new session did not retain selected project and route: %+v / %s", created, item.Text)
			}
			if strings.Join(rpc.calls, ",") != "model/list,thread/start,thread/name/set" {
				t.Fatal(rpc.calls)
			}
		})
	}
}

func TestNewSessionRefusesInvalidDirectoryWithoutRPCOrRebinding(t *testing.T) {
	for _, mode := range []string{"relative", "outside", "symlink", "missing", "file", "bad-quote", "external", "unknown-reply", "owner"} {
		t.Run(mode, func(t *testing.T) {
			b, rpc := setup(t)
			s := catalogSession(t, b, "original")
			if err := b.store.Bind("chat-1", state.Binding{ThreadID: s.ThreadID, OwnerID: "ou_owner"}); err != nil {
				t.Fatal(err)
			}
			in := input("invalid-directory", "/new")
			switch mode {
			case "relative":
				in.Text = "/new --cwd ../relative"
			case "outside":
				in.Text = "/new --cwd " + t.TempDir()
			case "symlink":
				link := filepath.Join(b.cfg.ProjectDir, "link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatal(err)
				}
				in.Text = "/new --cwd " + link
			case "missing":
				in.Text = "/new --cwd " + filepath.Join(b.cfg.ProjectDir, "missing")
			case "file":
				file := filepath.Join(b.cfg.ProjectDir, "file")
				if err := os.WriteFile(file, nil, 0600); err != nil {
					t.Fatal(err)
				}
				in.Text = "/new --cwd " + file
			case "bad-quote":
				in.Text = "/new --cwd \"unfinished"
			case "external":
				s.OriginHome = "/synthetic/external"
				if _, err := b.store.RememberSession(s); err != nil {
					t.Fatal(err)
				}
			case "unknown-reply":
				in.ReplyTo = "unknown"
			case "owner":
				if err := b.store.Bind("chat-1", state.Binding{ThreadID: s.ThreadID, OwnerID: "another-owner"}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := b.store.Binding("chat-1")
			b.handle(context.Background(), in)
			after, _ := b.store.Binding("chat-1")
			if len(rpc.calls) != 0 || before != after || len(b.out) == 0 {
				t.Fatalf("invalid selection changed session or called upstream: %v", rpc.calls)
			}
		})
	}
}

func TestNewSessionRejectsServerDirectoryMismatch(t *testing.T) {
	b, rpc := setup(t)
	dir := filepath.Join(b.cfg.ProjectDir, "selected")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rpc.call = func(string, any) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"thread": map[string]string{"id": "wrong-project", "cwd": b.cfg.ProjectDir}})
	}
	b.handle(context.Background(), input("mismatch", "/new --cwd "+dir))
	if _, ok := b.store.Binding("chat-1"); ok {
		t.Fatal("directory mismatch changed binding")
	}
	if _, ok := b.store.Session("wrong-project"); ok {
		t.Fatal("directory mismatch saved as selected project")
	}
}
