package bridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type resultRecord struct {
	Chat, Owner, Thread, Text, Name, FileKey string
	Created                                  int64
}

func productID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (b *Bridge) productCommand(ctx context.Context, input Input, command, rest string) bool {
	switch command {
	case "/cancelone":
		b.cancelTask(input, strings.TrimSpace(rest))
	case "/projects":
		b.projects(input, rest)
	case "/files":
		if rest != "" {
			if !b.target(&input, strings.TrimSpace(rest)) {
				return true
			}
		} else if !b.routeInput(&input) {
			return true
		}
		b.filesPanel(input)
	case "/result":
		b.result(input, strings.TrimSpace(rest))
	case "/continue":
		if rest != "" {
			if !b.target(&input, strings.TrimSpace(rest)) {
				return true
			}
		} else if !b.routeInput(&input) {
			return true
		}
		s, ok := b.store.Session(input.ThreadID)
		if !ok || s.OwnerID != input.UserID || s.OriginHome != "" || !b.workspaceAllowed(s.Cwd) {
			b.respond(input, "请选择已授权的本项目会话接续。")
			return true
		}
		b.mu.Lock()
		active := b.active != nil && b.active.thread == s.ThreadID
		b.mu.Unlock()
		if active {
			b.respond(input, "原会话仍在执行，请先完成交接再接续。", s.ThreadID)
			return true
		}
		if _, _, err := ReadExport(b.cfg, s.Cwd, "docs/HANDOFF.md"); err != nil {
			b.respond(input, "未找到可读取的交接文档，请先生成交接。", s.ThreadID)
			return true
		}
		b.newSession(ctx, input, "接续 · "+b.sessionLabel(s), true)
	default:
		return false
	}
	return true
}

func (b *Bridge) cancelTask(input Input, id string) {
	if !productID(id) {
		b.respond(input, "用法：/cancelone 任务编号；/queue 查看每条任务。")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, q := range b.queue {
		if q.ID != id || q.Input.ChatID != input.ChatID || q.Input.UserID != input.UserID {
			continue
		}
		if err := os.Remove(filepath.Join(b.cfg.StateDir, "queue-"+q.ID+".json")); err != nil {
			b.respond(input, "取消未确认，任务保持；请刷新队列。")
			return
		}
		b.queue = append(b.queue[:i], b.queue[i+1:]...)
		b.refreshReplyStatusLocked()
		b.markInput(q.Input.MessageID, "handled")
		b.respond(input, "仅此任务已取消："+b.shortSummary(q.Text)+"。其他排队和已执行的修改保留。", q.Input.ThreadID)
		return
	}
	b.respond(input, "没有可取消的这条排队任务；它可能已开始、已取消或不属于当前聊天。/queue 查看等待任务，/status 查看执行状态。")
}

func (b *Bridge) projects(input Input, rest string) {
	page := 1
	if rest != "" {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 {
			b.respond(input, "用法：/projects [页码]")
			return
		}
		page = n
	}
	paths := map[string]bool{}
	if b.workspaceAllowed(b.cfg.ProjectDir) {
		paths[b.cfg.ProjectDir] = true
	}
	for _, s := range b.store.Sessions(input.UserID) {
		if s.OriginHome == "" && b.workspaceAllowed(s.Cwd) && s.Cwd != "" {
			paths[s.Cwd] = true
		}
	}
	var list []string
	for p := range paths {
		list = append(list, p)
	}
	sort.Strings(list)
	var choices []replyChoice
	start := (page - 1) * 12
	if start >= len(list) {
		b.respond(input, "没有这一页项目，/projects 返回首页。")
		return
	}
	for i := start; i < len(list) && i < start+12; i++ {
		p := list[i]
		choices = append(choices, replyChoice{fmt.Sprintf("project_%d", i), filepath.Base(p) + " · " + p, "/new --cwd " + strconv.Quote(p)})
	}
	if start+12 < len(list) {
		choices = append(choices, replyChoice{"next", "下一页项目", fmt.Sprintf("/projects %d", page+1)})
	}
	if page > 1 {
		choices = append(choices, replyChoice{"previous", "上一页项目", fmt.Sprintf("/projects %d", page-1)})
	}
	b.emitChoices(input, "选择已知项目新建独立会话：默认使用全局模型配置。\n这些项目来自默认项目和你已有的授权会话，没有扫描其他目录。其他已授权目录可填写 /new --cwd 绝对目录 名称。", choices, "")
}

// The application reads its own owner-scoped records. No arbitrary paths are
// accepted, and legacy result JSON without ownership is deliberately excluded.
func (b *Bridge) readResult(id string) (resultRecord, bool) {
	var r resultRecord
	if !productID(id) {
		return r, false
	}
	for _, prefix := range []string{"result-", "artifact-"} {
		path := filepath.Join(b.cfg.StateDir, prefix+id+".json")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &r) != nil {
			continue
		}
		if r.Owner != "" && r.Chat != "" && r.Created > 0 && (r.FileKey == "" || r.FileKey == id) && r.Name == safeFileName(r.Name) {
			return r, true
		}
	}
	return resultRecord{}, false
}

func (b *Bridge) result(input Input, id string) {
	r, ok := b.readResult(id)
	if !ok || r.Owner != input.UserID || r.Chat != input.ChatID {
		b.respond(input, "结果不可用或不属于当前聊天；/files 查看本会话成果。")
		return
	}
	input.ThreadID = r.Thread
	if r.Thread != "" {
		s, exists := b.store.Session(r.Thread)
		if !exists || s.OwnerID != input.UserID {
			b.respond(input, "无法确认成果所属会话。")
			return
		}
	}
	data := []byte(r.Text)
	if r.FileKey != "" {
		path := filepath.Join(b.cfg.StateDir, "artifact-data-"+r.FileKey+".bin")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
			b.respond(input, "成果快照不可用，请重新生成或发送。")
			return
		}
		var errRead error
		data, errRead = os.ReadFile(path)
		if errRead != nil {
			b.respond(input, "成果读取失败。")
			return
		}
	}
	if len(data) == 0 || len(data) > MaxFileBytes {
		b.respond(input, "成果为空或超过附件大小上限。")
		return
	}
	b.queueFile(input, r.Name, data, true)
}

func (b *Bridge) filesPanel(input Input) {
	if input.ThreadID == "" {
		b.respond(input, "请先选择会话，或发送 /files #编号。")
		return
	}
	s, ok := b.store.Session(input.ThreadID)
	if !ok || s.OwnerID != input.UserID {
		b.respond(input, "无法确认会话归属。")
		return
	}
	var choices []replyChoice
	b.fileMu.Lock()
	for i := len(b.receivedFiles) - 1; i >= 0 && len(choices) < 10; i-- {
		f := b.receivedFiles[i]
		if f.Chat == input.ChatID && f.Owner == input.UserID && f.Thread == input.ThreadID {
			choices = append(choices, replyChoice{fmt.Sprintf("attachment_%d", i), f.Name + " · 收件 " + time.Unix(f.Created, 0).Format("01-02 15:04"), fmt.Sprintf("/file #%d %s", s.Ref, f.Path)})
		}
	}
	b.fileMu.Unlock()
	var results []struct {
		id string
		r  resultRecord
	}
	for _, prefix := range []string{"result-", "artifact-"} {
		paths, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, prefix+"*.json"))
		for _, p := range paths {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), prefix), ".json")
			r, valid := b.readResult(id)
			if valid && r.Chat == input.ChatID && r.Owner == input.UserID && r.Thread == input.ThreadID {
				results = append(results, struct {
					id string
					r  resultRecord
				}{id, r})
			}
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].r.Created > results[j].r.Created })
	seen := map[string]bool{}
	for _, r := range results {
		if len(choices) >= 20 {
			break
		}
		if seen[r.id] {
			continue
		}
		seen[r.id] = true
		choices = append(choices, replyChoice{"result_" + r.id, r.r.Name + " · 成果 " + r.id[:8] + " · " + time.Unix(r.r.Created, 0).Format("01-02 15:04"), "/result " + r.id})
	}
	text := "本会话最近附件与已登记成果（最多20项），点击生成取回草稿；同名文件按时间与结果编号区分。模型仅写到项目、尚未发送的文件可用 /file #编号 路径 登记并取回。"
	if len(choices) == 0 {
		text = "本会话暂无已登记附件或成果；可发送附件，或用 /file #编号 路径 取回项目文件。"
	}
	b.emitChoices(input, text, choices, input.ThreadID)
}
