package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/diagnostics"
	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type RPC interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Respond(json.RawMessage, any) error
	Reject(json.RawMessage) error
	Events() <-chan codex.Message
	Done() <-chan struct{}
}

type Messenger interface {
	Send(context.Context, string, string, string) error
}

type Input struct {
	ChatID, UserID, MessageID, Text string
	Attachment                      *Attachment `json:",omitempty"`
	ReplyTo                         string
	ReplyRoot                       string
	SourceCardID                    string `json:",omitempty"` // Set only by an authenticated manual card submission.
	ThreadID                        string
	Explicit                        bool
	selection                       uint64
	queued                          bool
	demoReply                       bool // Internal-only, never accepted from text or callback values.
}

type task struct {
	resultStatus              string
	modelSettings             modelsettings.Effective
	chat, owner, thread, turn string
	started                   time.Time
	stopping                  bool
	stopAt                    time.Time
	handoff                   bool
	final                     []string
	finalBytes                int
	inputs                    []string
	previews                  map[string]string
}

type question struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	IsSecret bool   `json:"isSecret"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

type pending struct {
	id                                json.RawMessage
	method, chat, owner, thread, turn string
	questions                         []question
	created                           time.Time
}

type Bridge struct {
	conversationMu     sync.Mutex
	conversationInputs map[string]time.Time
	renderer           presentation.Renderer
	models             *modelsettings.Service
	cfg                config.Config
	rpc                RPC
	sender             Messenger
	store              Repository
	log                *slog.Logger
	mu                 sync.Mutex
	active             *task
	replyStatus        atomic.Pointer[replyStatus]
	pending            map[string]pending
	pairCode           string
	pairUntil          time.Time
	in                 chan Input
	out                chan outbound
	fatal              chan error
	failed             atomic.Bool
	maintenanceMu      sync.Mutex
	operations         int
	draining           bool
	recoveryOnce       sync.Once
	recoveryErr        error
	jobs               sync.WaitGroup
	queue              []queued
	wake               chan struct{}
	selection          map[string]uint64
	pages              map[string]sessionPage
	loaded             map[string]bool // Threads loaded in this App Server process only.
	draftMu            sync.Mutex
	inboxMu            sync.Mutex
	fileMu             sync.Mutex
	receivedFiles      []receivedFile
	drafts             map[string]commandDraft // Ephemeral UI grants, never session state.
	closedDrafts       map[string]closedDraft
	transfers          map[string]bool
	diagnostics        diagnostics.Collector
}

func (b *Bridge) Diagnostics() *diagnostics.Collector { return &b.diagnostics }

type outbound struct {
	Chat                 string          `json:"chat_id"`
	Text                 string          `json:"text"`
	Key                  string          `json:"delivery_id"`
	Created              int64           `json:"created_at"`
	ThreadID             string          `json:"thread_id,omitempty"`
	OwnerID              string          `json:"owner_id,omitempty"`
	Format               string          `json:"format,omitempty"`
	Card                 json.RawMessage `json:"card,omitempty"`
	Interaction          string          `json:"interaction,omitempty"` // Fixed purpose enum, never a request ID.
	TextFallback         bool            `json:"text_fallback,omitempty"`
	RequestToken         string          `json:"request_token,omitempty"` // Private outbox only; never diagnostics.
	FilePath             string          `json:"file_path,omitempty"`
	FileName             string          `json:"file_name,omitempty"`
	FileDigest           string          `json:"file_digest,omitempty"`
	ConversationRoot     string          `json:"conversation_root,omitempty"`
	DeliveredIDs         []string        `json:"delivered_ids,omitempty"`
	ConversationIDs      []string        `json:"conversation_ids,omitempty"`
	ConversationEntryIDs []string        `json:"conversation_entry_ids,omitempty"`
	Transcript           bool            `json:"conversation_transcript,omitempty"`
}

func New(c config.Config, rpc RPC, sender Messenger, store Repository, log *slog.Logger, renderer presentation.Renderer) *Bridge {
	if renderer == nil {
		renderer = presentation.Text{}
	}
	b := &Bridge{renderer: renderer, cfg: c, rpc: rpc, sender: sender, store: store, log: log, pending: map[string]pending{}, in: make(chan Input, 64), out: make(chan outbound, 256), fatal: make(chan error, 1), wake: make(chan struct{}, 1), selection: map[string]uint64{}, pages: map[string]sessionPage{}, loaded: map[string]bool{}}
	b.models = modelsettings.New(store, modelsettings.CatalogFunc(func(ctx context.Context) ([]modelsettings.Model, error) { return codex.ListModels(ctx, b.rpcCall) }), func() string { return b.cfg.Model })
	return b
}

func randomToken(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Pairing must be explicitly enabled at startup, expires in ten minutes, and can
// bind just one owner. It is never first-message-wins enrollment.
func (b *Bridge) EnablePairing() (string, error) {
	if len(b.cfg.AllowedOpenIDs) != 0 || b.store.PairedID() != "" {
		return "", errors.New("owner already configured; pairing is disabled")
	}
	code, err := randomToken(16)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.pairCode = code
	b.pairUntil = time.Now().Add(10 * time.Minute)
	b.mu.Unlock()
	return code, nil
}

func (b *Bridge) authorized(id string) bool {
	if len(b.cfg.AllowedOpenIDs) > 0 {
		for _, allowed := range b.cfg.AllowedOpenIDs {
			if id == allowed {
				return true
			}
		}
		return false
	}
	return id != "" && id == b.store.PairedID()
}

// Submit only enqueues; no model work or network request runs in the Feishu
// callback. A full queue returns an error so Feishu may retry the event.
func (b *Bridge) Submit(input Input) error {
	if !b.beginOperation() {
		return errors.New("bridge is stopping for maintenance; retry after reconnection")
	}
	defer b.endOperation()
	if input.ChatID == "" || input.UserID == "" || input.MessageID == "" {
		return nil
	}
	if !b.authorized(input.UserID) {
		b.mu.Lock()
		canPair := b.pairCode != "" && time.Now().Before(b.pairUntil) && strings.HasPrefix(input.Text, "/pair ")
		b.mu.Unlock()
		if !canPair {
			return nil
		}
	}
	if len(input.Text) > 32000 {
		input.Text = "/input-too-long"
	}
	if b.authorized(input.UserID) {
		retained, err := b.retainInput(input)
		if err != nil {
			return errors.New("cannot durably receive input")
		}
		if !retained {
			return nil
		}
	}
	select {
	case b.in <- input:
		return nil
	default:
		return errors.New("bridge input queue full")
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); b.jobs.Wait() }()
	if err := b.Recover(); err != nil {
		return err
	}
	b.jobs.Add(1)
	go func() { defer b.jobs.Done(); b.deliver(ctx) }()
	work, control := make(chan Input, 64), make(chan Input, 64)
	for _, lane := range []chan Input{work, control} {
		b.jobs.Add(1)
		go func(lane <-chan Input) {
			defer b.jobs.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case input := <-lane:
					if ctx.Err() != nil {
						return
					}
					b.handle(ctx, input)
					b.endOperation()
				}
			}
		}(lane)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.rpc.Done():
			return errors.New("Codex App Server disconnected; restart bridge to resume saved sessions")
		case err := <-b.fatal:
			return err
		case input := <-b.in:
			if !b.beginOperation() {
				continue
			}
			lane := work
			if controlInput(input) {
				lane = control
			}
			select {
			case lane <- input:
			default:
				b.endOperation()
				b.respond(input, "接收队列繁忙，本条尚未执行，请稍后重发。")
				b.markInput(input.MessageID, "handled")
			}
		case <-b.wake:
			b.startNext(ctx)
		case message := <-b.rpc.Events():
			b.event(message)
		case <-tick.C:
			b.expire(ctx)
		}
	}
}

func (b *Bridge) fail(err error) {
	b.failed.Store(true)
	select {
	case b.fatal <- err:
	default:
	}
}

func (b *Bridge) emit(chat, text string, threadIDs ...string) {
	b.emitPayload(chat, text, nil, threadIDs...)
}

func (b *Bridge) emitPayload(chat, text string, card json.RawMessage, threadIDs ...string) {
	b.emitInteraction(chat, text, card, "", "", threadIDs...)
}

func (b *Bridge) emitInteraction(chat, text string, card json.RawMessage, purpose, requestToken string, threadIDs ...string) bool {
	var session state.Session
	if len(threadIDs) > 0 && threadIDs[0] != "" {
		session, _ = b.store.Session(threadIDs[0])
		if session.Ref > 0 {
			text = fmt.Sprintf("[会话 #%d]\n简述：%s\n%s\n\n%s", session.Ref, b.sessionLabel(session), b.sessionDisplay(session), text)
		}
	}
	key, err := randomToken(16)
	if err != nil {
		b.fail(errors.New("cannot generate delivery id"))
		return false
	}
	if b.cfg.AppSecret != "" {
		text = strings.ReplaceAll(text, b.cfg.AppSecret, "[REDACTED]")
	}
	if len(text) > 64000 {
		path := filepath.Join(b.cfg.StateDir, "result-"+key+".json")
		if err := state.WritePrivate(path, resultRecord{Chat: chat, Owner: session.OwnerID, Thread: session.ThreadID, Text: text, Name: "完整结果.txt", Created: time.Now().Unix()}); err != nil {
			b.fail(errors.New("cannot save long result"))
			return false
		}
		end := 64000
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end] + "\n（结果较长，发送 /result " + key + " 取回完整附件；/files 查看本会话成果。）"
	}
	item := outbound{Format: b.renderer.Format(), Chat: chat, Text: text, Card: card, Key: key, Created: time.Now().UnixNano(), ThreadID: session.ThreadID, OwnerID: session.OwnerID, Interaction: purpose}
	if purpose == "request" {
		// request() holds b.mu and supplies the token for this exact prompt.
		request := b.pending[requestToken]
		item.RequestToken, item.ThreadID, item.OwnerID = requestToken, request.thread, request.owner
	}
	if purpose == "shortcut" {
		item.RequestToken = requestToken
	}
	if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
		if len(card) != 0 {
			b.diagnostics.Record("prompt", "failed")
		}
		b.fail(errors.New("cannot persist outgoing reply"))
		return false
	}
	select {
	case b.out <- item:
		if len(card) != 0 {
			b.diagnostics.Record("prompt", "queued")
		}
		return true
	default:
		if len(card) != 0 {
			b.diagnostics.Record("prompt", "queue_full")
		}
		b.fail(errors.New("outgoing queue full; stop rather than silently losing results"))
		return false
	}
}

func (b *Bridge) deliver(ctx context.Context) {
	b.retryOutbox(ctx)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b.retryOutbox(ctx)
		case item := <-b.out:
			b.deliverOne(ctx, item)
		}
	}
}

func (b *Bridge) outboxPath(item outbound) string {
	return filepath.Join(b.cfg.StateDir, fmt.Sprintf("outbox-%019d-%s.json", item.Created, item.Key))
}

func (b *Bridge) retryOutbox(ctx context.Context) {
	paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, "outbox-*.json"))
	if err != nil {
		b.fail(errors.New("cannot list outgoing replies"))
		return
	}
	sort.Strings(paths)
	for _, path := range paths {
		if ctx.Err() != nil {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			b.fail(errors.New("cannot read outgoing reply"))
			return
		}
		var item outbound
		if json.Unmarshal(data, &item) != nil || b.outboxPath(item) != path {
			b.fail(errors.New("invalid outgoing reply state"))
			return
		}
		if !b.deliverOne(ctx, item) {
			return
		}
	}
}

func (b *Bridge) deliverOne(ctx context.Context, item outbound) bool {
	if !b.beginOperation() {
		return false
	}
	defer b.endOperation()
	path := b.outboxPath(item)
	data, readErr := os.ReadFile(path)
	if os.IsNotExist(readErr) {
		return true
	}
	if readErr != nil {
		b.fail(errors.New("cannot read outgoing reply"))
		return false
	}
	var saved outbound
	if json.Unmarshal(data, &saved) != nil || b.outboxPath(saved) != path {
		b.fail(errors.New("invalid outgoing reply state"))
		return false
	}
	// A channel entry can predate a retry that persisted text fallback. The
	// durable copy is authoritative, including after restart and queued retry.
	item = saved
	// Old releases persisted Feishu cards without a format marker. Never send a
	// different frontend's opaque payload; keep the durable text fallback.
	format := item.Format
	if format == "" {
		format = "feishu-card-v1"
	}
	if len(item.Card) > 0 && format != b.renderer.Format() {
		item.TextFallback = true
	}
	if item.Interaction == "shortcut" && !item.TextFallback {
		b.draftMu.Lock()
		_, live := b.drafts[item.RequestToken]
		b.draftMu.Unlock()
		if !live {
			item.TextFallback = true
			if state.WritePrivate(path, item) != nil {
				b.fail(errors.New("cannot retire stale shortcut card"))
				return false
			}
		}
	}
	stage := "text_delivery"
	if len(item.Card) != 0 && !item.TextFallback {
		stage = "card_delivery"
	}
	record := func(outcome string) {
		b.diagnostics.Record(stage, outcome)
		if stage == "card_delivery" {
			switch item.Interaction {
			case "demo":
				b.diagnostics.Record("demo_delivery", outcome)
			case "request":
				b.diagnostics.Record("request_delivery", outcome)
			}
		}
		if item.TextFallback && item.Interaction == "request" {
			b.diagnostics.Record("request_text_fallback", outcome)
		}
	}
	deadline, live := b.requestDeliveryDeadline(item)
	if !live {
		record("expired")
		return b.acknowledgeOutbox(path)
	}
	if ctx.Err() != nil {
		return false
	}
	// Feishu de-duplicates UUIDs for one hour. Retain uncertain older deliveries
	// for manual inspection rather than risk sending a duplicate after that window.
	if time.Since(time.Unix(0, item.Created)) > 50*time.Minute {
		record("expired")
		if err := os.Rename(path, filepath.Join(b.cfg.StateDir, "undelivered-"+item.Key+".json")); err != nil && !os.IsNotExist(err) {
			b.fail(errors.New("cannot retain expired outgoing reply"))
		}
		b.log.Warn("消息超过自动重试时限，内容已保留供人工查看", "delivery_id", item.Key)
		return true
	}
	callDeadline := time.Now().Add(45 * time.Second)
	if !deadline.IsZero() && deadline.Before(callDeadline) {
		callDeadline = deadline
	}
	callCtx, cancel := context.WithDeadline(ctx, callDeadline)
	var err error
	item.ConversationRoot, err = b.conversationRoot(callCtx, item)
	if err != nil {
		cancel()
		record("failed")
		return false
	}
	var ids []string
	if item.Transcript {
		// Main-chat user input already exists; only its attributed topic copy is sent.
		ids = []string{}
	} else if len(item.DeliveredIDs) > 0 {
		ids = item.DeliveredIDs
	} else if item.FilePath != "" {
		ids, err = b.sendFile(callCtx, item)
	} else if len(item.Card) != 0 && !item.TextFallback {
		if sender, ok := b.sender.(RichMessenger); ok {
			ids, err = sender.SendCardRouted(callCtx, item.Chat, item.Card, item.Key)
		} else {
			stage = "text_delivery" // Text fallback is not evidence of a delivered card.
			err = b.sender.Send(callCtx, item.Chat, item.Text, item.Key)
		}
	} else {
		ids, err = b.sendText(callCtx, item)
	}
	var rejected interface{ CardContentRejected() bool }
	if err != nil && len(ids) == 0 && stage == "card_delivery" && (item.Interaction == "request" || item.Interaction == "shortcut") && !item.TextFallback && callCtx.Err() == nil && errors.As(err, &rejected) && rejected.CardContentRejected() {
		record("failed")
		if _, live := b.requestDeliveryDeadline(item); !live {
			cancel()
			return b.acknowledgeOutbox(path)
		}
		// Persist the selected transport before sending, so a restart or failed
		// text attempt cannot return to the rejected card. Keep the original
		// prompt and use a distinct, stable deduplication key for the text.
		item.TextFallback = true
		if state.WritePrivate(path, item) != nil {
			if item.Interaction == "request" {
				b.diagnostics.Record("request_text_fallback", "failed")
			}
			b.fail(errors.New("cannot persist approval text fallback"))
			cancel()
			return false
		}
		stage = "text_delivery"
		if item.Interaction == "request" {
			b.diagnostics.Record("request_text_fallback", "started")
		}
		ids, err = b.sendText(callCtx, item)
	}
	if err == nil && item.ConversationRoot != "" {
		item.DeliveredIDs = ids
		if state.WritePrivate(path, item) != nil {
			b.fail(errors.New("cannot persist main delivery receipt"))
			cancel()
			return false
		}
		copies := item.ConversationIDs
		if len(copies) == 0 {
			if item.FilePath != "" {
				copies, err = b.sendConversationFile(callCtx, item)
			} else {
				copies, err = b.sender.(ConversationMessenger).SendConversationText(callCtx, item.Chat, item.ConversationRoot, item.Text, item.Key+":conversation")
			}
			if err == nil {
				item.ConversationIDs = copies
				if state.WritePrivate(path, item) != nil {
					b.fail(errors.New("cannot persist conversation delivery receipt"))
					cancel()
					return false
				}
			}
		}
		ids = append(append([]string(nil), ids...), copies...)
		// Forward the native topic only after the current reply is inside it.
		// User transcript copies do not create an extra main-chat notification.
		if err == nil && !item.Transcript {
			entries := item.ConversationEntryIDs
			if len(entries) == 0 {
				entries, err = b.sender.(ConversationMessenger).SendConversationEntry(callCtx, item.Chat, item.ConversationRoot, item.Key+":conversation-entry")
				if err == nil {
					item.ConversationEntryIDs = entries
					if state.WritePrivate(path, item) != nil {
						b.fail(errors.New("cannot persist conversation entry receipt"))
						cancel()
						return false
					}
				}
			}
			ids = append(ids, entries...)
		}
	}
	if len(ids) > 0 && item.ThreadID != "" && item.OwnerID != "" {
		if routeErr := b.store.RememberRoutes(ids, state.MessageRoute{ChatID: item.Chat, OwnerID: item.OwnerID, ThreadID: item.ThreadID, Created: time.Now().Unix()}); routeErr != nil {
			b.fail(errors.New("cannot persist reply routing"))
			cancel()
			return false
		}
	}
	cancel()
	if err != nil {
		record("failed")
		if ctx.Err() == nil {
			b.log.Warn("飞书发送失败，消息已落盘并将重试", "delivery_id", item.Key)
		}
		return false
	}
	record("delivered")
	acknowledged := b.acknowledgeOutbox(path)
	if acknowledged && item.FilePath != "" {
		_ = os.Remove(item.FilePath)
	}
	return acknowledged
}

// Pending RPCs are process-local. Retire stale prompts after resolution, expiry
// or restart, rather than send an entry that can no longer accept a decision.
func (b *Bridge) requestDeliveryDeadline(item outbound) (time.Time, bool) {
	if item.Interaction != "request" {
		return time.Time{}, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[item.RequestToken]
	a := b.active
	deadline := p.created.Add(10 * time.Minute)
	return deadline, ok && a != nil && !a.stopping && p.chat == item.Chat && p.owner == item.OwnerID &&
		p.thread == item.ThreadID && a.chat == p.chat && a.owner == p.owner && a.thread == p.thread && a.turn == p.turn && time.Now().Before(deadline)
}

func (b *Bridge) acknowledgeOutbox(path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		b.fail(errors.New("cannot acknowledge outgoing reply"))
		return false
	}
	return true
}

func (b *Bridge) sendText(ctx context.Context, item outbound) ([]string, error) {
	text, key := item.Text, item.Key
	if item.TextFallback {
		prefix := "快捷卡片暂不可用，以下是原消息；可手动输入 /commands 重新生成入口。"
		if item.Interaction == "request" {
			prefix = "交互卡片未能发送，以下是文字审批/回答备用入口。请核对原请求后手动发送对应命令；尚未允许任何操作。"
		}
		text = prefix + "\n\n" + text
		key += ":text-fallback"
	}
	if sender, ok := b.sender.(RoutedMessenger); ok {
		return sender.SendRouted(ctx, item.Chat, text, key)
	}
	return nil, b.sender.Send(ctx, item.Chat, text, key)
}

var help = `多会话使用：引用带 [会话 #编号] 的机器人消息，即可继续该会话，不必先切换。
也可发送 #编号 内容；不引用、不加编号的普通消息才使用默认会话。
结果、进度和审批显示会话、项目简名、状态与模型配置；/show 查看完整目录。

` + commandset.Help() + `

会话命令可引用回复，也可显式指定 #编号。
目标优先级：显式编号 > 引用消息 > 默认会话。未知引用不会转给默认会话。
外部原始会话只读；/fork 可在授权项目范围内复制接续。
/commands 点击命令生成可修改草稿，按发送才执行。`

func (b *Bridge) handle(ctx context.Context, input Input) {
	if !b.beginOperation() {
		return
	}
	defer b.endOperation()
	if !b.authorized(input.UserID) {
		b.mu.Lock()
		code := strings.TrimSpace(strings.TrimPrefix(input.Text, "/pair "))
		valid := b.pairCode != "" && time.Now().Before(b.pairUntil) && subtle.ConstantTimeCompare([]byte(code), []byte(b.pairCode)) == 1
		if valid {
			if err := b.store.Pair(input.UserID); err != nil {
				b.mu.Unlock()
				b.fail(errors.New("cannot persist owner pairing"))
				return
			}
			b.pairCode = ""
		}
		b.mu.Unlock()
		if valid {
			b.home(input, "配对成功。")
		}
		return
	}
	claimed, err := b.store.Claim(input.MessageID, time.Now())
	if err != nil {
		b.fail(errors.New("cannot persist message receipt"))
		return
	}
	if !claimed {
		return
	}
	if !b.markInput(input.MessageID, "handling") {
		return
	}
	defer b.finishInput(input)
	if input.demoReply {
		b.diagnostics.Record("demo_result", "completed")
		b.emit(input.ChatID, "隔离演示回复已接收，演示结束。没有执行宿主操作，也没有向 Codex 提交真实审批或答案。", input.ThreadID)
		return
	}
	if input.Attachment != nil {
		b.receiveFile(ctx, input)
		return
	}
	text := strings.TrimSpace(input.Text)
	if strings.HasPrefix(text, "#") {
		ref, body := splitWord(text)
		if !b.target(&input, ref) {
			return
		}
		if body == "" {
			b.sessionsCommand(ctx, input, "/show", "")
			return
		}
		text = body
	}
	command, rest := splitWord(text)
	if b.productCommand(ctx, input, command, rest) {
		return
	}
	if b.modelCommand(ctx, input, command, rest) {
		return
	}
	if b.sessionsCommand(ctx, input, command, rest) {
		return
	}
	switch command {
	case "/file":
		b.fileCommand(ctx, input, rest)
	case "/commands":
		b.commands(input, rest)
	case "/start":
		b.home(input)
	case "/help":
		b.respond(input, help)
	case "/approve", "/deny", "/answer":
		b.answer(input, command, strings.TrimSpace(rest))
	case "/pair":
		b.respond(input, "当前账号已授权。")
	case "/input-too-long":
		b.respond(input, "消息过长，请拆分为每条不超过 32000 字节。")
	default:
		if strings.HasPrefix(text, "/") {
			b.respond(input, "未知命令，发送 /help 查看用法。")
			return
		}
		if text != "" {
			if !b.routeInput(&input) {
				return
			}
			b.prompt(ctx, input, text, false)
		}
	}
}

func (b *Bridge) status(ctx context.Context, input Input) {
	b.mu.Lock()
	b.refreshReplyStatusLocked()
	var message string
	if a := b.active; a != nil && (input.ThreadID == "" || input.ThreadID == a.thread) {
		if a.chat != input.ChatID || a.owner != input.UserID {
			b.mu.Unlock()
			b.respond(input, "当前项目已有其他会话在执行任务，请稍后重试。")
			return
		}
		status := b.taskStatusLocked(a)
		message = fmt.Sprintf("状态：%s\n已运行：%s\n会话：%s\n等待回答/审批：%d", status, time.Since(a.started).Round(time.Second), a.thread, len(b.pending))
	} else {
		queued := 0
		for _, q := range b.queue {
			if q.Input.ChatID == input.ChatID && q.Input.UserID == input.UserID && q.Input.ThreadID == input.ThreadID {
				queued++
			}
		}
		message = fmt.Sprintf("此会话未执行任务；排队任务：%d。", queued)
	}
	b.mu.Unlock()
	if input.ThreadID == "" {
		b.respond(input, "空闲。发送文字即可创建会话。")
		return
	}
	b.respondSessionInfo(input, message+"\n\n"+b.sessionModelInfo(ctx, input, true))
}

func (b *Bridge) rpcCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := b.rpc.Call(ctx, method, params)
	if err == nil {
		switch method {
		case "thread/start", "thread/resume", "thread/fork":
			b.observeApprovalSettings(raw)
		}
	}
	return raw, err
}

func (b *Bridge) stop(ctx context.Context, input Input) {
	b.mu.Lock()
	a := b.active
	if a == nil || a.chat != input.ChatID || a.owner != input.UserID || (input.ThreadID != "" && a.thread != input.ThreadID) {
		b.mu.Unlock()
		if input.ThreadID != "" {
			b.cancelQueued(input)
			return
		}
		b.respond(input, "本会话没有执行中的任务。")
		return
	}
	if a.turn == "" {
		b.mu.Unlock()
		b.respond(input, "会话正在初始化，请稍后再次发送 /stop。", a.thread)
		return
	}
	a.stopping = true
	a.stopAt = time.Now()
	b.refreshReplyStatusLocked()
	thread, turn := a.thread, a.turn
	b.mu.Unlock()
	if _, err := b.rpcCall(ctx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": turn}); err != nil {
		b.respond(input, "停止请求未确认，请用 /status 检查，勿假定任务已停止。", a.thread)
		b.fail(errors.New("turn interruption was not acknowledged; inspect Codex session before restarting"))
		return
	}
	b.respond(input, "已请求停止，等待 Codex 确认。", a.thread)
}

const handoffPrompt = `请先收拢当前工作，创建或更新项目的 docs/HANDOFF.md。记录目标、用户约束、已完成事项、正在进行的工作及文件、实际验证结果、阻塞与未验证项，以及新会话可直接执行的步骤。不得写入密钥或令牌。完成后明确说“该切换会话了”，附交接文档路径和接续提示。不要在交接后继续开展新任务。`

const sessionInstructions = "通过已连接前端与用户交流。遵守项目 AGENTS.md。Codex OpenAPI 模型请求须串行执行，不同时发起多个模型请求；本地工具和桥接的消息接收、事件处理、回复发送可正常并发。回复应直接说明当前请求的具体对象、动作和结果；进度消息写清已确认的发现、当前检查项和下一步要核实的内容，不要只说‘执行中’‘修改中’或重复会话介绍。仅在有新信息时更新进度，不编造进展或用空泛确认刷屏。接近上下文容量时提前生成 docs/HANDOFF.md，明确提醒‘该切换会话了’，附接续步骤。不要读取、展示或修改桥接服务的私有配置、凭证和会话状态；不要把敏感数据写入回复或交接文档。"

func (b *Bridge) prompt(ctx context.Context, input Input, text string, handoff bool) {
	if !handoff {
		text = b.fileContext(input, text)
	}
	b.mu.Lock()
	if !input.queued {
		input.selection = b.selection[input.ChatID]
	}
	if a := b.active; a != nil {
		if a.chat != input.ChatID || a.owner != input.UserID || a.thread != input.ThreadID {
			b.mu.Unlock()
			b.enqueue(input, text, handoff)
			return
		}
		if a.turn == "" || a.stopping {
			b.mu.Unlock()
			b.respond(input, "任务正在初始化或停止，请稍后重试。", a.thread)
			return
		}
		if handoff {
			a.handoff = true
		}
		// A timed-out steer can still have reached Codex. Keep its receipt pending
		// until the turn result, or report uncertainty after a restart.
		a.inputs = append(a.inputs, input.MessageID)
		thread, turn := a.thread, a.turn
		b.mu.Unlock()
		_, err := b.rpcCall(ctx, "turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []map[string]string{{"type": "text", "text": text}}})
		if err != nil {
			b.respond(input, "补充指令未确认，请查看 /status 后重新发送。", thread)
			return
		}
		b.respond(input, "已将补充指令交给当前任务。", thread)
		return
	}
	a := &task{chat: input.ChatID, owner: input.UserID, thread: input.ThreadID, started: time.Now(), handoff: handoff, previews: map[string]string{}, inputs: []string{input.MessageID}}
	b.active = a
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	// The model's first substantive reply carries the saved session routing.
	// Do not prepend separate received/connected messages to every user request.
	b.jobs.Add(1)
	b.beginOperation() // The calling operation prevents maintenance acquisition.
	go func() {
		defer b.jobs.Done()
		defer b.endOperation()
		b.start(ctx, input, a, text)
	}()
}

func (b *Bridge) start(ctx context.Context, input Input, a *task, text string) {
	params := b.sessionParams(b.cfg.ProjectDir)
	method := "thread/start"
	var result struct {
		Thread threadSummary `json:"thread"`
	}
	loaded := false
	if input.ThreadID != "" {
		if saved, ok := b.store.Session(input.ThreadID); ok {
			if saved.OriginHome != "" {
				b.startFailed(a, fmt.Sprintf("此外部会话未接管。请先 /fork #%d 创建独立副本后接续。", saved.Ref))
				return
			}
			if saved.OwnerID != "" && saved.OwnerID != input.UserID {
				b.startFailed(a, "会话属于其他账号，未执行。")
				return
			}
			if !b.workspaceAllowed(saved.Cwd) {
				b.startFailed(a, "此会话位于当前授权项目目录之外；可以查看，尚不能从桥接执行任务。")
				return
			}
			if saved.Cwd != "" {
				params["cwd"] = saved.Cwd
			}
			b.mu.Lock()
			loaded = b.loaded[input.ThreadID]
			b.mu.Unlock()
			if loaded {
				result.Thread = threadSummary{ID: saved.ThreadID, Name: saved.Name, Preview: saved.Summary, Cwd: saved.Cwd}
			}
		}
		method = "thread/resume"
		params["threadId"] = input.ThreadID
		params["excludeTurns"] = true
	}
	settings, settingsErr := b.nextModelSettings(ctx, input.ThreadID, input.UserID)
	if settingsErr != nil {
		b.startFailed(a, settingsErr.Error()+" 未启动任务；用 /model 检查设置。")
		return
	}
	params["model"] = settings.Model
	if !loaded {
		raw, err := b.rpcCall(ctx, method, params)
		if err != nil {
			b.startFailed(a, sessionStartError(method, err))
			return
		}
		if json.Unmarshal(raw, &result) != nil || result.Thread.ID == "" {
			b.startFailed(a, "Codex 返回的会话信息无效。")
			return
		}
	}
	if (input.ThreadID != "" && result.Thread.ID != input.ThreadID) || !b.workspaceAllowed(result.Thread.Cwd) {
		b.startFailed(a, "会话 ID 或项目范围不匹配，未启动任务。")
		return
	}
	if result.Thread.Cwd == "" {
		result.Thread.Cwd = b.cfg.ProjectDir
	}
	summary := b.shortSummary(result.Thread.Preview)
	if summary == "" {
		if saved, ok := b.store.Session(result.Thread.ID); ok {
			summary = saved.Summary
		}
	}
	if summary == "" && !a.handoff {
		summary = b.shortSummary(text)
	}
	_, err := b.store.RememberSession(state.Session{ThreadID: result.Thread.ID, OwnerID: input.UserID, Cwd: result.Thread.Cwd, Name: result.Thread.Name, Summary: summary})
	if err != nil {
		b.startFailed(a, "无法保存会话归属，未启动任务。")
		return
	}
	input.ThreadID = result.Thread.ID
	if !b.rememberInputRoute(input) {
		return
	}
	b.mu.Lock()
	keepDefault := !input.Explicit && b.selection[input.ChatID] == input.selection
	b.mu.Unlock()
	if keepDefault {
		if err := b.store.Bind(input.ChatID, state.Binding{ThreadID: result.Thread.ID, OwnerID: input.UserID}); err != nil {
			b.startFailed(a, "无法保存会话绑定，未启动任务。")
			return
		}
	}
	b.mu.Lock()
	b.loaded[result.Thread.ID] = true
	a.thread = result.Thread.ID
	a.modelSettings = settings
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	turnParams := map[string]any{"threadId": result.Thread.ID, "input": []map[string]string{{"type": "text", "text": text}}, "model": settings.Model, "effort": settings.Effort}
	raw, err := b.rpcCall(ctx, "turn/start", turnParams)
	if err != nil {
		// A lost acknowledgement may still have started the task. Stop the bridge
		// instead of clearing active state and possibly starting a second task.
		b.fail(errors.New("turn/start result unknown; inspect saved Codex session before retrying"))
		return
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &started) != nil || started.Turn.ID == "" {
		b.fail(errors.New("invalid turn/start response"))
		return
	}
	b.mu.Lock()
	if b.active == a && a.turn == "" {
		a.turn = started.Turn.ID
	}
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
}

func sessionStartError(method string, err error) string {
	stage := "创建"
	if method == "thread/resume" {
		stage = "恢复"
	}
	detail := "连接失败"
	var rpcErr *codex.RPCError
	if errors.As(err, &rpcErr) {
		detail = fmt.Sprintf("RPC %d", rpcErr.Code)
		var data struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rpcErr.Data, &data) == nil && data.Type == "modelProviderMismatch" {
			return "会话保存的模型提供商与当前配置不一致，未启动任务。请切回原提供商，或用 /fork 创建副本；原绑定已保留。"
		}
		if method == "thread/resume" && rpcErr.Code == -32600 && strings.Contains(strings.ToLower(rpcErr.Message), "no rollout found") {
			return "会话恢复失败：Codex 尚无可恢复的会话记录（RPC -32600）。未启动任务，原绑定已保留。可发送 /new 新建会话后继续；不要删除原会话。"
		}
	} else if errors.Is(err, context.DeadlineExceeded) {
		detail = "请求超时"
	}
	return fmt.Sprintf("Codex 会话%s失败（%s）。未启动任务，原绑定已保留。", stage, detail)
}

func (b *Bridge) startFailed(a *task, message string) {
	b.mu.Lock()
	if b.active == a {
		b.active = nil
	}
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	if b.respond(Input{ChatID: a.chat, UserID: a.owner, ThreadID: a.thread}, message, a.thread) {
		for _, id := range a.inputs {
			b.markInput(id, "handled")
		}
	}
	b.signalNext()
}
