package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type fixtureCardRejection struct{ rejected bool }

func (fixtureCardRejection) Error() string               { return "fixture delivery error" }
func (e fixtureCardRejection) CardContentRejected() bool { return e.rejected }

type fallbackSender struct {
	cards, texts int
	card         func(context.Context) ([]string, error)
	text         func(context.Context, string, string, string) ([]string, error)
}

func (s *fallbackSender) Send(context.Context, string, string, string) error {
	return errors.New("unrouted send must not be used")
}
func (s *fallbackSender) SendCardRouted(ctx context.Context, _ string, _ json.RawMessage, _ string) ([]string, error) {
	s.cards++
	if s.card != nil {
		return s.card(ctx)
	}
	return nil, fixtureCardRejection{true}
}
func (s *fallbackSender) SendRouted(ctx context.Context, chat, text, key string) ([]string, error) {
	s.texts++
	if s.text != nil {
		return s.text(ctx, chat, text, key)
	}
	return []string{"fixture-text"}, nil
}

func readFixtureOutbox(t *testing.T, b *Bridge, item outbound) outbound {
	t.Helper()
	data, err := os.ReadFile(b.outboxPath(item))
	var saved outbound
	if err != nil || json.Unmarshal(data, &saved) != nil {
		t.Fatal("cannot read synthetic outbox")
	}
	return saved
}

func TestRequestFallbackPersistsBeforeSendAndRetriesFromDurableMode(t *testing.T) {
	for _, sample := range []struct{ method, fields, command string }{
		{"item/commandExecution/requestApproval", `"command":"fixture-command"`, "/approve"},
		{"item/tool/requestUserInput", `"questions":[{"id":"q","question":"fixture-question"}]`, "/answer"},
	} {
		t.Run(sample.command, func(t *testing.T) {
			b, rpc := setup(t)
			token, _, item := requestMenu(t, b, sample.method, sample.fields)
			s := &fallbackSender{}
			b.sender = s
			var firstKey, firstText string
			s.text = func(ctx context.Context, chat, text, key string) ([]string, error) {
				saved := readFixtureOutbox(t, b, item)
				deadline, ok := ctx.Deadline()
				if !saved.TextFallback || saved.RequestToken != token || saved.Text != item.Text || saved.Key != item.Key || chat != item.Chat || key != item.Key+":text-fallback" || !strings.Contains(text, item.Text) || !strings.Contains(text, "尚未允许任何操作") || !ok || deadline.After(b.pending[token].created.Add(10*time.Minute)) {
					t.Fatal("fallback lost persistence, original request, deadline or distinct deduplication key")
				}
				if firstKey == "" {
					firstKey, firstText = key, text
					return nil, errors.New("fixture text offline")
				}
				if key != firstKey || text != firstText {
					t.Fatal("retry changed text or deduplication key")
				}
				return []string{"fixture-text"}, nil
			}
			if b.deliverOne(context.Background(), item) || !readFixtureOutbox(t, b, item).TextFallback {
				t.Fatal("failed text delivery lost its durable mode")
			}
			// Deliberately pass the pre-transition channel entry, not the updated copy.
			if !b.deliverOne(context.Background(), item) || s.cards != 1 || s.texts != 2 {
				t.Fatal("stale queued retry resent a rejected card")
			}
			b.deliverOne(context.Background(), item)
			if s.texts != 2 || len(rpc.responses) != 0 || len(rpc.calls) != 0 || len(b.pending) != 1 {
				t.Fatal("fallback replayed delivery or made a decision")
			}
			if route, ok := b.store.Route("fixture-text", item.Chat, item.OwnerID); !ok || route.ThreadID != item.ThreadID {
				t.Fatal("fallback did not retain the exact reply route")
			}
			if _, ok := b.store.Route("fixture-text", item.Chat, "ou_other"); ok {
				t.Fatal("fallback route accepted another owner")
			}
			snapshot := b.Diagnostics().Snapshot()
			if snapshot.Counts["request_delivery"]["failed"] != 1 || snapshot.Counts["request_delivery"]["delivered"] != 0 || snapshot.Counts["request_text_fallback"]["started"] != 1 || snapshot.Counts["request_text_fallback"]["failed"] != 1 || snapshot.Counts["request_text_fallback"]["delivered"] != 1 {
				t.Fatal("fallback text counted as a delivered interaction card")
			}
			// The same account/chat and one-shot manual response rules still apply.
			rest := token
			if sample.command == "/answer" {
				rest += " fixture-answer"
			}
			other := input("wrong-owner", "")
			other.UserID = "ou_other"
			b.answer(other, sample.command, rest)
			if len(rpc.responses) != 0 {
				t.Fatal("another owner answered the fallback")
			}
			b.answer(input("manual-reply", ""), sample.command, rest)
			b.answer(input("manual-replay", ""), sample.command, rest)
			if len(rpc.responses) != 1 || len(b.pending) != 0 {
				t.Fatal("manual fallback response was lost or replayed")
			}
		})
	}
}

func TestRequestFallbackFailsClosedWhenTransportSelectionCannotPersist(t *testing.T) {
	b, _ := setup(t)
	_, _, item := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"fixture"`)
	s := &fallbackSender{card: func(context.Context) ([]string, error) {
		path := b.outboxPath(item)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		return nil, fixtureCardRejection{true}
	}}
	b.sender = s
	if b.deliverOne(context.Background(), item) || s.texts != 0 {
		t.Fatal("text sent before a successful durable transition")
	}
	select {
	case <-b.fatal:
	default:
		t.Fatal("failed durable transition did not fail closed")
	}
}

func TestFallbackForbiddenForUncertainPartialDemoAndCanceledDelivery(t *testing.T) {
	for _, name := range []string{"ordinary_error", "not_rejected", "partial", "demo", "canceled_before", "canceled_during", "resolved_during", "expired_during"} {
		t.Run(name, func(t *testing.T) {
			b, rpc := setup(t)
			token, _, item := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"fixture"`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &fallbackSender{}
			b.sender = s
			s.card = func(context.Context) ([]string, error) {
				switch name {
				case "ordinary_error":
					return nil, errors.New("fixture uncertain transport")
				case "not_rejected":
					return nil, fixtureCardRejection{false}
				case "partial":
					return []string{"fixture-partial"}, fixtureCardRejection{true}
				case "canceled_during":
					cancel()
				case "resolved_during":
					b.event(notification("serverRequest/resolved", `{"threadId":"thread-1","requestId":77}`))
				case "expired_during":
					p := b.pending[token]
					p.created = time.Now().Add(-11 * time.Minute)
					b.pending[token] = p
				}
				return nil, fixtureCardRejection{true}
			}
			if name == "demo" {
				item.Interaction, item.RequestToken = "demo", ""
				if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
					t.Fatal(err)
				}
			}
			if name == "canceled_before" {
				cancel()
			}
			b.deliverOne(ctx, item)
			if s.texts != 0 || len(rpc.responses) != 0 || len(rpc.calls) != 0 {
				t.Fatal("forbidden fallback or automatic decision")
			}
			if name == "canceled_before" && s.cards != 0 {
				t.Fatal("sent after cancellation")
			}
			if name == "partial" {
				if _, ok := b.store.Route("fixture-partial", item.Chat, item.OwnerID); !ok {
					t.Fatal("partial delivery route lost")
				}
			}
			if _, err := os.Stat(b.outboxPath(item)); err == nil && readFixtureOutbox(t, b, item).TextFallback {
				t.Fatal("forbidden fallback persisted")
			}
		})
	}
}

func TestStaleRequestPromptsRetireWithoutSendingOrReplayingRPC(t *testing.T) {
	for _, name := range []string{"expired", "resolved", "turn_completed", "stopping", "wrong_route", "restart", "legacy_without_token", "expired_text_retry"} {
		t.Run(name, func(t *testing.T) {
			b, rpc := setup(t)
			token, _, item := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"fixture"`)
			s := &fallbackSender{}
			b.sender = s
			switch name {
			case "expired", "expired_text_retry":
				p := b.pending[token]
				p.created = time.Now().Add(-11 * time.Minute)
				b.pending[token] = p
				if name == "expired_text_retry" {
					item.TextFallback = true
					if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
						t.Fatal(err)
					}
				}
			case "resolved":
				b.event(notification("serverRequest/resolved", `{"threadId":"thread-1","requestId":77}`))
			case "turn_completed":
				b.active = nil
			case "stopping":
				b.active.stopping = true
			case "wrong_route":
				item.OwnerID = "ou_other"
				if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
					t.Fatal(err)
				}
			case "legacy_without_token":
				item.RequestToken = ""
				if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
					t.Fatal(err)
				}
			case "restart":
				s.text = func(context.Context, string, string, string) ([]string, error) {
					return nil, errors.New("fixture offline")
				}
				b.deliverOne(context.Background(), item)
				if !readFixtureOutbox(t, b, item).TextFallback {
					t.Fatal("no fallback saved for restart fixture")
				}
				b = New(b.cfg, rpc, s, b.store, b.log, b.renderer)
				s.cards, s.texts = 0, 0
			}
			b.retryOutbox(context.Background())
			if s.cards != 0 || s.texts != 0 || len(rpc.calls) != 0 || len(rpc.responses) != 0 {
				t.Fatal("stale request was sent or resumed")
			}
			if _, err := os.Stat(b.outboxPath(item)); !os.IsNotExist(err) {
				t.Fatal("stale prompt was left to retry")
			}
		})
	}
}

func TestFallbackHonorsUnchangedOriginalExpiryOnManualAnswer(t *testing.T) {
	b, rpc := setup(t)
	token, _, item := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"fixture"`)
	b.sender = &fallbackSender{}
	if !b.deliverOne(context.Background(), item) {
		t.Fatal("fallback delivery failed")
	}
	p := b.pending[token]
	p.created = time.Now().Add(-11 * time.Minute)
	b.pending[token] = p
	b.answer(input("late-manual", ""), "/approve", token)
	body, _ := json.Marshal(rpc.responseResults)
	if string(body) != `[{"decision":"decline"}]` {
		t.Fatal("fallback extended approval expiry")
	}
}

func TestRequestResolutionDuringBlockedCardSendDoesNotWaitOnDelivery(t *testing.T) {
	b, rpc := setup(t)
	_, _, item := requestMenu(t, b, "item/fileChange/requestApproval", `"reason":"fixture"`)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	s := &fallbackSender{card: func(ctx context.Context) ([]string, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, fixtureCardRejection{true}
	}}
	b.sender = s
	go func() { done <- b.deliverOne(context.Background(), item) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("card transport did not start")
	}
	b.event(notification("serverRequest/resolved", `{"threadId":"thread-1","requestId":77}`))
	close(release)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("resolved prompt not retired")
		}
	case <-time.After(time.Second):
		t.Fatal("request resolution blocked on transport")
	}
	if s.texts != 0 || len(b.pending) != 0 || len(rpc.responses) != 0 {
		t.Fatal("resolved request fell back or sent a decision")
	}
}
