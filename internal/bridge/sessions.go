package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type threadSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Preview string `json:"preview"`
	Cwd     string `json:"cwd"`
	Status  struct {
		Type string `json:"type"`
	} `json:"status"`
}

// Reuse existing titles/previews or the first task; labels never start a model request.
func (b *Bridge) shortSummary(text string) string {
	if b.cfg.AppSecret != "" {
		text = strings.ReplaceAll(text, b.cfg.AppSecret, "[REDACTED]")
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 60 {
		text = string(runes[:59]) + "…"
	}
	return text
}

func (b *Bridge) sessionLabel(s state.Session) string {
	if name := b.shortSummary(s.Name); name != "" {
		return name
	}
	if summary := b.shortSummary(s.Summary); summary != "" {
		return summary
	}
	return "未命名会话"
}

func splitWord(text string) (string, string) {
	text = strings.TrimSpace(text)
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		return text[:i], strings.TrimSpace(text[i:])
	}
	return text, ""
}

func (b *Bridge) isSessionRef(text string) bool {
	if strings.HasPrefix(text, "#") || validThreadID(text) {
		return true
	}
	if _, err := strconv.Atoi(text); err == nil && text != "" {
		return true
	}
	_, ok := b.store.Session(text)
	return ok
}

func (b *Bridge) sessionLine(id string) string {
	s, ok := b.store.Session(id)
	if !ok {
		return "会话信息尚未记录"
	}
	return fmt.Sprintf("会话 #%d · %s\n%s", s.Ref, b.sessionLabel(s), b.sessionDisplay(s))
}

func (b *Bridge) overview(input Input) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshReplyStatusLocked()
	lines := []string{"多会话状态"}
	if b.cfg.Sandbox == "workspace-write" && len(b.cfg.WritableRoots) > 0 {
		lines = append(lines, "桥接额外授权目录：\n"+strings.Join(b.cfg.WritableRoots, "\n"))
	}
	if binding, ok := b.store.Binding(input.ChatID); ok && binding.OwnerID == input.UserID {
		lines = append(lines, "普通消息默认目标："+b.sessionLine(binding.ThreadID))
	} else {
		lines = append(lines, "普通消息尚无默认会话。")
	}
	if a := b.active; a != nil && a.chat == input.ChatID && a.owner == input.UserID {
		lines = append(lines, "当前任务："+b.sessionLine(a.thread))
	} else {
		lines = append(lines, "本聊天没有执行中的任务。")
	}
	queued := 0
	for _, q := range b.queue {
		if q.Input.ChatID == input.ChatID && q.Input.UserID == input.UserID {
			queued++
		}
	}
	lines = append(lines, fmt.Sprintf("排队任务：%d（/queue 查看）", queued), "引用会话消息发送 /status 查看该会话；/sessions 浏览会话。")
	b.respond(input, strings.Join(lines, "\n\n"))
}

type sessionPage struct {
	Cursor    string
	Archived  bool
	LocalDone bool
	External  []state.Session
	Offset    int
	HasMore   bool
}

type queued struct {
	ID      string `json:"id"`
	Input   Input  `json:"input"`
	Text    string `json:"text"`
	Handoff bool   `json:"handoff"`
	Created int64  `json:"created,omitempty"`
}

func (b *Bridge) target(input *Input, ref string) bool {
	var session state.Session
	var ok bool
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil && n > 0 {
		session, ok = b.store.SessionByRef(n)
	} else {
		session, ok = b.store.Session(ref)
	}
	if !ok {
		b.respond(*input, "会话不存在。先发送 /sessions 获取编号。")
		return false
	}
	if session.OwnerID != "" && session.OwnerID != input.UserID {
		b.respond(*input, "此会话属于其他账号。")
		return false
	}
	if session.OwnerID == "" {
		session.OwnerID = input.UserID
		if _, err := b.store.RememberSession(session); err != nil {
			b.respond(*input, "无法记录会话归属。")
			return false
		}
	}
	input.ThreadID = session.ThreadID
	input.Explicit = true
	return b.rememberInputRoute(*input)
}

func (b *Bridge) routeInput(input *Input) bool {
	if input.ThreadID != "" {
		return b.rememberInputRoute(*input)
	}
	parent := input.ReplyTo
	if parent == "" {
		parent = input.ReplyRoot
	}
	if parent != "" {
		route, ok := b.store.Route(parent, input.ChatID, input.UserID)
		// A known but invalid parent must not be bypassed using a root from a
		// different identity or an expired chain. A valid direct parent wins.
		if !ok && route.ThreadID == "" && route.ChatID == "" && input.ReplyRoot != "" && input.ReplyRoot != parent {
			route, ok = b.store.Route(input.ReplyRoot, input.ChatID, input.UserID)
		}
		if !ok {
			b.respond(*input, "这条回复无法定位会话，请用 #会话编号 内容，或 /sessions 查找。未发送给默认会话。")
			return false
		}
		input.ThreadID = route.ThreadID
		input.Explicit = true
		return b.rememberInputRoute(*input)
	}
	if binding, ok := b.store.Binding(input.ChatID); ok {
		if binding.OwnerID != input.UserID {
			b.respond(*input, "默认会话属于其他账号。")
			return false
		}
		input.ThreadID = binding.ThreadID
	}
	return b.rememberInputRoute(*input)
}

func (b *Bridge) rememberInputRoute(input Input) bool {
	if input.MessageID == "" || input.ThreadID == "" || !b.authorized(input.UserID) {
		return true
	}
	created := time.Now().Unix()
	if saved, ok := b.store.Route(input.MessageID, input.ChatID, input.UserID); ok && saved.ThreadID == input.ThreadID {
		created = saved.Created
	}
	ids := []string{input.MessageID}
	if input.SourceCardID != "" {
		ids = append(ids, input.SourceCardID)
	}
	if err := b.store.RememberRoutes(ids, state.MessageRoute{ChatID: input.ChatID, OwnerID: input.UserID, ThreadID: input.ThreadID, Created: created}); err != nil {
		b.fail(errors.New("cannot persist incoming message routing"))
		return false
	}
	return b.rememberConversationInput(input, created)
}

func (b *Bridge) sessionsCommand(ctx context.Context, input Input, command, rest string) bool {
	switch command {
	case "/new":
		b.newSession(ctx, input, rest)
	case "/sessions":
		b.listSessions(ctx, input, rest)
	case "/use", "/show", "/history", "/rename", "/archive", "/unarchive", "/cancel", "/fork", "/stop", "/status", "/handoff":
		if command == "/status" && rest == "" && input.ReplyTo == "" && input.ThreadID == "" {
			b.overview(input)
			return true
		}
		ref, body := splitWord(rest)
		if command == "/rename" && ref != "" && !b.isSessionRef(ref) {
			body, ref = rest, ""
		}
		if command != "/rename" && body != "" {
			b.respond(input, "用法："+command+" #编号；也可引用会话消息只发送 "+command+"。")
			return true
		}
		if ref != "" {
			if !b.target(&input, ref) {
				return true
			}
		} else if !b.routeInput(&input) {
			return true
		}
		if input.ThreadID == "" {
			b.respond(input, "请引用带会话编号的消息，或指定编号，例如 "+command+" #1；/sessions 查看列表。")
			return true
		}
		switch command {
		case "/status":
			b.status(ctx, input)
			return true
		case "/stop":
			b.stop(ctx, input)
			return true
		case "/handoff":
			b.prompt(ctx, input, handoffPrompt, true)
			return true
		}
		if command == "/cancel" {
			b.cancelQueued(input)
			return true
		}
		if command == "/fork" {
			b.forkSession(ctx, input)
			return true
		}
		saved, _ := b.store.Session(input.ThreadID)
		if saved.OriginHome != "" {
			switch command {
			case "/history":
				b.externalHistory(input, saved)
			case "/show":
				b.respond(input, fmt.Sprintf("外部会话（原始记录只读）\n名称：%s\nID：%s\n项目：%s\n/fork #%d 创建独立副本接续。\n\n%s", saved.Name, saved.ThreadID, saved.Cwd, saved.Ref, b.sessionModelInfo(ctx, input)), saved.ThreadID)
			default:
				b.respond(input, fmt.Sprintf("此外部会话保留在原客户端；用 /fork #%d 创建独立副本接续，不会接管正在运行的原会话。", saved.Ref), saved.ThreadID)
			}
			return true
		}
		if command == "/use" {
			s, ok := b.readSession(ctx, input)
			if !ok {
				return true
			}
			if _, err := b.store.RememberSession(state.Session{ThreadID: s.ID, Name: s.Name, Cwd: s.Cwd, OwnerID: input.UserID}); err != nil {
				b.respond(input, "无法保存会话归属。")
				return true
			}
			if err := b.store.Bind(input.ChatID, state.Binding{ThreadID: s.ID, OwnerID: input.UserID}); err != nil {
				b.fail(errors.New("cannot save selected session"))
				return true
			}
			b.mu.Lock()
			b.selection[input.ChatID]++
			b.mu.Unlock()
			b.respond(input, "默认会话已切换。回复历史机器人消息仍会定位到原会话。", s.ID)
			return true
		}
		if command == "/show" {
			s, ok := b.readSession(ctx, input)
			if ok {
				b.respondSessionInfo(input, fmt.Sprintf("名称：%s\nID：%s\n项目：%s\n后端会话状态：%s\n\n%s", s.Name, s.ID, s.Cwd, s.Status.Type, b.sessionModelInfo(ctx, input)))
			}
			return true
		}
		if command == "/history" {
			b.history(ctx, input)
			return true
		}
		b.mu.Lock()
		busy := b.active != nil && b.active.thread == input.ThreadID
		for _, q := range b.queue {
			if q.Input.ThreadID == input.ThreadID {
				busy = true
			}
		}
		b.mu.Unlock()
		if busy {
			b.respond(input, "会话正在执行或排队，请先停止/取消任务。", input.ThreadID)
			return true
		}
		method := "thread/archive"
		params := map[string]any{"threadId": input.ThreadID}
		if command == "/rename" {
			if strings.TrimSpace(body) == "" || len(body) > 200 {
				b.respond(input, "用法：/rename #编号 新名称；也可引用会话消息发送 /rename 新名称（不超过200字节）。")
				return true
			}
			method = "thread/name/set"
			params["name"] = strings.TrimSpace(body)
		} else if command == "/unarchive" {
			method = "thread/unarchive"
		}
		if _, err := b.rpcCall(ctx, method, params); err != nil {
			b.respond(input, "会话操作失败，原绑定保留。", input.ThreadID)
			return true
		}
		if command == "/rename" {
			if s, ok := b.store.Session(input.ThreadID); ok {
				s.Name = strings.TrimSpace(body)
				if _, err := b.store.RememberSession(s); err != nil {
					b.fail(err)
				}
			}
		}
		if command == "/archive" {
			b.mu.Lock()
			delete(b.loaded, input.ThreadID)
			b.mu.Unlock()
		}
		b.respond(input, "会话操作完成。", input.ThreadID)
	case "/queue":
		b.mu.Lock()
		var lines []string
		var choices []replyChoice
		seen := map[string]bool{}
		for _, q := range b.queue {
			if q.Input.ChatID == input.ChatID && q.Input.UserID == input.UserID {
				s, _ := b.store.Session(q.Input.ThreadID)
				stamp := "时间未记录"
				if q.Created > 0 {
					stamp = time.Unix(q.Created, 0).Format("01-02 15:04:05")
				}
				lines = append(lines, fmt.Sprintf("%d. 任务 %s · 会话 #%d · %s\n内容：%s\n提交：%s · 等待中\n项目：%s", len(lines)+1, q.ID, s.Ref, b.sessionLabel(s), b.shortSummary(q.Text), stamp, s.Cwd))
				if q.ID != "" {
					choices = append(choices, replyChoice{"task_" + q.ID, "仅取消此任务 · " + b.shortSummary(q.Text), "/cancelone " + q.ID})
				}
				if !seen[s.ThreadID] && len(choices) < 16 {
					choices = append(choices, replyChoice{fmt.Sprintf("cancel_%d", s.Ref), fmt.Sprintf("/cancel #%d  取消此会话全部排队", s.Ref), fmt.Sprintf("/cancel #%d", s.Ref)})
					seen[s.ThreadID] = true
				}
			}
		}
		b.mu.Unlock()
		if len(lines) == 0 {
			b.respond(input, "本聊天没有排队任务。")
		} else {
			choices = append(choices, replyChoice{"queue", "/queue  刷新队列", "/queue"})
			b.emitChoices(input, "等待 Codex API：\n"+strings.Join(lines, "\n"), choices, "")
		}
	default:
		return false
	}
	return true
}

func (b *Bridge) newSession(ctx context.Context, input Input, arguments string, continuation ...bool) {
	directory, name, parseErr := newSessionArguments(arguments)
	if parseErr != nil {
		b.respond(input, parseErr.Error())
		return
	}
	if len(name) > 200 {
		b.respond(input, "会话名称不超过200字节。")
		return
	}
	cwd, ok := b.newSessionDirectory(input, directory)
	if !ok {
		return
	}
	params := b.sessionParams(cwd)
	sourceThread := ""
	if len(continuation) > 0 && continuation[0] {
		sourceThread = input.ThreadID
	}
	settings, settingsErr := b.nextModelSettings(ctx, sourceThread, input.UserID)
	if settingsErr != nil {
		b.respond(input, settingsErr.Error()+" 未创建会话。")
		return
	}
	params["model"] = settings.Model
	raw, err := b.rpcCall(ctx, "thread/start", params)
	var result struct {
		Thread threadSummary `json:"thread"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Thread.ID == "" || !b.workspaceAllowed(result.Thread.Cwd) {
		b.respond(input, "新建会话失败，原默认会话保持。")
		return
	}
	if result.Thread.Cwd == "" {
		result.Thread.Cwd = cwd
	}
	returnedCwd, cwdErr := filepath.EvalSymlinks(result.Thread.Cwd)
	if cwdErr != nil || returnedCwd != cwd {
		b.respond(input, "新会话返回的项目目录与所选目录不符，未切换默认会话。")
		return
	}
	result.Thread.Cwd = cwd
	if name != "" {
		if _, err := b.rpcCall(ctx, "thread/name/set", map[string]any{"threadId": result.Thread.ID, "name": name}); err != nil {
			b.respond(input, "会话已创建，但名称保存失败。", result.Thread.ID)
		} else {
			result.Thread.Name = name
		}
	}
	s, err := b.store.RememberSession(state.Session{ThreadID: result.Thread.ID, Cwd: result.Thread.Cwd, Name: result.Thread.Name, Summary: b.shortSummary(result.Thread.Preview), OwnerID: input.UserID})
	if err != nil {
		b.fail(errors.New("cannot save new session"))
		return
	}
	if sourceThread != "" {
		if err := b.store.SetModelSettings(s.ThreadID, input.UserID, state.ModelSettings{Model: settings.Model, Effort: settings.Effort}); err != nil {
			b.respond(input, "接续配置保存失败，未切换默认或启动任务。", sourceThread)
			return
		}
	}
	if err := b.store.Bind(input.ChatID, state.Binding{ThreadID: s.ThreadID, OwnerID: input.UserID}); err != nil {
		b.fail(errors.New("cannot select new session"))
		return
	}
	b.mu.Lock()
	b.loaded[s.ThreadID] = true
	b.selection[input.ChatID]++
	b.mu.Unlock()
	b.respond(input, fmt.Sprintf("新建会话 #%d，已设为默认。\n项目：%s\n直接回复本消息，或发送 #%d 内容开始。其他会话任务继续保留。", s.Ref, cwd, s.Ref), s.ThreadID)
	if sourceThread != "" {
		input.ThreadID, input.Explicit = s.ThreadID, true
		b.prompt(ctx, input, "读取 docs/HANDOFF.md，核对实际状态并继续未完成工作。", false)
	}
}

func (b *Bridge) readSession(ctx context.Context, input Input) (threadSummary, bool) {
	raw, err := b.rpcCall(ctx, "thread/read", map[string]any{"threadId": input.ThreadID})
	var result struct {
		Thread threadSummary `json:"thread"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Thread.ID != input.ThreadID {
		b.respond(input, "无法读取此会话，未启动或改变任务。")
		return threadSummary{}, false
	}
	if _, err := b.store.RememberSession(state.Session{ThreadID: result.Thread.ID, Name: result.Thread.Name, Summary: b.shortSummary(result.Thread.Preview), Cwd: result.Thread.Cwd, OwnerID: input.UserID}); err != nil {
		b.respond(input, "无法保存会话信息，未启动或改变任务。")
		return threadSummary{}, false
	}
	return result.Thread, true
}

func (b *Bridge) listSessions(ctx context.Context, input Input, mode string) {
	page := sessionPage{}
	if mode == "next" {
		b.mu.Lock()
		page = b.pages[input.ChatID]
		b.mu.Unlock()
		if !page.HasMore {
			b.respond(input, "没有下一页，请发送 /sessions 刷新。")
			return
		}
	} else if mode == "archived" {
		page.Archived = true
	} else if mode != "" {
		b.respond(input, "用法：/sessions、/sessions next 或 /sessions archived")
		return
	}
	if mode != "next" {
		var err error
		page.External, err = b.externalCatalog(page.Archived)
		if err != nil {
			b.respond(input, "外部会话目录读取失败，本次未返回不完整列表。")
			return
		}
	}
	params := map[string]any{"limit": 20, "modelProviders": []string{}, "sourceKinds": []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}, "sortKey": "updated_at", "archived": page.Archived}
	if page.Cursor != "" {
		params["cursor"] = page.Cursor
	}
	var result struct {
		Data []threadSummary `json:"data"`
		Next string          `json:"nextCursor"`
	}
	if !page.LocalDone {
		raw, err := b.rpcCall(ctx, "thread/list", params)
		if err != nil || json.Unmarshal(raw, &result) != nil {
			b.respond(input, "会话列表读取失败，请稍后重试。")
			return
		}
		page.Cursor = result.Next
		page.LocalDone = result.Next == ""
	}
	var lines []string
	var choices []replyChoice
	for _, s := range result.Data {
		session, err := b.store.RememberSession(state.Session{ThreadID: s.ID, Name: s.Name, Summary: b.shortSummary(s.Preview), Cwd: s.Cwd})
		if err != nil {
			b.fail(errors.New("cannot save session catalog"))
			return
		}
		if session.OwnerID != "" && session.OwnerID != input.UserID {
			continue
		}
		if session.OriginHome != "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("#%d %s [%s]\n  %s", session.Ref, b.sessionLabel(session), b.sessionActivityStatus(session.ThreadID), session.Cwd))
		choices = append(choices, replyChoice{fmt.Sprintf("session_%d", session.Ref), lines[len(lines)-1], fmt.Sprintf("/commands #%d", session.Ref)})
	}
	if page.LocalDone {
		for page.Offset < len(page.External) && len(lines) < 20 {
			s := page.External[page.Offset]
			page.Offset++
			session, err := b.store.RememberSession(s)
			if err != nil {
				b.fail(errors.New("cannot save external session catalog"))
				return
			}
			if session.OwnerID != "" && session.OwnerID != input.UserID {
				continue
			}
			lines = append(lines, fmt.Sprintf("#%d %s [外部原始记录只读]\n  %s", session.Ref, b.sessionLabel(session), session.Cwd))
			choices = append(choices, replyChoice{fmt.Sprintf("session_%d", session.Ref), lines[len(lines)-1], fmt.Sprintf("/commands #%d", session.Ref)})
		}
	}
	page.HasMore = !page.LocalDone || page.Offset < len(page.External)
	b.mu.Lock()
	b.pages[input.ChatID] = page
	b.mu.Unlock()
	text := "Codex 会话：\n" + strings.Join(lines, "\n")
	if len(lines) == 0 {
		text = "本页没有可管理的会话。"
	}
	if page.HasMore {
		text += "\n下一页：/sessions next"
		choices = append(choices, replyChoice{"next", "/sessions next  下一页", "/sessions next"})
	}
	choices = append(choices, replyChoice{"refresh", "/sessions  刷新", "/sessions"}, replyChoice{"archived", "/sessions archived  已归档", "/sessions archived"}, replyChoice{"new", "/new  新建会话", "/new"})
	panel := ""
	if len(lines) == 0 {
		panel = "本页没有可管理的会话。"
	}
	b.emitChoices(input, text+"\n点击会话编号生成操作菜单命令；手动发送后查看该会话可用操作。", choices, "", panel)
}

func (b *Bridge) history(ctx context.Context, input Input) {
	raw, err := b.rpcCall(ctx, "thread/items/list", map[string]any{"threadId": input.ThreadID, "limit": 12, "sortDirection": "desc"})
	var result struct {
		Data []struct {
			Item struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil {
		b.respond(input, "历史读取失败，未恢复或启动会话。", input.ThreadID)
		return
	}
	var lines []string
	for i := len(result.Data) - 1; i >= 0; i-- {
		item := result.Data[i].Item
		role, text := "", ""
		if item.Type == "agentMessage" {
			role = "Codex"
			text = item.Text
		} else if item.Type == "userMessage" {
			role = "用户"
			for _, c := range item.Content {
				if c.Type == "text" {
					text += c.Text + "\n"
				}
			}
		}
		if text != "" {
			runes := []rune(text)
			if len(runes) > 1000 {
				text = string(runes[:1000]) + "…"
			}
			lines = append(lines, role+"："+text)
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "最近记录中没有文字消息。")
	}
	b.respond(input, strings.Join(lines, "\n\n"), input.ThreadID)
}

func (b *Bridge) signalNext() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Bridge) enqueue(input Input, text string, handoff bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queue) >= 64 {
		b.respond(input, "队列已满，请稍后重试。", input.ThreadID)
		return
	}
	id, err := randomToken(16)
	if err != nil {
		b.fail(err)
		return
	}
	q := queued{ID: id, Input: input, Text: text, Handoff: handoff, Created: time.Now().Unix()}
	if err := state.WritePrivate(filepath.Join(b.cfg.StateDir, "queue-"+id+".json"), q); err != nil {
		b.fail(errors.New("cannot save queued task"))
		return
	}
	b.queue = append(b.queue, q)
	b.refreshReplyStatusLocked()
	b.respond(input, fmt.Sprintf("任务 %s 已排队，前面有 %d 个等待任务。内容：%s\n/queue 查看；/cancelone %s 只取消本条。", id, len(b.queue)-1, b.shortSummary(text), id), input.ThreadID)
	if b.active == nil {
		b.signalNext()
	}
}

func (b *Bridge) startNext(ctx context.Context) {
	if !b.beginOperation() {
		return
	}
	defer b.endOperation()
	b.mu.Lock()
	if b.active != nil || len(b.queue) == 0 {
		b.mu.Unlock()
		return
	}
	q := b.queue[0]
	b.queue = b.queue[1:]
	b.refreshReplyStatusLocked()
	b.mu.Unlock()
	if err := os.Remove(filepath.Join(b.cfg.StateDir, "queue-"+q.ID+".json")); err != nil {
		b.fail(errors.New("cannot claim queued task"))
		return
	}
	// Its target was captured when submitted; never look up a newly selected default.
	q.Input.queued = true
	b.prompt(ctx, q.Input, q.Text, q.Handoff)
}

func (b *Bridge) cancelQueued(input Input) {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	kept := b.queue[:0]
	for _, q := range b.queue {
		if q.Input.ThreadID == input.ThreadID && q.Input.ChatID == input.ChatID && q.Input.UserID == input.UserID {
			if err := os.Remove(filepath.Join(b.cfg.StateDir, "queue-"+q.ID+".json")); err != nil {
				b.fail(errors.New("cannot cancel queued task"))
				return
			}
			b.markInput(q.Input.MessageID, "handled")
			count++
		} else {
			kept = append(kept, q)
		}
	}
	b.queue = kept
	b.refreshReplyStatusLocked()
	b.respond(input, fmt.Sprintf("已取消 %d 个排队任务。", count), input.ThreadID)
}

func (b *Bridge) recoverQueue() error {
	paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, "queue-*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return errors.New("cannot read queued task")
		}
		var q queued
		if json.Unmarshal(data, &q) != nil || filepath.Join(b.cfg.StateDir, "queue-"+q.ID+".json") != path {
			return errors.New("invalid queued task")
		}
		_, receiptErr := os.Lstat(b.inputPath(q.Input.MessageID))
		if b.authorized(q.Input.UserID) && os.IsNotExist(receiptErr) {
			b.respond(q.Input, "桥接已重启：此前排队任务未自动执行，请检查后重新发送。", q.Input.ThreadID)
		}
		if err := os.Rename(path, filepath.Join(b.cfg.StateDir, "cancelled-queue-"+q.ID+".json")); err != nil {
			return errors.New("cannot retain interrupted queue")
		}
	}
	return nil
}

func (b *Bridge) workspaceAllowed(cwd string) bool {
	if cwd == "" {
		return true
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	roots := []string{b.cfg.ProjectDir}
	if b.cfg.Sandbox == "workspace-write" {
		roots = append(roots, b.cfg.WritableRoots...)
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
