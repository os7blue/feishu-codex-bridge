package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/codex"
)

func (b *Bridge) event(message codex.Message) {
	if !b.beginOperation() {
		return
	}
	defer b.endOperation()
	if len(message.ID) > 0 && string(message.ID) != "null" {
		b.request(message)
		return
	}
	var p struct {
		ThreadID       string          `json:"threadId"`
		TurnID         string          `json:"turnId"`
		RequestID      json.RawMessage `json:"requestId"`
		ThreadSettings json.RawMessage `json:"threadSettings"`
		Review         struct {
			Status string `json:"status"`
		} `json:"review"`
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		Item struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Text    string `json:"text"`
			Phase   string `json:"phase"`
			Command string `json:"command"`
			Status  string `json:"status"`
			Changes []struct {
				Path string `json:"path"`
				Diff string `json:"diff"`
			} `json:"changes"`
		} `json:"item"`
		TokenUsage struct {
			Last struct {
				Input int64 `json:"inputTokens"`
			} `json:"last"`
			Window int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(message.Params, &p) != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if message.Method == "thread/closed" {
		delete(b.loaded, p.ThreadID)
		return
	}
	a := b.active
	if a == nil || p.ThreadID != a.thread {
		return
	}
	if message.Method == "turn/started" {
		if a.turn == "" {
			a.turn = p.Turn.ID
		}
		b.refreshReplyStatusLocked()
		return
	}
	if p.TurnID != "" && a.turn != "" && p.TurnID != a.turn {
		return
	}
	switch message.Method {
	case "thread/settings/updated":
		b.observeApprovalSettings(p.ThreadSettings)
	case "serverRequest/resolved":
		for token, request := range b.pending {
			if string(request.id) == string(p.RequestID) {
				b.diagnostics.Record("response", "resolved")
				b.forgetPending(token)
			}
		}
	case "item/completed":
		if p.Item.Type == "commandExecution" {
			status, text := "unknown", "命令执行已结束，未识别结果状态。"
			switch p.Item.Status {
			case "completed":
				status, text = "completed", "命令执行完成。"
			case "failed":
				status, text = "failed", "命令执行失败。"
			case "declined":
				status, text = "declined", "命令执行请求被拒绝。"
			}
			b.diagnostics.Record("command", status)
			if status != "completed" {
				b.emit(a.chat, text, a.thread)
			}
		}
		if p.Item.Type == "agentMessage" && p.Item.Text != "" {
			if p.Item.Phase == "commentary" {
				b.emit(a.chat, p.Item.Text, a.thread)
			} else if a.finalBytes+len(p.Item.Text) <= 512000 {
				a.final = append(a.final, p.Item.Text)
				a.finalBytes += len(p.Item.Text)
			} else {
				b.emit(a.chat, p.Item.Text, a.thread)
			}
		}
	case "item/started":
		if p.Item.Type == "commandExecution" {
			b.diagnostics.Record("command", "started")
		}
		if p.Item.Type == "commandExecution" || p.Item.Type == "fileChange" {
			preview := p.Item.Command
			for _, change := range p.Item.Changes {
				preview += "\n文件：" + change.Path + "\n" + change.Diff
			}
			if len(preview) > 16000 {
				preview = preview[:16000] + "\n（预览已截断，请在本机查看完整修改）"
			}
			if a.previews == nil {
				a.previews = map[string]string{}
			}
			if len(a.previews) < 128 {
				a.previews[p.Item.ID] = preview
			}
		}
		// Meaningful progress comes from agent commentary. Retain diagnostics
		// and approval previews without sending generic tool-activity notices.
	case "item/autoApprovalReview/started":
		b.diagnostics.Record("auto_review", "started")
	case "item/autoApprovalReview/completed":
		status, text := "unknown", "自动审批审查已结束，未识别结果状态。"
		switch p.Review.Status {
		case "approved":
			status, text = "approved", "自动审批审查已允许操作；执行结果另行报告。"
		case "denied":
			status, text = "denied", "自动审批审查已拒绝操作；当前仅确认审查结果，未确认命令执行。"
		case "timedOut":
			status, text = "timedOut", "自动审批审查超时。"
		case "aborted":
			status, text = "aborted", "自动审批审查已中止。"
		}
		b.diagnostics.Record("auto_review", status)
		b.emit(a.chat, text, a.thread)
	case "thread/tokenUsage/updated":
		// Use the latest input, never cumulative billed tokens, as context usage.
		if p.TokenUsage.Window > 0 && p.TokenUsage.Last.Input >= p.TokenUsage.Window*75/100 {
			claimed, err := b.store.ClaimHandoff(a.thread)
			if err != nil {
				b.fail(errors.New("cannot record context handoff"))
				return
			}
			if !claimed {
				return
			}
			if !a.handoff {
				b.emit(a.chat, "会话上下文已接近容量，正在要求 Codex 提前生成交接文档。完成后会提醒“该切换会话了”，届时用 /new 切换。", a.thread)
				key, err := randomToken(16)
				if err != nil {
					b.fail(errors.New("cannot prepare handoff request"))
					return
				}
				select {
				case b.in <- Input{ChatID: a.chat, UserID: a.owner, MessageID: "handoff-" + key, Text: "/handoff", ThreadID: a.thread, Explicit: true}:
				default:
					b.fail(errors.New("cannot enqueue context handoff"))
				}
			}
		}
	case "turn/completed":
		if a.turn != "" && p.Turn.ID != a.turn {
			return
		}
		a.resultStatus = "已结束"
		switch p.Turn.Status {
		case "completed":
			a.resultStatus = "已完成"
		case "interrupted":
			a.resultStatus = "已停止"
		case "failed":
			a.resultStatus = "失败"
		}
		b.refreshReplyStatusLocked()
		text := strings.Join(a.final, "\n\n")
		switch p.Turn.Status {
		case "completed":
			if text == "" {
				text = "本轮执行结束，Codex 未返回文字结果。"
			}
		case "interrupted":
			text = "任务已停止。已执行的文件修改仍保留。\n" + text
		case "failed":
			text = "任务失败，请检查本机 Codex 会话和配置。已执行的修改可能仍保留。\n" + text
		default:
			text = "任务结束，状态：" + p.Turn.Status + "\n" + text
		}
		var retainedResult bool
		if p.Turn.Status == "failed" || p.Turn.Status == "interrupted" {
			retainedResult = b.respond(Input{ChatID: a.chat, UserID: a.owner, ThreadID: a.thread}, text, a.thread)
		} else {
			retainedResult = b.emitInteraction(a.chat, text, nil, "", "", a.thread)
		}
		if a.handoff && p.Turn.Status == "completed" {
			path := filepath.Join(b.cfg.ProjectDir, "docs", "HANDOFF.md")
			if s, ok := b.store.Session(a.thread); ok && s.Cwd != "" {
				path = filepath.Join(s.Cwd, "docs", "HANDOFF.md")
			}
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && !info.ModTime().Before(a.started) {
				s, _ := b.store.Session(a.thread)
				b.emitChoices(Input{ChatID: a.chat, UserID: a.owner}, "交接文档已更新："+path+"\n该切换会话了。接续会沿用本项目、交接文档和模型/强度；新建独立任务使用全局默认。", []replyChoice{{"continue", "沿用项目和配置接续", fmt.Sprintf("/continue #%d", s.Ref)}, {"file", "发送交接文档", fmt.Sprintf("/file #%d docs/HANDOFF.md", s.Ref)}, {"new", "新建独立任务", "/projects"}}, a.thread)
			} else {
				b.respond(Input{ChatID: a.chat, UserID: a.owner, ThreadID: a.thread}, "未确认交接文档已更新，请检查 Codex 的结果后再切换会话。", a.thread)
			}
		}
		if retainedResult {
			for _, id := range a.inputs {
				b.markInput(id, "handled")
			}
		}
		for token, request := range b.pending {
			if request.thread == a.thread && request.turn == a.turn {
				b.forgetPending(token)
			}
		}
		b.active = nil
		b.refreshReplyStatusLocked()
		b.signalNext()
	}
}

func (b *Bridge) request(message codex.Message) {
	b.diagnostics.Record("request", "received")
	var p struct {
		ThreadID  string     `json:"threadId"`
		TurnID    string     `json:"turnId"`
		Reason    string     `json:"reason"`
		Command   string     `json:"command"`
		Cwd       string     `json:"cwd"`
		ItemID    string     `json:"itemId"`
		Kind      string     `json:"kind"`
		Questions []question `json:"questions"`
	}
	if json.Unmarshal(message.Params, &p) != nil {
		b.rejectRequest(message, "invalid")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.active
	if a == nil || p.ThreadID != a.thread || (a.turn != "" && p.TurnID != a.turn) {
		b.rejectRequest(message, "unavailable")
		return
	}
	if a.turn == "" {
		a.turn = p.TurnID
	}
	if len(b.pending) >= 16 {
		b.rejectRequest(message, "capacity")
		return
	}
	token, err := randomToken(8)
	if err != nil {
		b.fail(errors.New("cannot generate approval token"))
		return
	}
	request := pending{id: message.ID, method: message.Method, chat: a.chat, owner: a.owner, thread: p.ThreadID, turn: p.TurnID, questions: p.Questions, created: time.Now()}
	var text string
	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		class := "command_approval"
		if message.Method == "item/fileChange/requestApproval" {
			class = "file_approval"
		}
		if p.Kind == "humanCommand" {
			class = "human_command"
		}
		b.diagnostics.Record("request", class)
		text = "Codex 请求允许一次操作。\n\n" + p.Reason
		if message.Method == "item/commandExecution/requestApproval" && p.Kind == "humanCommand" {
			text = "Codex 请求强制人工审批。此操作必须由你逐次确认，允许仅对本次操作有效。\n\n" + p.Reason
		}
		if p.Command != "" {
			text += "\n\n命令：" + p.Command
		}
		if p.Cwd != "" {
			text += "\n\n目录：" + p.Cwd
		}
		if preview := a.previews[p.ItemID]; preview != "" {
			text += "\n\n操作预览：" + preview
		}
		text += "\n允许：/approve " + token + "\n拒绝：/deny " + token
	case "item/tool/requestUserInput":
		b.diagnostics.Record("request", "user_input")
		if len(p.Questions) == 0 {
			b.rejectRequest(message, "invalid")
			return
		}
		for _, q := range p.Questions {
			if q.IsSecret {
				b.rejectRequest(message, "secret")
				b.emit(a.chat, "Codex 请求私密输入，请在本机处理；桥接程序未转发此输入请求。", a.thread)
				return
			}
			text += "\n" + q.ID + "：" + q.Question
			for _, option := range q.Options {
				text += "\n- " + option.Label + "：" + option.Description
			}
		}
		text += "\n回复：/answer " + token + " 答案"
		if len(p.Questions) > 1 {
			text += "\n多题请用 JSON 对象，例如 {\"题目ID\":\"答案\"}，须回答所有题目。"
		}
	default:
		b.rejectRequest(message, "unsupported")
		b.emit(a.chat, "Codex 发出了当前桥接版本尚不支持的交互请求，已拒绝；请在本机处理。", a.thread)
		return
	}
	b.pending[token] = request
	b.refreshReplyStatusLocked()
	prompt := strings.TrimSpace(text) + "\n此请求十分钟内有效。"
	b.emitInteraction(a.chat, prompt, b.replyMenu(token, prompt, request), "request", token, a.thread)
}

func (b *Bridge) rejectRequest(message codex.Message, reason string) {
	b.diagnostics.Record("request_rejected", reason)
	if b.rpc.Reject(message.ID) != nil {
		b.diagnostics.Record("response", "failed")
	}
}

func (b *Bridge) answer(input Input, command, rest string) {
	token, answer := splitWord(rest)
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[token]
	if !ok || p.chat != input.ChatID || p.owner != input.UserID {
		b.diagnostics.Record("response", "unavailable")
		b.respond(input, "请求不存在、已过期或不属于当前会话。")
		return
	}
	if time.Since(p.created) >= 10*time.Minute {
		b.diagnostics.Record("response", "expired")
		if p.method == "item/tool/requestUserInput" {
			_ = b.rpc.Reject(p.id)
		} else {
			_ = b.rpc.Respond(p.id, map[string]string{"decision": "decline"})
		}
		b.forgetPending(token)
		b.respond(input, "请求已过期，未允许操作。")
		return
	}
	var result any
	if p.method == "item/tool/requestUserInput" {
		if command != "/answer" {
			b.respond(input, "此请求需要用 /answer 回复。")
			return
		}
		answers := map[string]string{}
		if len(p.questions) == 1 {
			answers[p.questions[0].ID] = strings.TrimSpace(answer)
		} else if json.Unmarshal([]byte(answer), &answers) != nil {
			b.respond(input, "多题答案必须是 JSON 对象。")
			return
		}
		wire := map[string]any{}
		for _, q := range p.questions {
			if strings.TrimSpace(answers[q.ID]) == "" {
				b.respond(input, "请回答所有题目，答案不能为空。")
				return
			}
			wire[q.ID] = map[string]any{"answers": []string{answers[q.ID]}}
		}
		result = map[string]any{"answers": wire}
	} else {
		if command != "/approve" && command != "/deny" {
			b.respond(input, "此请求需要用 /approve 或 /deny 回复。")
			return
		}
		decision := "decline"
		if command == "/approve" {
			decision = "accept"
		}
		result = map[string]string{"decision": decision}
	}
	if err := b.rpc.Respond(p.id, result); err != nil {
		b.diagnostics.Record("response", "failed")
		b.fail(errors.New("cannot deliver approval response to Codex"))
		return
	}
	outcome := "answered"
	if command == "/approve" {
		outcome = "accepted"
	}
	if command == "/deny" {
		outcome = "declined"
	}
	b.diagnostics.Record("response", outcome)
	b.forgetPending(token)
	b.emit(input.ChatID, "回复已回传 Codex；后续操作结果将另行报告。", p.thread)
}

func (b *Bridge) expire(ctx context.Context) {
	if !b.beginOperation() {
		return
	}
	defer b.endOperation()
	b.mu.Lock()
	for token, request := range b.pending {
		if time.Since(request.created) < 10*time.Minute {
			continue
		}
		b.diagnostics.Record("response", "expired")
		if request.method == "item/tool/requestUserInput" {
			_ = b.rpc.Reject(request.id)
		} else {
			_ = b.rpc.Respond(request.id, map[string]string{"decision": "decline"})
		}
		b.forgetPending(token)
		b.respond(Input{ChatID: request.chat, UserID: request.owner, ThreadID: request.thread}, "交互请求已超时，未自动允许操作。", request.thread)
	}
	a := b.active
	if a != nil && a.stopping && !a.stopAt.IsZero() && time.Since(a.stopAt) > 30*time.Second {
		b.mu.Unlock()
		b.fail(errors.New("Codex did not confirm turn completion after interruption; bridge is stopping"))
		return
	}
	if a == nil || a.stopping || a.turn == "" || time.Since(a.started) < time.Duration(b.cfg.TurnTimeoutSeconds)*time.Second {
		b.mu.Unlock()
		return
	}
	input := Input{ChatID: a.chat, UserID: a.owner}
	b.mu.Unlock()
	b.respond(input, fmt.Sprintf("任务超过配置的 %d 秒时限，正在请求停止。", b.cfg.TurnTimeoutSeconds))
	b.stop(ctx, input)
}
