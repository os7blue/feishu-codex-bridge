package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
	"strings"
	"time"
	"unicode/utf8"
)

// Card callbacks do no RPC or network work. Opening a draft only returns UI;
// pressing Send feeds the edited text through the ordinary authorized inbox.
type CardAction = presentation.Action
type CardResult = presentation.Result

type commandDraft struct {
	chat, owner, target, context string
	thread                       string
	expires                      time.Time
	menu, submitted              bool
	demo                         bool
	request                      string
	replies                      []replyChoice
	questionIDs                  []string
	receipt                      json.RawMessage
}

type commandTemplate struct {
	id, label, text string
	scoped          bool
}

type closedDraft struct {
	owner, chat, reason string
	at                  time.Time
}

// Caller holds draftMu. Retain only bounded, short-lived reasons for this owner.
func (b *Bridge) closeDraft(token string, grant commandDraft, reason string) {
	if b.closedDrafts == nil {
		b.closedDrafts = map[string]closedDraft{}
	}
	for key, item := range b.closedDrafts {
		if time.Since(item.at) > time.Hour {
			delete(b.closedDrafts, key)
		}
	}
	if len(b.closedDrafts) >= 128 {
		var oldest string
		var stamp time.Time
		for key, item := range b.closedDrafts {
			if oldest == "" || item.at.Before(stamp) {
				oldest, stamp = key, item.at
			}
		}
		delete(b.closedDrafts, oldest)
	}
	b.closedDrafts[token] = closedDraft{owner: grant.owner, chat: grant.chat, reason: reason, at: time.Now()}
	delete(b.drafts, token)
}

var commandTemplates = []commandTemplate{
	{"projects", "选择项目新建", "/projects", false},
	{"continue", "沿用项目、文档和配置接续", "/continue", true},
	{"files", "附件与成果", "/files", true},
	{"sessions", "会话列表", "/sessions", false},
	{"sessions_next", "列表下一页", "/sessions next", false},
	{"sessions_archived", "已归档列表", "/sessions archived", false},
	{"new", "新建会话", "/new", false},
	{"status", "整体状态", "/status", false},
	{"queue", "任务队列", "/queue", false},
	{"help", "帮助", "/help", false},
	{"models", "可用模型和强度", "/models", false},
	{"global_model", "全局模型默认", "/model global", false},
	{"global_effort", "全局思考强度默认", "/effort global", false},
	{"model", "会话模型设置", "/model", true},
	{"effort", "会话思考强度设置", "/effort", true},
	{"file", "发送文件（填写路径）", "/file", true},
	{"show", "会话信息", "/show", true},
	{"session_status", "会话状态", "/status", true},
	{"history", "最近消息", "/history", true},
	{"use", "设为默认", "/use", true},
	{"rename", "重命名", "/rename", true},
	{"fork", "复制会话", "/fork", true},
	{"stop", "停止任务", "/stop", true},
	{"cancel", "取消排队", "/cancel", true},
	{"handoff", "生成交接", "/handoff", true},
	{"archive", "归档会话", "/archive", true},
	{"unarchive", "恢复归档", "/unarchive", true},
}

func (b *Bridge) submittedCard(grant commandDraft, text string) json.RawMessage {
	context := grant.context
	if b.cfg.AppSecret != "" {
		context = strings.ReplaceAll(context, b.cfg.AppSecret, "[REDACTED]")
		text = strings.ReplaceAll(text, b.cfg.AppSecret, "[REDACTED]")
	}
	return b.render(presentation.View{Kind: "submitted", Title: "回复已入队", Context: context, Text: text}, nil)
}

// Caller holds draftMu. Expired grants and the hard cap bound callback memory.
func (b *Bridge) newDraft(grant commandDraft) (string, bool) {
	now := time.Now()
	for token, item := range b.drafts {
		if !now.Before(item.expires) {
			b.closeDraft(token, item, "已超过有效期，未因此重新执行任何操作。")
		}
	}
	if len(b.drafts) >= 128 {
		return "", false
	}
	token, err := randomToken(16)
	if err != nil {
		return "", false
	}
	if b.drafts == nil {
		b.drafts = make(map[string]commandDraft)
	}
	if grant.expires.IsZero() {
		grant.expires = now.Add(30 * time.Minute)
	}
	b.drafts[token] = grant
	return token, true
}

func (b *Bridge) commands(input Input, rest string) {
	if rest != "" {
		if _, extra := splitWord(rest); extra != "" {
			b.emit(input.ChatID, "用法：/commands #编号；也可引用会话消息发送 /commands。")
			return
		}
		if !b.target(&input, rest) {
			return
		}
	} else if !b.routeInput(&input) {
		return
	}
	target := "#编号"
	context := "尚未指定会话，请在会话命令草稿中填写编号。"
	if session, ok := b.store.Session(input.ThreadID); ok {
		target = fmt.Sprintf("#%d", session.Ref)
		context = b.sessionLine(session.ThreadID)
	}
	b.draftMu.Lock()
	token, ok := b.newDraft(commandDraft{chat: input.ChatID, owner: input.UserID, target: target, thread: input.ThreadID, context: context, menu: true})
	b.draftMu.Unlock()
	if !ok {
		b.emit(input.ChatID, "草稿数量已达上限，请稍后重试；仍可直接发送文字命令。")
		return
	}
	b.emitPayload(input.ChatID, "命令草稿不可用时，可直接发送文字命令。\n"+help, b.menuCard(token, target, context), input.ThreadID)
}

// SendClickDemo queues one real interactive message in the paired owner's chat.
// It creates UI grants only; this method and opening the card do no model work.
func (b *Bridge) SendClickDemo() error {
	if !b.beginOperation() {
		return errors.New("bridge is stopping for maintenance")
	}
	defer b.endOperation()
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := b.store.PairedID()
	chat, ok := b.store.OwnerChat()
	if !ok || owner == "" || !b.authorized(owner) {
		return errors.New("no authorized paired chat for click demo")
	}
	binding, ok := b.store.Binding(chat)
	if !ok || binding.OwnerID != owner {
		return errors.New("click demo chat binding unavailable")
	}
	context := b.sessionLine(binding.ThreadID) + "\n\n点击下方 /status 文字，打开预填命令的草稿。可修改会话编号，手动发送才查询状态。本次测试仅支持 /status，不会运行模型或修改会话。"
	b.draftMu.Lock()
	token, ok := b.newDraft(commandDraft{chat: chat, owner: owner, context: context, menu: true, demo: true})
	b.draftMu.Unlock()
	if !ok {
		return errors.New("click demo grant unavailable")
	}
	card := b.demoCard(token, context)
	if !b.emitInteraction(chat, "点击命令测试：/status；请点击卡片中的命令文字打开草稿。", card, "demo", "", binding.ThreadID) {
		b.diagnostics.Record("demo", "failed")
		return errors.New("click demo queue unavailable")
	}
	b.diagnostics.Record("demo", "queued")
	// Synthetic requests have no pending RPC, host action or real request ID.
	// Their replies travel through the same form/inbox but end locally.
	for _, question := range []bool{false, true} {
		requestToken, err := randomToken(8)
		if err != nil {
			return errors.New("click demo token unavailable")
		}
		grant := commandDraft{chat: chat, owner: owner, thread: binding.ThreadID, menu: true, demo: true, request: requestToken, expires: time.Now().Add(10 * time.Minute)}
		grant.context = b.sessionLine(binding.ThreadID) + "\n\n隔离审批演示：这不是宿主执行授权，不会执行任何命令。\n\n操作说明：展示待审操作，并测试允许、拒绝之间的改选。\n\n命令示例：echo approval-demo\n目录示例：项目目录"
		grant.replies = []replyChoice{{"approve", "/approve " + requestToken, "/approve " + requestToken}, {"deny", "/deny " + requestToken, "/deny " + requestToken}}
		if question {
			grant.context = b.sessionLine(binding.ThreadID) + "\n\n隔离回答演示：请选择演示答案，也可修改文字后手动发送。不会向模型发起请求。"
			grant.questionIDs = []string{"demo"}
			grant.replies = []replyChoice{{"option_0", "/answer " + requestToken + " 演示答案", "/answer " + requestToken + " 演示答案"}}
		}
		b.draftMu.Lock()
		token, ok := b.newDraft(grant)
		b.draftMu.Unlock()
		if !ok {
			return errors.New("click demo grant unavailable")
		}
		if !b.emitInteraction(chat, grant.context, b.replyChoicesCard(token, grant), "demo", "", binding.ThreadID) {
			b.diagnostics.Record("demo", "failed")
			return errors.New("click demo queue unavailable")
		}
		b.diagnostics.Record("demo", "queued")
	}
	return nil
}

func (b *Bridge) HandleCard(action CardAction) (result CardResult) {
	return b.HandleAction(action)
}

// HandleAction is the frontend-independent callback boundary. Native adapters
// decode their own payloads; the application owns grants and authorization.
func (b *Bridge) HandleAction(action presentation.Action) (result presentation.Result) {
	if !b.beginOperation() {
		return CardResult{Message: "桥接即将维护，草稿尚未提交；连接恢复后请重新打开面板。", Error: true}
	}
	defer b.endOperation()
	stage := "callback"
	b.diagnostics.Record("callback", "received")
	defer func() {
		if result.Error {
			b.diagnostics.Record(stage, "invalid")
		} else if action.Action == "draft_open" {
			b.diagnostics.Record(stage, "opened")
		}
	}()
	invalid := CardResult{Message: "草稿已过期或不可用。请求回复请确认原请求仍有效；普通命令可重新发送 /commands。", Error: true}
	if !b.authorized(action.UserID) || action.ChatID == "" || action.Token == "" {
		return invalid
	}
	b.draftMu.Lock()
	defer b.draftMu.Unlock()
	grant, ok := b.drafts[action.Token]
	if !ok {
		if closed, exists := b.closedDrafts[action.Token]; exists && closed.owner == action.UserID && closed.chat == action.ChatID {
			return CardResult{Message: closed.reason + " 请重新打开原会话的状态或命令菜单。", Error: true}
		}
		return CardResult{Message: "旧卡片不可用，可能已被清理或因重启失效。未从此点击提交操作；请重新发送 /commands 或 /status。", Error: true}
	}
	if grant.owner != action.UserID || grant.chat != action.ChatID {
		return invalid
	}
	if !time.Now().Before(grant.expires) {
		b.closeDraft(action.Token, grant, "已超过有效期，未因此重新执行任何操作。")
		return CardResult{Message: "卡片已超过有效期，本次点击未提交；历史提交结果请查看对应会话的 /status，再重新打开 /commands。", Error: true}
	}
	if action.Action == "draft_open" {
		stage = "draft_open"
		if grant.demo {
			stage = "demo_open"
		}
	} else if action.Action == "draft_send" {
		stage = "draft_send"
		if grant.demo {
			stage = "demo_send"
		}
	} else if action.Action == "draft_back" {
		stage = "draft_back"
	}
	switch action.Action {
	case "draft_back":
		if grant.menu || grant.submitted {
			return invalid
		}
		// Reuse this grant as a menu: the old form token can no longer submit.
		// Keep its owner, request, target and original expiration unchanged.
		grant.menu = true
		b.drafts[action.Token] = grant
		var card json.RawMessage
		switch {
		case grant.request != "":
			card = b.replyChoicesCard(action.Token, grant)
		case grant.demo:
			card = b.demoCard(action.Token, grant.context)
		case len(grant.replies) > 0:
			card = b.render(presentation.View{Kind: "panel", Title: "操作与结果", Context: grant.context, Token: action.Token}, grant.replies)
		default:
			card = b.menuCard(action.Token, grant.target, grant.context)
		}
		b.diagnostics.Record(stage, "opened")
		return CardResult{Message: "已返回选项，可以重新选择；尚未提交。", Card: card}
	case "draft_open":
		if grant.submitted {
			return invalid
		}
		if grant.demo && grant.request == "" && action.Command != "status" {
			return invalid
		}
		if grant.request != "" || len(grant.replies) > 0 {
			for _, choice := range grant.replies {
				if choice.id == action.Command {
					return b.selectDraft(grant, choice.text, action.Token)
				}
			}
			return invalid
		}
		for _, command := range commandTemplates {
			if command.id != action.Command {
				continue
			}
			text := command.text
			if command.scoped {
				text += " " + grant.target
			}
			return b.selectDraft(grant, text, action.Token)
		}
	case "draft_send":
		if grant.menu {
			return invalid
		}
		if grant.submitted {
			b.diagnostics.Record(stage, "duplicate")
			return CardResult{Message: "这份草稿已经入队，请勿重复发送。", Card: grant.receipt}
		}
		text := strings.TrimSpace(action.Text)
		if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 1000 {
			return CardResult{Message: "请填写 1 至 1000 字的命令或任务。", Error: true}
		}
		if grant.request != "" && !validRequestReply(grant, text) {
			return CardResult{Message: "请保留原请求编号和回复命令，并填写全部答案；允许或拒绝请使用对应命令。", Error: true}
		}
		if command, _ := splitWord(text); grant.demo && grant.request == "" && command != "/status" {
			return CardResult{Message: "这张测试卡片仅支持 /status，可修改其会话编号。", Error: true}
		}
		if err := b.Submit(Input{ChatID: grant.chat, UserID: grant.owner, MessageID: "card-draft:" + action.Token, SourceCardID: action.MessageID, Text: text, ThreadID: grant.thread, Explicit: grant.thread != "", demoReply: grant.demo && grant.request != ""}); err != nil {
			b.diagnostics.Record(stage, "queue_full")
			return CardResult{Message: "接收队列已满，草稿保留，请稍后再次点击发送。", Error: true}
		}
		grant.submitted = true
		grant.receipt = b.submittedCard(grant, text)
		if grant.demo && grant.request != "" {
			for key, sibling := range b.drafts {
				if sibling.demo && sibling.request == grant.request {
					delete(b.drafts, key)
				}
			}
		}
		b.drafts[action.Token] = grant
		b.diagnostics.Record(stage, "queued")
		return CardResult{Message: "已入队，等待桥接处理。", Card: grant.receipt}
	}
	return invalid
}
