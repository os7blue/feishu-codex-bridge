// Package commandset defines the public root commands used by help, cards and
// native command registration. Pairing and the /start alias stay out of discovery.
package commandset

import "strings"

type Command struct {
	Name        string `json:"command"`
	Description string `json:"description"`
	Usage       string `json:"usage"`
}

var catalog = []Command{
	{"sessions", "会话列表；支持 next 下一页、archived 已归档", "/sessions [next|archived]"},
	{"new", "新建会话；默认沿用当前项目，可指定授权目录", "/new [--cwd 绝对目录] [名称]"},
	{"status", "查看整体状态，或指定会话的状态", "/status [#编号]"},
	{"queue", "查看排队任务", "/queue"},
	{"help", "查看命令帮助", "/help"},
	{"models", "分页列出当前账号可用模型及思考强度", "/models [页码]"},
	{"model", "查看或设置模型；会话default整套跟随全局；page N翻页", "/model [global|#编号] [模型|default|page N]"},
	{"effort", "查看或设置强度；会话default整套跟随全局", "/effort [global|#编号] [强度|default]"},
	{"show", "查看会话信息", "/show [#编号]"},
	{"history", "查看会话最近文字消息", "/history [#编号]"},
	{"use", "设为普通消息的默认会话", "/use #编号"},
	{"rename", "重命名会话", "/rename [#编号] 新名称"},
	{"fork", "创建独立副本，原会话保留", "/fork [#编号]"},
	{"stop", "停止指定会话的活动任务", "/stop [#编号]"},
	{"cancel", "取消指定会话的排队任务", "/cancel [#编号]"},
	{"handoff", "为会话生成交接文档", "/handoff [#编号]"},
	{"archive", "归档会话", "/archive [#编号]"},
	{"unarchive", "恢复已归档会话", "/unarchive [#编号]"},
	{"commands", "打开可修改、手动发送的命令草稿卡片", "/commands [#编号]"},
	{"approve", "批准待处理的审批请求", "/approve 请求编号"},
	{"deny", "拒绝待处理的审批请求", "/deny 请求编号"},
	{"answer", "回答待处理的问题", "/answer 请求编号 答案"},
	{"file", "发送项目文件；也可直接发送附件给机器人", "/file [#编号] 文件路径"},
	{"projects", "从已知授权项目选择新建", "/projects [页码]"},
	{"continue", "沿用项目、交接文档和模型配置接续", "/continue [#编号]"},
	{"files", "查看会话最近附件与已登记成果", "/files [#编号]"},
	{"result", "取回当前聊天有权限的完整成果附件", "/result 结果编号"},
	{"cancelone", "仅取消一条等待任务，保留其他任务", "/cancelone 任务编号"},
}

func All() []Command { return append([]Command(nil), catalog...) }

func Help() string {
	var lines []string
	for _, command := range catalog {
		lines = append(lines, command.Usage+"："+command.Description)
	}
	return strings.Join(lines, "\n")
}
