package bridge

import (
	"fmt"
	"strings"
)

// Local control stays responsive while ordinary commands wait for metadata.
// These lanes do not start parallel model requests: prompt still owns active.
func controlInput(input Input) bool {
	command, rest := splitWord(input.Text)
	if strings.HasPrefix(command, "#") {
		command, _ = splitWord(rest)
	}
	switch command {
	case "/stop", "/approve", "/deny", "/answer", "/status", "/queue", "/cancel", "/cancelone", "/start", "/help":
		return true
	}
	return false
}

func (b *Bridge) home(input Input, intro ...string) {
	if !b.routeInput(&input) {
		return
	}
	text := "选择接下来要做的事：继续当前工作、选择项目新建、查看任务或调整设置。\n高亮入口生成草稿，手动发送后才提交。"
	if len(intro) > 0 {
		text = intro[0] + "\n\n" + text
	}
	choices := []replyChoice{{"projects", "选择项目 / 新建会话", "/projects"}, {"sessions", "查看会话", "/sessions"}, {"queue", "查看任务队列", "/queue"}, {"models", "模型设置", "/model global"}, {"help", "全部命令", "/help"}}
	if s, ok := b.store.Session(input.ThreadID); ok {
		text = "当前项目：" + s.Cwd + "\n默认会话：" + b.sessionLine(s.ThreadID) + "\n\n" + text
		choices = append([]replyChoice{{"status", "继续前查看当前任务", fmt.Sprintf("/status #%d", s.Ref)}, {"files", "附件与成果", fmt.Sprintf("/files #%d", s.Ref)}}, choices...)
	}
	b.emitChoices(input, text, choices, input.ThreadID)
}
