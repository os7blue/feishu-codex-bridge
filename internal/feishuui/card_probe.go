package feishuui

import (
	"encoding/json"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
	"strings"
)

type CardSample struct {
	Name string
	Card json.RawMessage
}

// These public, fixed samples contain no grants, request IDs or user data.
// Clicks cannot resolve a pending request or execute a command.
func CardSamples() []CardSample {
	token := strings.Repeat("0", 32)
	hint := "卡片格式检查；本卡片不会执行命令，点击不会产生有效回复。"
	styled := clickableCommand("/status", token, "status")
	minimal := map[string]any{
		"tag": "interactive_container", "behaviors": styled["behaviors"],
		"elements": []any{map[string]any{"tag": "div", "text": plain("/status · 点击格式检查")}},
	}
	return []CardSample{
		{"basic", cardJSON("卡片格式检查：基础", []any{map[string]any{"tag": "div", "text": plain(hint)}})},
		{"styled_command", cardJSON("卡片格式检查：命令文字", []any{map[string]any{"tag": "div", "text": plain(hint)}, styled})},
		{"minimal_command", cardJSON("卡片格式检查：基础交互", []any{map[string]any{"tag": "div", "text": plain(hint)}, minimal})},
		{"form", (Renderer{}).Render(presentation.View{Kind: "draft", Title: "编辑命令草稿", Token: token, Text: "/status", Context: hint})},
	}
}
