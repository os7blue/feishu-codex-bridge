package bridge

import (
	"encoding/json"
	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
)

func (b *Bridge) render(v presentation.View, choices []replyChoice) json.RawMessage {
	for _, c := range choices {
		v.Choices = append(v.Choices, presentation.Choice{ID: c.id, Label: c.label})
	}
	return b.renderer.Render(v)
}
func (b *Bridge) menuCard(token, target, context string) json.RawMessage {
	return b.render(presentation.View{Kind: "menu", Title: "命令草稿", Token: token, Target: target, Context: context}, draftChoices(commandDraft{target: target}))
}
func (b *Bridge) replyChoicesCard(token string, grant commandDraft) json.RawMessage {
	title := "待审批操作"
	if len(grant.questionIDs) > 0 {
		title = "等待你的回答"
	}
	if grant.demo {
		title = "隔离交互演示"
	}
	return b.render(presentation.View{Kind: "request", Title: title, Context: grant.context, Token: token}, grant.replies)
}
func (b *Bridge) demoCard(token, context string) json.RawMessage {
	return b.render(presentation.View{Kind: "demo", Title: "点击命令测试", Context: context, Token: token}, []replyChoice{{"status", "/status", "/status"}})
}
