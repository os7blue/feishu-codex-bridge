package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
)

func modelDescription(s modelsettings.Effective) string {
	return fmt.Sprintf("模型：%s（来源：%s）\n思考强度：%s（来源：%s）", s.Model, s.ModelSource, s.Effort, s.EffortSource)
}

func (b *Bridge) nextModelSettings(ctx context.Context, thread, owner string) (modelsettings.Effective, error) {
	return b.models.Snapshot(ctx, modelsettings.Scope{ThreadID: thread, OwnerID: owner})
}

// Session details show the active turn's captured settings separately from the
// next-turn configuration. Never hold the task mutex during metadata RPCs.
func (b *Bridge) sessionModelInfo(ctx context.Context, input Input, local ...bool) string {
	session, ok := b.store.Session(input.ThreadID)
	if !ok || session.OwnerID != input.UserID {
		return "模型配置：无法确认会话归属。"
	}
	if session.OriginHome != "" || !b.workspaceAllowed(session.Cwd) {
		return "外部只读会话：未读取原客户端的模型配置；创建授权副本后使用本桥接设置。"
	}
	var lines []string
	b.mu.Lock()
	if a := b.active; a != nil && a.thread == input.ThreadID && a.owner == input.UserID && a.chat == input.ChatID {
		if a.modelSettings.Model != "" {
			label := "当前轮实际使用："
			if a.turn == "" {
				label = "当前轮已选配置（等待启动确认）："
			}
			lines = append(lines, label+"\n"+modelDescription(a.modelSettings))
		} else {
			lines = append(lines, "当前轮模型配置：启动中，尚未确认。")
		}
	}
	b.mu.Unlock()
	if len(local) > 0 && local[0] {
		global, overrides := b.store.ModelPreferences()
		own := overrides[input.ThreadID]
		next := modelsettings.Effective{Model: own.Model, Effort: own.Effort, ModelSource: "会话覆盖", EffortSource: "会话覆盖"}
		if next.Model == "" {
			next.Model, next.ModelSource = global.Model, "桥接全局默认"
		}
		if next.Model == "" {
			next.Model, next.ModelSource = b.cfg.Model, "桥接启动配置"
		}
		if next.Model == "" {
			next.Model, next.ModelSource = "继承账号默认", "启动时核验"
		}
		if next.Effort == "" {
			next.Effort, next.EffortSource = global.Effort, "桥接全局默认"
		}
		if next.Effort == "" {
			next.Effort, next.EffortSource = "继承模型默认", "启动时核验"
		}
		lines = append(lines, "下一轮模型配置（本地保存值，启动时核验）：\n"+modelDescription(next))
		return strings.Join(lines, "\n\n")
	}
	view, err := b.models.Inspect(ctx, modelsettings.Scope{ThreadID: input.ThreadID, OwnerID: input.UserID})
	if err != nil {
		lines = append(lines, "下一轮模型配置暂不可用："+err.Error())
	} else if view.Problem != nil {
		lines = append(lines, "下一轮模型配置尚未有效："+view.Problem.Error())
	} else {
		lines = append(lines, "下一轮模型配置（启动时重新核验）：\n"+modelDescription(view.Effective))
	}
	return strings.Join(lines, "\n\n")
}

func (b *Bridge) respondSessionInfo(input Input, text string) {
	session, ok := b.store.Session(input.ThreadID)
	if !ok || session.OwnerID != input.UserID {
		b.respond(input, text)
		return
	}
	if !b.rememberInputRoute(input) {
		return
	}
	target := fmt.Sprintf("#%d", session.Ref)
	choices := []replyChoice{
		{"commands", "/commands " + target + "  会话操作", "/commands " + target},
		{"history", "/history " + target + "  最近消息", "/history " + target},
		{"status", "/status " + target + "  会话状态", "/status " + target},
	}
	if session.OriginHome == "" && b.workspaceAllowed(session.Cwd) {
		choices = append(choices, replyChoice{"model", "/model " + target + "  模型设置", "/model " + target}, replyChoice{"effort", "/effort " + target + "  思考强度设置", "/effort " + target})
	}
	b.emitChoices(input, text, choices, input.ThreadID)
}

const settingsUsage = "用法：/models 查看可用项；/model global 模型、/effort global 强度设置本桥接全局默认；/model #编号 模型、/effort #编号 强度设置单会话。会话自定义会固定模型和强度，不受全局修改影响；会话任一项写 default 恢复整套继承，global 的 default 只重置对应项；省略值查看设置。"

func (b *Bridge) modelCommand(ctx context.Context, input Input, command, rest string) bool {
	if command != "/models" && command != "/model" && command != "/effort" {
		return false
	}
	if command == "/models" {
		page := 1
		if rest != "" {
			n, err := strconv.Atoi(rest)
			if err != nil || n < 1 || n > 1000 {
				b.respond(input, "用法：/models [页码]")
				return true
			}
			page = n
		}
		models, err := b.models.Models(ctx)
		if err != nil {
			b.respond(input, err.Error())
			return true
		}
		lines := []string{"当前账号可用模型（读取目录不发起模型推理）："}
		if (page-1)*18 >= len(models) {
			b.respond(input, "没有这一页模型；/models 返回首页。")
			return true
		}
		for i, m := range models {
			if i < (page-1)*18 || i >= page*18 {
				continue
			}
			var efforts []string
			for _, e := range m.Efforts {
				efforts = append(efforts, e)
			}
			lines = append(lines, fmt.Sprintf("%s：%s；默认强度 %s", m.Model, strings.Join(efforts, ", "), m.DefaultEffort))
		}
		lines = append(lines, settingsUsage)
		choices := []replyChoice{{"global", "调整全局默认", "/model global"}}
		if page > 1 {
			choices = append(choices, replyChoice{"previous", "上一页", fmt.Sprintf("/models %d", page-1)})
		}
		if page*18 < len(models) {
			choices = append(choices, replyChoice{"next", "下一页", fmt.Sprintf("/models %d", page+1)})
		}
		b.emitChoices(input, strings.Join(lines, "\n"), choices, input.ThreadID)
		return true
	}
	word, value := splitWord(rest)
	explicitGlobal := word == "global"
	if explicitGlobal {
		input.ThreadID = ""
	} else if b.isSessionRef(word) {
		if !b.target(&input, word) {
			return true
		}
	} else {
		value = rest
		if !b.routeInput(&input) {
			return true
		}
	}
	page := 0
	if strings.HasPrefix(value, "page ") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(value, "page ")))
		if err != nil || n < 1 || n > 1000 {
			b.respond(input, "模型页码无效。")
			return true
		}
		page, value = n-1, ""
	}
	if len(strings.Fields(value)) > 1 || value != "" && !modelsettings.ValidToken(value) {
		b.respond(input, settingsUsage)
		return true
	}
	if input.ThreadID == "" && value != "" && !explicitGlobal {
		b.respond(input, "未选定会话；修改全局默认必须明确写 global。\n"+settingsUsage)
		return true
	}
	if input.ThreadID != "" {
		session, ok := b.store.Session(input.ThreadID)
		if !ok || session.OwnerID != input.UserID {
			b.respond(input, "会话归属无法确认，未读取或修改设置。")
			return true
		}
		if value != "" && (session.OriginHome != "" || !b.workspaceAllowed(session.Cwd)) {
			b.respond(input, "此会话只读，未修改设置；请先 /fork 创建授权范围内的副本。")
			return true
		}
	}
	scope := modelsettings.Scope{ThreadID: input.ThreadID, OwnerID: input.UserID}
	var view modelsettings.View
	var err error
	if value == "" {
		view, err = b.models.Inspect(ctx, scope)
	} else {
		chosen := value
		if chosen == "default" {
			chosen = ""
		}
		view, err = b.models.Change(ctx, scope, strings.TrimPrefix(command, "/"), chosen)
	}
	if err != nil {
		global, overrides := b.store.ModelPreferences()
		saved := global
		if input.ThreadID != "" {
			saved = overrides[input.ThreadID]
		}
		b.respond(input, err.Error()+fmt.Sprintf("\n已保存配置（未校验）：模型=%s，强度=%s；空值表示跟随默认。\n", saved.Model, saved.Effort)+settingsUsage)
		return true
	}
	models, effective := view.Models, view.Effective
	err = view.Problem
	target := "global"
	label := "本桥接全局默认（所有会话可分别覆盖）"
	if s, ok := b.store.Session(input.ThreadID); ok {
		target, label = fmt.Sprintf("#%d", s.Ref), fmt.Sprintf("会话 #%d", s.Ref)
		if view.Configured.Model != "" || view.Configured.Effort != "" {
			label += " · 会话独立配置"
		} else {
			label += " · 跟随全局"
		}
	}
	message := label + "\n"
	if value != "" {
		message += "已保存，从下一轮任务生效；排队任务在启动时读取设置。\n"
	}
	if err != nil {
		message += err.Error()
	} else {
		message += modelDescription(effective)
	}
	b.mu.Lock()
	if a := b.active; a != nil && a.owner == input.UserID && a.chat == input.ChatID && (input.ThreadID == "" || a.thread == input.ThreadID) {
		if a.modelSettings.Model != "" {
			message += "\n当前轮仍使用：" + a.modelSettings.Model + " / " + a.modelSettings.Effort
		}
	}
	b.mu.Unlock()
	message += "\n" + settingsUsage
	var choices []replyChoice
	if command == "/model" {
		for i, m := range models {
			if i < page*18 || i >= (page+1)*18 {
				continue
			}
			choices = append(choices, replyChoice{fmt.Sprintf("model_%d", i), m.Model, "/model " + target + " " + m.Model})
		}
		if page > 0 {
			choices = append(choices, replyChoice{"previous", "上一页模型", fmt.Sprintf("/model %s page %d", target, page)})
		}
		if (page+1)*18 < len(models) {
			choices = append(choices, replyChoice{"next", "下一页模型", fmt.Sprintf("/model %s page %d", target, page+2)})
		}
	} else if m, ok := modelsettings.Find(models, effective.Model); ok {
		for i, e := range m.Efforts {
			choices = append(choices, replyChoice{fmt.Sprintf("effort_%d", i), e, "/effort " + target + " " + e})
		}
	}
	resetLabel := "模型和强度一起跟随全局"
	if target == "global" {
		resetLabel = "恢复默认模型"
		if command == "/effort" {
			resetLabel = "恢复默认强度"
		}
	}
	choices = append(choices, replyChoice{"inherit", resetLabel, command + " " + target + " default"}, replyChoice{"models", "可用模型", "/models"})
	b.emitChoices(input, message, choices, input.ThreadID)
	return true
}
