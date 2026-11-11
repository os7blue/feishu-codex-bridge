package bridge

import (
	"fmt"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
	"strings"
)

// Only bridge-authored replies use these shortcuts. Model text is never parsed
// into actions. Each grant keeps its original chat, owner and session.
func (b *Bridge) respond(input Input, text string, threads ...string) bool {
	thread := input.ThreadID
	if len(threads) > 0 {
		thread = threads[0]
	}
	if thread != "" {
		routed := input
		routed.ThreadID = thread
		if !b.rememberInputRoute(routed) {
			return false
		}
	}
	choices := []replyChoice{{"sessions", "/sessions  会话列表", "/sessions"}, {"new", "/new  新建会话", "/new"}, {"status", "/status  整体状态", "/status"}}
	if s, ok := b.store.Session(thread); ok && (s.OwnerID == "" || s.OwnerID == input.UserID) {
		target := fmt.Sprintf("#%d", s.Ref)
		choices = []replyChoice{{"commands", "/commands " + target + "  会话操作", "/commands " + target}, {"history", "/history " + target + "  最近消息", "/history " + target}, {"status", "/status " + target + "  会话状态", "/status " + target}}
		if s.OriginHome != "" {
			choices = append(choices, replyChoice{"fork", "/fork " + target + "  复制接续", "/fork " + target})
		}
		v := b.replyStatus.Load()
		if v != nil && v.thread == thread && (v.status == "执行中" || v.status == "等待审批或回答") {
			choices = append(choices, replyChoice{"stop", "/stop " + target + "  停止任务", "/stop " + target})
		}
		if v != nil && v.queued[thread] > 0 {
			choices = append(choices, replyChoice{"cancel", "/cancel " + target + "  取消此会话全部排队", "/cancel " + target})
		}
		choices = append(choices, replyChoice{"files", "附件与成果", "/files " + target})
	}
	if strings.HasPrefix(input.Text, "/help") || strings.HasPrefix(input.Text, "/start") {
		choices = append(choices, replyChoice{"file", "/file  文件收发帮助", "/file"}, replyChoice{"commands", "/commands  全部操作", "/commands"})
	}
	return b.emitChoices(input, text, choices, thread)
}

func (b *Bridge) emitChoices(input Input, text string, choices []replyChoice, thread string, panel ...string) bool {
	if b.cfg.AppSecret != "" {
		text = strings.ReplaceAll(text, b.cfg.AppSecret, "[REDACTED]")
		choices = append([]replyChoice(nil), choices...)
		for i := range choices {
			choices[i].label = strings.ReplaceAll(choices[i].label, b.cfg.AppSecret, "[REDACTED]")
			choices[i].text = strings.ReplaceAll(choices[i].text, b.cfg.AppSecret, "[REDACTED]")
		}
	}
	context := text
	title := "操作与结果"
	if len(panel) > 0 {
		context = panel[0]
		title = "命令面板"
		if b.cfg.AppSecret != "" {
			context = strings.ReplaceAll(context, b.cfg.AppSecret, "[REDACTED]")
		}
	}
	if thread != "" {
		context = b.sessionLine(thread) + "\n\n" + context
	}
	grant := commandDraft{chat: input.ChatID, owner: input.UserID, thread: thread, context: context, menu: true, replies: choices}
	b.draftMu.Lock()
	token, ok := b.newDraft(grant)
	b.draftMu.Unlock()
	if !ok {
		return b.emitInteraction(input.ChatID, text, nil, "", "", thread)
	}
	card := b.render(presentation.View{Kind: "panel", Title: title, Context: context, Token: token}, choices)
	if len(card) > 24000 {
		b.draftMu.Lock()
		delete(b.drafts, token)
		b.draftMu.Unlock()
		return b.emitInteraction(input.ChatID, text, nil, "", "", thread)
	}
	return b.emitInteraction(input.ChatID, text, card, "shortcut", token, thread)
}

func (b *Bridge) recoveryReply(chat, owner, text string, receipts ...inputReceipt) bool {
	choices := []replyChoice{{"status", "/status  检查状态", "/status"}, {"sessions", "/sessions  查找会话", "/sessions"}, {"new", "/new  新建会话", "/new"}}
	seen := map[string]bool{}
	for _, r := range receipts {
		if r.Input.ChatID != chat || r.Input.UserID != owner || seen[r.Input.ThreadID] || len(seen) >= 8 {
			continue
		}
		s, ok := b.store.Session(r.Input.ThreadID)
		if !ok || s.OwnerID != owner {
			continue
		}
		seen[s.ThreadID] = true
		text += fmt.Sprintf("\n待确认对象：会话 #%d · %s。未自动重放；请先检查该会话记录。", s.Ref, b.sessionLabel(s))
		choices = append(choices, replyChoice{fmt.Sprintf("recover_%d", s.Ref), fmt.Sprintf("会话 #%d · 检查最近记录", s.Ref), fmt.Sprintf("/history #%d", s.Ref)})
	}
	return b.emitChoices(Input{ChatID: chat, UserID: owner}, text, choices, "")
}
