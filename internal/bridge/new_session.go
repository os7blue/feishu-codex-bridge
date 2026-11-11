package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Keep /new <name> compatible. The optional directory is a single argument;
// double quotes allow spaces without invoking a shell or expanding variables.
func newSessionArguments(text string) (cwd, name string, err error) {
	option, rest := splitWord(text)
	if option != "--cwd" {
		return "", strings.TrimSpace(text), nil
	}
	if strings.HasPrefix(rest, "\"") {
		quoted, quoteErr := strconv.QuotedPrefix(rest)
		if quoteErr != nil {
			return "", "", errors.New("目录引号不完整。")
		}
		cwd, err = strconv.Unquote(quoted)
		name = strings.TrimSpace(rest[len(quoted):])
		if name != "" && !strings.HasPrefix(rest[len(quoted):], " ") && !strings.HasPrefix(rest[len(quoted):], "\t") {
			return "", "", errors.New("目录和名称之间需要空格。")
		}
	} else {
		cwd, name = splitWord(rest)
	}
	if err != nil || !filepath.IsAbs(cwd) {
		return "", "", errors.New("请使用 /new --cwd 绝对目录 [名称]；含空格的目录用双引号包裹。")
	}
	return cwd, name, nil
}

func (b *Bridge) newSessionDirectory(input Input, explicit string) (string, bool) {
	cwd := explicit
	if cwd == "" {
		// Resolve the quoted/selected session without binding this creation
		// message to the old session. The successful reply routes to the new one.
		source := input
		source.MessageID, source.SourceCardID = "", ""
		if !b.routeInput(&source) {
			return "", false
		}
		cwd = b.cfg.ProjectDir
		if source.ThreadID != "" {
			s, ok := b.store.Session(source.ThreadID)
			if !ok || s.OwnerID != "" && s.OwnerID != input.UserID {
				b.respond(input, "无法确认原会话归属，未创建会话。")
				return "", false
			}
			if s.OriginHome != "" {
				b.respond(input, "外部只读会话不能直接继承目录；请用 /new --cwd 授权目录 创建新会话，或 /fork 创建副本。")
				return "", false
			}
			if s.Cwd != "" {
				cwd = s.Cwd
			}
		}
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil || !b.workspaceAllowed(resolved) {
		b.respond(input, "目录不存在或不在已授权范围内，未创建会话。原默认会话保持。")
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		b.respond(input, "请选择已存在的项目目录，未创建会话。原默认会话保持。")
		return "", false
	}
	return resolved, true
}
