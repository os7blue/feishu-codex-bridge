// Package feishuui implements only Feishu card rendering. It has no session,
// scheduler, persistence, model or permission dependencies.
package feishuui

import (
	"encoding/json"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
	"strings"
)

type Renderer struct{}

func (Renderer) Format() string        { return "feishu-card-v1" }
func plain(text string) map[string]any { return map[string]any{"tag": "plain_text", "content": text} }
func cardNote(text string) map[string]any {
	return map[string]any{"tag": "note", "elements": []any{plain(text)}}
}
func cardContext(context string) []any {
	if strings.TrimSpace(context) == "" {
		return nil
	}
	if session, body, ok := strings.Cut(context, "\n\n"); ok {
		return []any{cardNote(session), map[string]any{"tag": "div", "text": plain(body)}}
	}
	return []any{map[string]any{"tag": "div", "text": plain(context)}}
}
func cardSection(title, hint string) []any {
	return []any{map[string]any{"tag": "hr"}, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": "**" + title + "**"}}, cardNote(hint)}
}
func clickableCommand(label, token, command string) map[string]any {
	text := plain(label)
	text["text_color"] = "blue"
	return map[string]any{"tag": "interactive_container", "behaviors": []any{map[string]any{"type": "callback", "value": map[string]string{"action": "draft_open", "token": token, "command": command}}}, "elements": []any{map[string]any{"tag": "div", "text": text}}}
}
func controls(v presentation.View) []any {
	var out []any
	for _, c := range v.Choices {
		out = append(out, clickableCommand(c.Label, v.Token, c.ID))
	}
	return out
}
func cardJSON(title string, elements []any) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"config": map[string]any{"wide_screen_mode": true, "update_multi": true}, "header": map[string]any{"title": plain(title), "template": "blue"}, "elements": elements})
	return data
}
func (Renderer) Render(v presentation.View) json.RawMessage {
	elements := cardContext(v.Context)
	switch v.Kind {
	case "menu":
		elements = append(elements, cardSection("选择命令", "点击高亮文字生成草稿；会话命令预填目标："+v.Target)...)
	case "request":
		elements = append(elements, cardSection("选择回复", "点击高亮命令生成草稿；手动发送后才提交。")...)
	case "panel":
		elements = append(elements, cardNote("点击高亮命令生成草稿，修改后手动发送。"))
	case "submitted":
		elements = append(elements, cardSection("已提交的内容", "已进入接收队列，处理结果将另行回复。")...)
		elements = append(elements, map[string]any{"tag": "div", "text": plain(v.Text)})
	case "draft":
		hint := "修改后手动发送才提交。30 分钟内有效，每份仅可发送一次。"
		if v.Request {
			hint = "修改后手动发送才提交。请保留请求编号；沿用原请求十分钟期限。"
		}
		elements = append(elements, cardSection("编辑并发送", hint)...)
		send := map[string]any{"tag": "button", "text": plain("发送"), "type": "primary", "name": "send_command", "action_type": "form_submit", "value": map[string]string{"action": "draft_send", "token": v.Token, "command": ""}}
		elements = append(elements, map[string]any{"tag": "form", "name": "command_form", "elements": []any{map[string]any{"tag": "input", "name": "command_text", "default_value": v.Text, "placeholder": plain("输入命令或任务"), "required": true, "max_length": 1000, "width": "fill"}, send}})
		if len(v.Choices) > 0 {
			elements = append(elements, cardSection("重新选择", "直接点击其他高亮命令，即可替换上方草稿。")...)
		}
	}
	elements = append(elements, controls(v)...)
	return cardJSON(v.Title, elements)
}
