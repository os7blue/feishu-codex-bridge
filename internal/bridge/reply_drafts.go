package bridge

import (
	"encoding/json"
	"fmt"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type replyChoice struct{ id, label, text string }

func draftChoices(grant commandDraft) []replyChoice {
	if grant.request != "" || len(grant.replies) > 0 {
		return grant.replies
	}
	var choices []replyChoice
	for _, command := range commandTemplates {
		if grant.demo && command.id != "status" {
			continue
		}
		text := command.text
		if command.scoped {
			text += " " + grant.target
		}
		choices = append(choices, replyChoice{command.id, text + "  " + command.label, text})
	}
	return choices
}

// Caller holds draftMu. Re-selecting replaces the old editable grant, keeping
// the request and deadline while making the previous form unable to submit.
func (b *Bridge) selectDraft(grant commandDraft, text, previous string) CardResult {
	wasMenu := grant.menu
	// A global session row has no thread until a target is selected. Capture
	// that visible target without changing ownership, bindings or starting work.
	if grant.request == "" && !grant.demo {
		_, rest := splitWord(text)
		ref, _ := splitWord(rest)
		if strings.HasPrefix(ref, "#") {
			if number, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil {
				if session, ok := b.store.SessionByRef(number); ok && (session.OwnerID == "" || session.OwnerID == grant.owner) {
					grant.thread, grant.target = session.ThreadID, ref
				}
			}
		}
	}
	// Ordinary command panels do not repeat the source result or session list.
	// Request previews remain visible while reviewing an approval or answer.
	if grant.request == "" && !grant.demo {
		grant.context = ""
		if grant.thread != "" {
			grant.context = b.sessionLine(grant.thread)
		}
		command, rest := splitWord(text)
		target, value := splitWord(rest)
		if command == "/continue" {
			global, overrides := b.store.ModelPreferences()
			p := overrides[grant.thread]
			if p.Model == "" {
				p.Model = global.Model
			}
			if p.Effort == "" {
				p.Effort = global.Effort
			}
			grant.context += fmt.Sprintf("\n接续预览：沿用本会话项目、docs/HANDOFF.md及模型/强度 %s / %s；未指定值提交时核验。手动发送后创建新会话并开始读取交接文档，原会话保留。", p.Model, p.Effort)
		}
		if command == "/new" {
			cwd, _, err := newSessionArguments(rest)
			if err == nil && cwd != "" {
				grant.context += "\n新建项目：" + cwd + "。这是独立会话，使用全局默认模型和强度。"
			}
		}
		if command == "/model" || command == "/effort" {
			global, overrides := b.store.ModelPreferences()
			if target == "global" {
				independent := 0
				for _, p := range overrides {
					if p.Model != "" || p.Effort != "" {
						independent++
					}
				}
				grant.context += fmt.Sprintf("\n影响范围：修改桥接全局默认，仅跟随全局的会话在下一轮使用；%d个自定义会话保持独立。当前轮不变。", independent)
			} else if value == "default" {
				old := overrides[grant.thread]
				grant.context += fmt.Sprintf("\n整套恢复继承：模型和思考强度都会变化。\n当前独立：%s / %s\n改为跟随全局：%s / %s（空值使用启动或账号默认，提交时核验）。当前轮不变。", old.Model, old.Effort, global.Model, global.Effort)
			}
		}
	}
	grant.menu, grant.submitted = false, false
	token, ok := b.newDraft(grant)
	if !ok {
		return CardResult{Message: "草稿数量已达上限，请稍后重试。", Error: true}
	}
	if !wasMenu {
		b.closeDraft(previous, grant, "这份草稿已被新的选择替换，旧版本未提交。")
	}
	return CardResult{Message: "命令已生成，可修改或点击其他高亮命令；手动发送后才提交。", Card: b.render(presentation.View{Kind: "draft", Title: "编辑命令草稿", Token: token, Text: text, Context: grant.context, Request: grant.request != ""}, draftChoices(grant))}
}

// Caller holds mu; callbacks only hold draftMu. Always take mu before draftMu,
// and never call RPC from a card callback. The ordinary answer handler rechecks
// the live request when an edited reply leaves the inbox.
func (b *Bridge) forgetPending(token string) {
	delete(b.pending, token)
	b.refreshReplyStatusLocked()
	b.draftMu.Lock()
	defer b.draftMu.Unlock()
	for key, grant := range b.drafts {
		if grant.request == token {
			b.closeDraft(key, grant, "原请求已结束或已处理；此点击不会再次执行。请查看原会话状态确认结果。")
		}
	}
}

func validRequestReply(grant commandDraft, text string) bool {
	command, rest := splitWord(text)
	token, body := splitWord(rest)
	if token != grant.request {
		return false
	}
	if len(grant.questionIDs) == 0 {
		return (command == "/approve" || command == "/deny") && body == ""
	}
	if command != "/answer" || strings.TrimSpace(body) == "" {
		return false
	}
	if len(grant.questionIDs) == 1 {
		return true
	}
	var answers map[string]string
	if json.Unmarshal([]byte(body), &answers) != nil {
		return false
	}
	for _, id := range grant.questionIDs {
		if strings.TrimSpace(answers[id]) == "" {
			return false
		}
	}
	return true
}

// replyMenu retains the original prompt and uses opaque choice IDs in callbacks.
// The callback cannot supply an arbitrary request ID, answer or approval command.
// Caller holds mu. UI grants expire at the request's original deadline.
func (b *Bridge) replyMenu(requestToken, prompt string, request pending) json.RawMessage {
	if b.cfg.AppSecret != "" {
		prompt = strings.ReplaceAll(prompt, b.cfg.AppSecret, "[REDACTED]")
	}
	// Replace the original reply instruction lines with clickable command lines,
	// rather than duplicating them with a separate group of controls.
	marker := "\n允许：/approve " + requestToken
	if request.method == "item/tool/requestUserInput" {
		marker = "\n回复：/answer " + requestToken
	}
	if index := strings.LastIndex(prompt, marker); index >= 0 {
		prompt = prompt[:index] + "\n此请求十分钟内有效。"
	}
	grant := commandDraft{chat: request.chat, owner: request.owner, request: requestToken,
		context: b.sessionLine(request.thread) + "\n\n" + prompt, menu: true, expires: request.created.Add(10 * time.Minute)}
	if request.method == "item/tool/requestUserInput" {
		for _, q := range request.questions {
			grant.questionIDs = append(grant.questionIDs, q.ID)
		}
		prefix := "/answer " + requestToken + " "
		if len(request.questions) == 1 {
			for i, option := range request.questions[0].Options {
				label := option.Label
				if b.cfg.AppSecret != "" {
					label = strings.ReplaceAll(label, b.cfg.AppSecret, "[REDACTED]")
				}
				if len(grant.replies) >= 15 {
					break
				}
				if strings.TrimSpace(label) == "" || utf8.RuneCountInString(prefix+label) > 1000 {
					continue
				}
				grant.replies = append(grant.replies, replyChoice{fmt.Sprintf("option_%d", i), "回复：" + prefix + label, prefix + label})
			}
			grant.replies = append(grant.replies, replyChoice{"custom", "回复：" + prefix + "答案", prefix})
		} else {
			answers := map[string]string{}
			for _, q := range request.questions {
				answers[q.ID] = ""
			}
			body, _ := json.Marshal(answers)
			if utf8.RuneCountInString(prefix+string(body)) > 1000 {
				return nil
			}
			grant.replies = []replyChoice{{"custom", "回复：" + prefix + string(body), prefix + string(body)}}
		}
	} else {
		grant.replies = []replyChoice{{"approve", "允许：/approve " + requestToken, "/approve " + requestToken}, {"deny", "拒绝：/deny " + requestToken, "/deny " + requestToken}}
	}
	b.draftMu.Lock()
	defer b.draftMu.Unlock()
	token, ok := b.newDraft(grant)
	if !ok {
		return nil
	}
	card := b.replyChoicesCard(token, grant)
	// Long previews still use the existing segmented text delivery.
	if len(card) > 24000 {
		delete(b.drafts, token)
		return nil
	}
	return card
}
