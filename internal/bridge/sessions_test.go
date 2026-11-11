package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func catalogSession(t *testing.T, b *Bridge, id string) state.Session {
	t.Helper()
	s, err := b.store.RememberSession(state.Session{ThreadID: id, Cwd: b.cfg.ProjectDir, OwnerID: "ou_owner"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestForkFirstPromptUsesLoadedThreadAndArchiveInvalidatesIt(t *testing.T) {
	b, f := setup(t)
	catalogSession(t, b, "source")
	f.call = func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "thread/fork":
			return json.RawMessage(`{"thread":{"id":"forked"}}`), nil
		case "turn/start":
			return json.RawMessage(`{"turn":{"id":"turn-forked"}}`), nil
		default:
			return json.RawMessage(`{}`), nil
		}
	}
	b.handle(context.Background(), input("fork", "/fork #1"))
	b.handle(context.Background(), input("continue", "#2 continue"))
	b.jobs.Wait()
	if b.active == nil || b.active.thread != "forked" || b.active.turn != "turn-forked" {
		t.Fatal("forked thread did not start")
	}
	if got := strings.Join(f.calls, ","); got != "thread/fork,model/list,turn/start" {
		t.Fatalf("unexpected fork lifecycle: %s", got)
	}
	b.event(notification("turn/completed", `{"threadId":"forked","turn":{"id":"turn-forked","status":"completed"}}`))
	b.handle(context.Background(), input("archive", "/archive #2"))
	if b.loaded["forked"] {
		t.Fatal("archived thread remained marked loaded")
	}
}

func sessionRPC(b *Bridge, f *fakeRPC) {
	f.call = func(method string, params any) (json.RawMessage, error) {
		wire, _ := json.Marshal(params)
		var p map[string]any
		_ = json.Unmarshal(wire, &p)
		switch method {
		case "thread/resume", "thread/read":
			raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": p["threadId"], "cwd": b.cfg.ProjectDir, "status": map[string]string{"type": "idle"}}})
			return raw, nil
		case "turn/start":
			raw, _ := json.Marshal(map[string]any{"turn": map[string]any{"id": "turn-" + p["threadId"].(string)}})
			return raw, nil
		default:
			return json.RawMessage(`{}`), nil
		}
	}
}

func TestReplyTargetsOriginalSessionAfterDefaultSwitch(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	c := catalogSession(t, b, "session-b")
	sessionRPC(b, f)
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: c.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if err := b.store.RememberRoutes([]string{"bot-message-a"}, state.MessageRoute{ChatID: "chat-1", OwnerID: "ou_owner", ThreadID: a.ThreadID, Created: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	x := input("reply-a", "continue")
	x.ReplyTo = "bot-message-a"
	b.handle(context.Background(), x)
	b.jobs.Wait()
	if b.active == nil || b.active.thread != a.ThreadID {
		t.Fatal("reply went to the selected default instead of its source")
	}
	binding, _ := b.store.Binding("chat-1")
	if binding.ThreadID != c.ThreadID {
		t.Fatal("reply changed the default session")
	}
}

func TestRenameUsesExplicitReplyOrDefaultTarget(t *testing.T) {
	cases := []struct {
		name, text, reply, target string
	}{
		{"reply", "/rename 桥接 构建", "reply-a", "session-a"},
		{"explicit wins", "/rename #2 桥接 构建", "reply-a", "session-b"},
		{"prefix wins", "#2\n/rename 桥接 构建", "reply-a", "session-b"},
		{"default", "/rename\t桥接 构建", "", "session-b"},
		{"unknown reply", "/rename 桥接 构建", "unknown", ""},
		{"unknown explicit", "/rename #999 桥接 构建", "reply-a", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, f := setup(t)
			catalogSession(t, b, "session-a")
			catalogSession(t, b, "session-b")
			if err := b.store.Bind("chat-1", state.Binding{ThreadID: "session-b", OwnerID: "ou_owner"}); err != nil {
				t.Fatal(err)
			}
			if err := b.store.RememberRoutes([]string{"reply-a"}, state.MessageRoute{ChatID: "chat-1", OwnerID: "ou_owner", ThreadID: "session-a", Created: time.Now().Unix()}); err != nil {
				t.Fatal(err)
			}
			f.call = func(method string, params any) (json.RawMessage, error) {
				p := params.(map[string]any)
				if method != "thread/name/set" || p["threadId"] != tc.target || p["name"] != "桥接 构建" {
					t.Fatalf("rename targeted the wrong session: %s %v", method, p)
				}
				return json.RawMessage(`{}`), nil
			}
			x := input("rename", tc.text)
			x.ReplyTo = tc.reply
			b.handle(context.Background(), x)
			if tc.target == "" {
				if len(f.calls) != 0 {
					t.Fatal("unresolved target changed a session")
				}
			} else {
				s, _ := b.store.Session(tc.target)
				if len(f.calls) != 1 || s.Name != "桥接 构建" {
					t.Fatal("resolved rename did not update the session")
				}
			}
			if binding, _ := b.store.Binding("chat-1"); binding.ThreadID != "session-b" {
				t.Fatal("rename changed the default session")
			}
		})
	}
}

func TestStatusOverviewAndQuotedStatusHaveDifferentScopes(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	c := catalogSession(t, b, "session-b")
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: c.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if err := b.store.RememberRoutes([]string{"reply-a"}, state.MessageRoute{ChatID: "chat-1", OwnerID: "ou_owner", ThreadID: a.ThreadID, Created: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: a.ThreadID, turn: "turn-a", started: time.Now()}
	b.handle(context.Background(), input("all", "/status"))
	item := <-b.out
	if item.ThreadID != "" || !strings.Contains(item.Text, "普通消息默认目标：会话 #2") || !strings.Contains(item.Text, "当前任务：会话 #1") || !strings.Contains(item.Text, "状态：执行中") {
		t.Fatal("overview did not distinguish active and default sessions")
	}
	x := input("specific", "/status")
	x.ReplyTo = "reply-a"
	b.handle(context.Background(), x)
	item = <-b.out
	if item.ThreadID != a.ThreadID || !strings.Contains(item.Text, "状态：执行中") || len(f.calls) != 0 {
		t.Fatal("quoted status lost session scope or started work")
	}
}

func TestMissingCommandTargetNeverStopsOrCreatesAnotherSession(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: a.ThreadID, turn: "turn-a"}
	b.handle(context.Background(), input("stop", "/stop"))
	b.handle(context.Background(), input("handoff", "/handoff"))
	b.jobs.Wait()
	if b.active.stopping || len(f.calls) != 0 || len(b.queue) != 0 {
		t.Fatal("a command without a target affected an unrelated session")
	}
}

func TestBareSessionNumberShowsRoutableInfoWithoutSwitching(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	c := catalogSession(t, b, "session-b")
	sessionRPC(b, f)
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: c.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), input("show", "#1"))
	item := <-b.out
	if item.ThreadID != a.ThreadID || !strings.Contains(item.Text, "项目："+b.cfg.ProjectDir) || strings.Join(f.calls, ",") != "thread/read,model/list" {
		t.Fatal("bare reference did not return routable session info")
	}
	if binding, _ := b.store.Binding("chat-1"); binding.ThreadID != c.ThreadID {
		t.Fatal("viewing a session changed the default")
	}
}

func TestExplicitTargetWinsOverReplyAndUnknownReplyFailsClosed(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	c := catalogSession(t, b, "session-b")
	sessionRPC(b, f)
	if err := b.store.RememberRoutes([]string{"old"}, state.MessageRoute{ChatID: "chat-1", OwnerID: "ou_owner", ThreadID: a.ThreadID, Created: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	x := input("m1", "#2 continue")
	x.ReplyTo = "old"
	b.handle(context.Background(), x)
	b.jobs.Wait()
	if b.active == nil || b.active.thread != c.ThreadID {
		t.Fatal("explicit session did not override quoted message")
	}
	before := len(f.calls)
	x = input("m2", "continue")
	x.ReplyTo = "unknown"
	b.handle(context.Background(), x)
	if len(f.calls) != before || len(b.queue) != 0 {
		t.Fatal("unknown reply was executed or queued against an unrelated session")
	}
}

func TestQueuedTargetSurvivesSelectionAndOnlyStartsAfterCompletion(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	c := catalogSession(t, b, "session-b")
	d := catalogSession(t, b, "session-c")
	sessionRPC(b, f)
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: a.ThreadID, turn: "turn-a", started: time.Now()}
	b.handle(context.Background(), input("queue", "#2 work on b"))
	if len(b.queue) != 1 || len(f.calls) != 0 {
		t.Fatal("queued task started while another model task was active")
	}
	b.handle(context.Background(), input("switch", "/use #3"))
	if b.queue[0].Input.ThreadID != c.ThreadID {
		t.Fatal("selection retargeted a queued task")
	}
	b.event(notification("turn/completed", `{"threadId":"session-a","turn":{"id":"turn-a","status":"completed"}}`))
	b.startNext(context.Background())
	b.jobs.Wait()
	if b.active == nil || b.active.thread != c.ThreadID {
		t.Fatal("next model task used the wrong session")
	}
	binding, _ := b.store.Binding("chat-1")
	if binding.ThreadID != d.ThreadID {
		t.Fatal("queued task changed the newer default selection")
	}
}

func TestStopAnotherSessionCancelsQueueWithoutInterruptingActive(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	catalogSession(t, b, "session-b")
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: a.ThreadID, turn: "turn-a", started: time.Now()}
	b.handle(context.Background(), input("queued", "#2 queued task"))
	b.handle(context.Background(), input("stop", "/stop #2"))
	if len(b.queue) != 0 || len(f.calls) != 0 || b.active.stopping {
		t.Fatal("stop was delivered to an unrelated active session")
	}
	paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, "queue-*.json"))
	if err != nil || len(paths) != 0 {
		t.Fatal("cancelled task remained executable on disk")
	}
}

func TestRecoveredQueueIsRetainedAndNeverReplayed(t *testing.T) {
	b, f := setup(t)
	a := catalogSession(t, b, "session-a")
	x := input("q1", "work")
	x.ThreadID = a.ThreadID
	b.enqueue(x, "work", false)
	if err := b.recoverQueue(); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "cancelled-queue-*.json"))
	if len(paths) != 1 || len(f.calls) != 0 {
		t.Fatal("recovered task replayed or lost")
	}
}

type routedSender struct{ ids []string }

func (s routedSender) Send(context.Context, string, string, string) error { return nil }
func (s routedSender) SendRouted(context.Context, string, string, string) ([]string, error) {
	return s.ids, nil
}

func TestSentMessageIDsRouteRepliesAndPersist(t *testing.T) {
	b, _ := setup(t)
	a := catalogSession(t, b, "session-a")
	b.sender = routedSender{[]string{"part-1", "part-2"}}
	b.emit("chat-1", "result", a.ThreadID)
	item := <-b.out
	if !strings.HasPrefix(item.Text, "[会话 #1]") || !b.deliverOne(context.Background(), item) {
		t.Fatal("routed delivery failed")
	}
	for _, id := range []string{"part-1", "part-2"} {
		route, ok := b.store.Route(id, "chat-1", "ou_owner")
		if !ok || route.ThreadID != a.ThreadID {
			t.Fatal("message chunk lost session routing")
		}
		if _, ok := b.store.Route(id, "other-chat", "ou_owner"); ok {
			t.Fatal("reply route crossed chat boundary")
		}
		if _, ok := b.store.Route(id, "chat-1", "ou_other"); ok {
			t.Fatal("reply route crossed owner boundary")
		}
	}
}

func TestReplyDescriptionSurvivesFollowupAndDefaultSwitch(t *testing.T) {
	b, _ := setup(t)
	b.handle(context.Background(), input("first-task", "构建飞书桥接程序\n  验证多会话回复"))
	b.jobs.Wait()
	a, ok := b.store.Session("thread-1")
	if !ok || a.Summary != "构建飞书桥接程序 验证多会话回复" {
		t.Fatal("first task description was not recorded")
	}
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	b.handle(context.Background(), input("followup", "目前任务如何了"))
	b.jobs.Wait()
	c := catalogSession(t, b, "another-session")
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: c.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	for len(b.out) > 0 {
		<-b.out
	}
	b.event(notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","text":"构建完成"}}`))
	b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	item := <-b.out
	want := "[会话 #1]\n简述：构建飞书桥接程序 验证多会话回复\n状态：已完成\n当前轮 · 模型：test-model · 思考强度：medium\n项目："
	if !strings.HasPrefix(item.Text, want) || !strings.Contains(item.Text, "状态快照：") || !strings.HasSuffix(item.Text, "\n\n构建完成") || item.ThreadID != a.ThreadID {
		t.Fatalf("reply lost its task description or original directory: %q", item.Text)
	}
}

func TestSessionPreviewAndRenamedTitleAppearInReplies(t *testing.T) {
	b, f := setup(t)
	f.call = func(method string, params any) (json.RawMessage, error) {
		if method == "thread/list" {
			raw, err := json.Marshal(map[string]any{"data": []map[string]any{{"id": "a", "preview": strings.Repeat("桥接", 50), "cwd": b.cfg.ProjectDir}}})
			return raw, err
		}
		return json.RawMessage(`{}`), nil
	}
	b.handle(context.Background(), input("list", "/sessions"))
	s, ok := b.store.SessionByRef(1)
	if !ok || utf8.RuneCountInString(s.Summary) != 60 || !strings.HasSuffix(s.Summary, "…") {
		t.Fatal("preview was not saved as a bounded Unicode description")
	}
	if item := <-b.out; !strings.Contains(item.Text, s.Summary) {
		t.Fatal("session list omitted the description")
	}
	b.handle(context.Background(), input("rename", "/rename #1 飞书桥接构建"))
	item := <-b.out
	if !strings.Contains(item.Text, "简述：飞书桥接构建\n状态：空闲") || strings.Contains(item.Text, "路径：") {
		t.Fatal("renamed session title did not replace the preview in replies")
	}
	if len(f.calls) != 2 || f.calls[0] != "thread/list" || f.calls[1] != "thread/name/set" {
		t.Fatal("reply descriptions made extra protocol or model requests")
	}
}

func TestSessionListIncludesProvidersSourcesAndPagination(t *testing.T) {
	b, f := setup(t)
	page := 0
	f.call = func(method string, params any) (json.RawMessage, error) {
		if method != "thread/list" {
			t.Fatalf("unexpected RPC %s", method)
		}
		p := params.(map[string]any)
		if _, has := p["cwd"]; has || len(p["modelProviders"].([]string)) != 0 || len(p["sourceKinds"].([]string)) < 5 {
			t.Fatal("catalog silently filtered sessions")
		}
		page++
		if page == 1 {
			return json.RawMessage(`{"data":[{"id":"a","name":"A","status":{"type":"idle"}}],"nextCursor":"second"}`), nil
		}
		if p["cursor"] != "second" {
			t.Fatal("pagination cursor lost")
		}
		return json.RawMessage(`{"data":[{"id":"b","name":"B","status":{"type":"idle"}}],"nextCursor":null}`), nil
	}
	b.handle(context.Background(), input("list1", "/sessions"))
	b.handle(context.Background(), input("list2", "/sessions next"))
	a, ok := b.store.SessionByRef(1)
	if !ok || a.ThreadID != "a" {
		t.Fatal("first session missing")
	}
	c, ok := b.store.SessionByRef(2)
	if !ok || c.ThreadID != "b" {
		t.Fatal("second page missing")
	}
}

func TestThreadOutsideWorkspaceNeverStartsTurn(t *testing.T) {
	b, f := setup(t)
	outside := t.TempDir()
	s, err := b.store.RememberSession(state.Session{ThreadID: "outside", Cwd: outside, OwnerID: "ou_owner"})
	if err != nil {
		t.Fatal(err)
	}
	x := input("x", "work")
	x.ThreadID = s.ThreadID
	x.Explicit = true
	b.prompt(context.Background(), x, "work", false)
	b.jobs.Wait()
	if len(f.calls) != 0 || b.active != nil {
		t.Fatal("out-of-scope project was resumed or executed")
	}
}

func TestReadOnlyExternalCatalogAndHistory(t *testing.T) {
	b, f := setup(t)
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "09")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "01900000-0000-7000-8000-000000000001"
	path := filepath.Join(dir, "rollout-test-"+id+".jsonl")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": b.cfg.ProjectDir}})
	data := string(meta) + "\n" + `{"type":"response_item","payload":{"role":"assistant","content":[{"text":"previous answer"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	b.cfg.CatalogHomes = []string{home}
	f.call = func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"data":[],"nextCursor":null}`), nil
	}
	b.handle(context.Background(), input("list", "/sessions"))
	s, ok := b.store.SessionByRef(1)
	if !ok || s.OriginHome != home {
		t.Fatal("external session missing from catalog")
	}
	b.handle(context.Background(), input("history", "/history #1"))
	b.handle(context.Background(), input("work", "#1 continue"))
	b.jobs.Wait()
	if len(f.calls) != 1 {
		t.Fatal("external session was resumed automatically")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != data {
		t.Fatal("external session was changed")
	}
}
