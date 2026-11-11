package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func TestNativeCatalogCoversCardsAndHelpWithUniqueRoots(t *testing.T) {
	names := map[string]bool{}
	for _, command := range commandset.All() {
		if names[command.Name] || command.Name == "" || strings.ContainsAny(command.Name, "/ \t\n") || command.Description == "" || !strings.HasPrefix(command.Usage, "/"+command.Name) || !strings.Contains(help, command.Usage) {
			t.Fatal("public root catalog and help disagree")
		}
		names[command.Name] = true
	}
	for _, template := range commandTemplates {
		root := strings.TrimPrefix(strings.Fields(template.text)[0], "/")
		if !names[root] {
			t.Fatal("card root missing from native command catalog")
		}
	}
	if names["start"] || names["pair"] || names["sessions next"] {
		t.Fatal("alias, bootstrap command or argument registered as a public root")
	}
}

func openMenu(t *testing.T, b *Bridge, text string) string {
	t.Helper()
	b.handle(context.Background(), input("menu-"+text, text))
	for token, grant := range b.drafts {
		if grant.menu {
			return token
		}
	}
	t.Fatal("menu was not created")
	return ""
}

func openDraft(t *testing.T, b *Bridge, menu, command string) (string, CardResult) {
	t.Helper()
	result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: menu, Action: "draft_open", Command: command})
	if result.Error || !json.Valid(result.Card) {
		t.Fatalf("draft not opened: %s", result.Message)
	}
	var card struct {
		Elements []struct {
			Tag      string `json:"tag"`
			Elements []struct {
				Value map[string]string `json:"value"`
			} `json:"elements"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(result.Card, &card); err != nil {
		t.Fatal(err)
	}
	for _, element := range card.Elements {
		if element.Tag == "form" {
			for _, control := range element.Elements {
				if control.Value["action"] == "draft_send" {
					return control.Value["token"], result
				}
			}
		}
	}
	t.Fatal("draft has no send form")
	return "", result
}

func TestCommandHighlightsAllowDirectReselection(t *testing.T) {
	b, rpc := setup(t)
	session, err := b.store.RememberSession(state.Session{ThreadID: "thread-target", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	if err != nil {
		t.Fatal(err)
	}
	target := "#" + strconv.Itoa(session.Ref)
	menu := openMenu(t, b, "/commands "+target)
	first, _ := openDraft(t, b, menu, "show")
	deadline := b.drafts[first].expires
	next, card := openDraft(t, b, first, "history")
	if _, exists := b.drafts[first]; exists {
		t.Fatal("previous editable grant survived reselection")
	}
	if !b.drafts[next].expires.Equal(deadline) || !strings.Contains(string(card.Card), `"default_value":"/history `+target+`"`) || !strings.Contains(string(card.Card), `"text_color":"blue"`) || strings.Contains(string(card.Card), "返回选项") || strings.Count(string(card.Card), `"tag":"button"`) != 1 {
		t.Fatal("reselection lost target, deadline or simple highlighted controls")
	}
	if result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: first, Action: "draft_send", Text: "/show " + target}); !result.Error || len(b.in) != 0 || len(rpc.calls) != 0 {
		t.Fatal("old form submitted or selecting executed a command")
	}
}

func TestClickOnlyDraftsAndEditedSendIsOnce(t *testing.T) {
	b, rpc := setup(t)
	menu := openMenu(t, b, "/commands")
	token, card := openDraft(t, b, menu, "new")
	if len(rpc.calls) != 0 || len(b.in) != 0 || !strings.Contains(string(card.Card), `"default_value":"/new"`) {
		t.Fatal("opening draft executed a command or lost its prefill")
	}
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/new 修改后的名称"}
	var jobs sync.WaitGroup
	for range 8 {
		jobs.Add(1)
		go func() { defer jobs.Done(); b.HandleCard(action) }()
	}
	jobs.Wait()
	if len(b.in) != 1 || len(rpc.calls) != 0 {
		t.Fatal("send was repeated or callback performed RPC")
	}
	x := <-b.in
	if x.Text != action.Text || x.ReplyTo != "" || x.MessageID != "card-draft:"+token {
		t.Fatal("edited text or deduplication ID lost")
	}
	b.handle(context.Background(), x)
	if strings.Join(rpc.calls, ",") != "model/list,thread/start,thread/name/set" {
		t.Fatal("submitted edit did not enter ordinary command handling")
	}
	if session, ok := b.store.Session("thread-1"); !ok || session.Name != "修改后的名称" {
		t.Fatal("edited name was not applied")
	}
}

func TestClickDemoQueuesRealCardAndOnlyManualStatusSend(t *testing.T) {
	b, rpc := setup(t)
	if err := b.store.Pair("ou_owner"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Bind("chat-1", state.Binding{OwnerID: "ou_owner", ThreadID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SendClickDemo(); err != nil {
		t.Fatal(err)
	}
	item := <-b.out
	if !strings.Contains(string(item.Card), "点击命令测试") || !strings.Contains(string(item.Card), `"tag":"interactive_container"`) || strings.Contains(string(item.Card), `"tag":"button"`) || len(rpc.calls) != 0 || len(b.in) != 0 {
		t.Fatal("demo did not queue a real inert command-text card")
	}
	menu := ""
	for token, grant := range b.drafts {
		if grant.menu && grant.demo && grant.request == "" {
			menu = token
		}
	}
	if menu == "" || !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: menu, Action: "draft_open", Command: "new"}).Error {
		t.Fatal("demo allowed another command")
	}
	token, card := openDraft(t, b, menu, "status")
	if !strings.Contains(string(card.Card), `"default_value":"/status"`) || len(rpc.calls) != 0 || len(b.in) != 0 {
		t.Fatal("demo click executed work or lost prefill")
	}
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/new"}
	if !b.HandleCard(action).Error || len(b.in) != 0 {
		t.Fatal("demo submitted another command")
	}
	action.Text = "/status"
	if b.HandleCard(action).Error || len(b.in) != 1 || len(rpc.calls) != 0 {
		t.Fatal("manual status send failed")
	}
	b.handle(context.Background(), <-b.in)
	if len(rpc.calls) != 0 {
		t.Fatal("status demo invoked the model")
	}
}

func TestClickDemoRequiresPairedAuthorizedChat(t *testing.T) {
	b, rpc := setup(t)
	if b.SendClickDemo() == nil || len(b.out) != 0 || len(b.drafts) != 0 {
		t.Fatal("demo emitted without paired chat")
	}
	if err := b.store.Pair("ou_other"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Bind("chat-1", state.Binding{OwnerID: "ou_other", ThreadID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if b.SendClickDemo() == nil || len(b.out) != 0 || len(b.drafts) != 0 || len(rpc.calls) != 0 {
		t.Fatal("demo emitted for unauthorized paired account")
	}
}

func TestDraftTargetRemainsVisibleAfterDefaultChanges(t *testing.T) {
	b, _ := setup(t)
	session, err := b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: session.ThreadID, OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	menu := openMenu(t, b, "/commands")
	if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-b", OwnerID: "ou_owner"}); err != nil {
		t.Fatal(err)
	}
	_, card := openDraft(t, b, menu, "stop")
	if !strings.Contains(string(card.Card), `"default_value":"/stop #1"`) {
		t.Fatal("draft followed changed default rather than its visible target")
	}
}

func TestEditedCommandDraftKeepsCapturedSession(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "natural language"
		if explicit {
			name = "explicit target overrides"
		}
		t.Run(name, func(t *testing.T) {
			b, rpc := setup(t)
			for _, id := range []string{"thread-a", "thread-b"} {
				if _, err := b.store.RememberSession(state.Session{ThreadID: id, OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir}); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-a", OwnerID: "ou_owner"}); err != nil {
				t.Fatal(err)
			}
			menu := openMenu(t, b, "/commands")
			token, _ := openDraft(t, b, menu, "show")
			if err := b.store.Bind("chat-1", state.Binding{ThreadID: "thread-b", OwnerID: "ou_owner"}); err != nil {
				t.Fatal(err)
			}
			text, expected := "继续处理这个任务", "thread-a"
			if explicit {
				text, expected = "#2 继续处理这个任务", "thread-b"
			}
			b.active = &task{chat: "chat-1", owner: "ou_owner", thread: expected, turn: "turn-fixture"}
			var actual string
			rpc.call = func(method string, params any) (json.RawMessage, error) {
				if method != "turn/steer" {
					t.Fatalf("unexpected call: %s", method)
				}
				actual = params.(map[string]any)["threadId"].(string)
				return json.RawMessage(`{}`), nil
			}
			if result := b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: text}); result.Error {
				t.Fatal(result.Message)
			}
			submitted := <-b.in
			if submitted.ThreadID != "thread-a" || !submitted.Explicit {
				t.Fatal("editable draft did not capture its visible target")
			}
			b.handle(context.Background(), submitted)
			if actual != expected || len(b.queue) != 0 {
				t.Fatal("edited draft followed changed default or ignored explicit target")
			}
			if binding, _ := b.store.Binding("chat-1"); binding.ThreadID != "thread-b" {
				t.Fatal("draft submission changed the selected default")
			}
		})
	}
}

func TestDraftRejectsOtherOwnerChatExpiredAndForgedActions(t *testing.T) {
	b, rpc := setup(t)
	menu := openMenu(t, b, "/commands")
	token, _ := openDraft(t, b, menu, "new")
	valid := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/new"}
	cases := []CardAction{valid, valid, valid, valid, valid, valid}
	cases[0].UserID = "ou_other"
	cases[1].ChatID = "chat-other"
	cases[2].Token = "forged"
	cases[3].Token = menu // A menu cannot be submitted as a draft.
	cases[4].Text = " "
	cases[5].Text = strings.Repeat("字", 1001)
	for _, action := range cases {
		if !b.HandleCard(action).Error {
			t.Fatal("invalid callback accepted")
		}
	}
	grant := b.drafts[token]
	grant.expires = time.Now().Add(-time.Second)
	b.drafts[token] = grant
	if !b.HandleCard(valid).Error {
		t.Fatal("expired draft accepted")
	}
	b.drafts = nil // Restart does not resurrect interactive UI grants.
	if !b.HandleCard(valid).Error || len(b.in) != 0 || len(rpc.calls) != 0 {
		t.Fatal("restart revived a stale action")
	}
}

func TestFullInboxKeepsDraftRetryable(t *testing.T) {
	b, _ := setup(t)
	menu := openMenu(t, b, "/commands")
	token, _ := openDraft(t, b, menu, "status")
	for len(b.in) < cap(b.in) {
		b.in <- input("filled", "waiting")
	}
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/status"}
	if !b.HandleCard(action).Error || b.drafts[token].submitted {
		t.Fatal("full inbox consumed draft")
	}
	<-b.in
	if b.HandleCard(action).Error || !b.drafts[token].submitted {
		t.Fatal("draft could not be retried")
	}
}

func TestDraftBackAllowsChangingCommandAndRejectsOldForm(t *testing.T) {
	b, rpc := setup(t)
	menu := openMenu(t, b, "/commands")
	token, _ := openDraft(t, b, menu, "new")
	deadline := b.drafts[token].expires
	action := CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_back"}
	forged := action
	forged.UserID = "ou_other"
	if !b.HandleCard(forged).Error || b.drafts[token].menu {
		t.Fatal("another owner changed the draft")
	}
	result := b.HandleCard(action)
	if result.Error || !strings.Contains(string(result.Card), "会话列表") || !b.drafts[token].expires.Equal(deadline) {
		t.Fatal("returning lost choices or extended the deadline")
	}
	if !b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: token, Action: "draft_send", Text: "/new"}).Error {
		t.Fatal("old form submitted after returning to choices")
	}
	next, card := openDraft(t, b, token, "status")
	if !strings.Contains(string(card.Card), `"default_value":"/status"`) || !b.drafts[next].expires.Equal(deadline) || len(b.in) != 0 || len(rpc.calls) != 0 {
		t.Fatal("reselection executed work, changed the deadline or lost the new command")
	}
	if b.HandleCard(CardAction{ChatID: "chat-1", UserID: "ou_owner", Token: next, Action: "draft_send", Text: "/status"}).Error || len(b.in) != 1 {
		t.Fatal("reselected draft could not be submitted")
	}
	action.Token = next
	if !b.HandleCard(action).Error || len(b.in) != 1 {
		t.Fatal("submitted command could be reopened or duplicated")
	}
}

type cardSender struct {
	cards int
	texts int
}

func (s *cardSender) Send(context.Context, string, string, string) error {
	s.texts++
	return nil
}
func (s *cardSender) SendCardRouted(context.Context, string, json.RawMessage, string) ([]string, error) {
	s.cards++
	return []string{"menu-message"}, nil
}

func TestCommandCardUsesDurableDeliveryAndReplyRouting(t *testing.T) {
	b, _ := setup(t)
	if _, err := b.store.RememberSession(state.Session{ThreadID: "thread-a", OwnerID: "ou_owner", Cwd: b.cfg.ProjectDir}); err != nil {
		t.Fatal(err)
	}
	openMenu(t, b, "/commands #1")
	item := <-b.out
	sender := &cardSender{}
	b.sender = sender
	if len(item.Card) == 0 || !b.deliverOne(context.Background(), item) || sender.cards != 1 || sender.texts != 0 {
		t.Fatal("card was not sent through the persistent outbox")
	}
	if route, ok := b.store.Route("menu-message", "chat-1", "ou_owner"); !ok || route.ThreadID != "thread-a" {
		t.Fatal("card reply routing lost")
	}
	if !b.deliverOne(context.Background(), item) || sender.cards != 1 {
		t.Fatal("acknowledged card delivered twice")
	}
}
