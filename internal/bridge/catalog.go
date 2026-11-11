package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type snapshotWriter struct {
	writer          io.Writer
	total, lastLine int64
}

func (w *snapshotWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if i := bytes.LastIndexByte(p[:n], '\n'); i >= 0 {
		w.lastLine = w.total + int64(i+1)
	}
	w.total += int64(n)
	return n, err
}

// External Codex homes are read-only catalogs. Only an explicit /fork imports
// a bounded snapshot into the bridge's own home, never into the source client.
func (b *Bridge) externalCatalog(archived bool) ([]state.Session, error) {
	var sessions []state.Session
	seen := map[string]bool{}
	for _, home := range b.cfg.CatalogHomes {
		if home == b.cfg.CodexHome {
			continue
		}
		names := map[string]string{}
		if file, err := os.Open(filepath.Join(home, "session_index.jsonl")); err == nil {
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 4096), 256<<10)
			for scanner.Scan() {
				var row struct {
					ID   string `json:"id"`
					Name string `json:"thread_name"`
				}
				if json.Unmarshal(scanner.Bytes(), &row) == nil {
					names[row.ID] = row.Name
				}
			}
			file.Close()
		}
		folder := "sessions"
		if archived {
			folder = "archived_sessions"
		}
		base := filepath.Join(home, folder)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return errors.New("cannot read external session catalog")
			}
			if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return errors.New("cannot read external session metadata")
			}
			scanner := bufio.NewScanner(io.LimitReader(file, 256<<10))
			scanner.Buffer(make([]byte, 4096), 256<<10)
			var meta struct {
				Type    string `json:"type"`
				Payload struct {
					ID  string `json:"id"`
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			ok := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &meta) == nil && meta.Type == "session_meta"
			file.Close()
			if !ok || !validThreadID(meta.Payload.ID) || seen[meta.Payload.ID] {
				return nil
			}
			seen[meta.Payload.ID] = true
			sessions = append(sessions, state.Session{ThreadID: meta.Payload.ID, Name: names[meta.Payload.ID], Cwd: meta.Payload.Cwd, OriginHome: home, RolloutPath: path, Archived: archived})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ThreadID > sessions[j].ThreadID })
	return sessions, nil
}

func validThreadID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func (b *Bridge) externalSource(session state.Session) (*os.File, error) {
	allowed := false
	for _, home := range b.cfg.CatalogHomes {
		if home == session.OriginHome {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("external catalog is no longer configured")
	}
	resolved, err := filepath.EvalSymlinks(session.RolloutPath)
	if err != nil {
		return nil, errors.New("external session is unavailable")
	}
	rel, err := filepath.Rel(session.OriginHome, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("external session path left its catalog")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, errors.New("cannot open external session")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("external session must be a regular file")
	}
	return f, nil
}

func (b *Bridge) externalHistory(input Input, session state.Session) {
	file, err := b.externalSource(session)
	if err != nil {
		b.respond(input, "无法读取外部会话记录。", session.ThreadID)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		b.respond(input, "无法读取外部会话记录。", session.ThreadID)
		return
	}
	start := info.Size() - (2 << 20)
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		b.respond(input, "无法定位会话记录。", session.ThreadID)
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 2<<20))
	if err != nil {
		b.respond(input, "无法读取外部会话记录。", session.ThreadID)
		return
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		var event struct {
			Type    string `json:"type"`
			Payload struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type != "response_item" || (event.Payload.Role != "user" && event.Payload.Role != "assistant") {
			continue
		}
		text := ""
		for _, content := range event.Payload.Content {
			text += content.Text
		}
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > 1000 {
			text = string(runes[:1000]) + "…"
		}
		lines = append(lines, event.Payload.Role+"："+text)
		if len(lines) > 8 {
			lines = lines[1:]
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "最近记录中没有可展示的文字消息。")
	}
	b.respond(input, "外部会话，只读查看：\n"+strings.Join(lines, "\n\n"), session.ThreadID)
}

func (b *Bridge) forkSession(ctx context.Context, input Input) {
	session, ok := b.store.Session(input.ThreadID)
	if !ok {
		b.respond(input, "会话不存在。")
		return
	}
	if session.OriginHome != "" {
		if b.cfg.CodexHome == "" {
			b.respond(input, "外部会话复制需要先配置独立 codex_home。")
			return
		}
		if !b.workspaceAllowed(session.Cwd) {
			b.respond(input, "原会话项目在当前授权范围之外；可以查看，接续前需明确授权项目范围。", session.ThreadID)
			return
		}
		file, err := b.externalSource(session)
		if err != nil {
			b.respond(input, "无法读取外部会话快照。", session.ThreadID)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			b.respond(input, "无法读取外部会话快照。")
			return
		}
		dir := filepath.Join(b.cfg.CodexHome, "sessions", time.Now().Format("2006/01/02"))
		if os.MkdirAll(dir, 0700) != nil {
			b.respond(input, "无法准备私有会话快照。")
			return
		}
		path := filepath.Join(dir, filepath.Base(session.RolloutPath))
		_, alreadyExists := os.Stat(path)
		if alreadyExists == nil {
			if _, err := os.Stat(path + ".bridge-import"); err != nil {
				b.respond(input, "目标目录已有同 ID 会话，未覆盖。请使用现有会话编号。")
				return
			}
		} else {
			out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				b.respond(input, "无法保存私有会话快照。")
				return
			}
			writer := &snapshotWriter{writer: out}
			_, copyErr := io.CopyN(writer, file, info.Size())
			if copyErr == nil {
				if writer.lastLine == 0 {
					copyErr = errors.New("incomplete session metadata")
				} else {
					copyErr = out.Truncate(writer.lastLine)
				}
			}
			syncErr := out.Sync()
			closeErr := out.Close()
			if copyErr != nil || syncErr != nil || closeErr != nil {
				os.Remove(path)
				b.respond(input, "会话快照保存失败，原会话未修改。")
				return
			}
			if err := state.WritePrivate(path+".bridge-import", map[string]string{"source_thread": session.ThreadID}); err != nil {
				os.Remove(path)
				b.respond(input, "无法保存会话导入标记，未启动模型任务。")
				return
			}
		}
		// Import metadata via the official scan, before asking it to fork history.
		if _, err := b.rpcCall(ctx, "thread/list", map[string]any{"limit": 1, "modelProviders": []string{}, "sourceKinds": []string{"cli", "vscode", "exec", "appServer", "unknown"}}); err != nil {
			b.respond(input, "快照已保存，Codex 索引失败，未启动模型任务。")
			return
		}
	}
	params := b.sessionParams(session.Cwd)
	params["threadId"] = session.ThreadID
	params["excludeTurns"] = true
	raw, err := b.rpcCall(ctx, "thread/fork", params)
	var result struct {
		Thread threadSummary `json:"thread"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Thread.ID == "" || result.Thread.ID == session.ThreadID {
		b.respond(input, "会话复制失败，原会话未修改；没有启动模型任务。")
		return
	}
	summary := b.shortSummary(result.Thread.Preview)
	if summary == "" {
		summary = session.Summary
	}
	created, err := b.store.RememberSession(state.Session{ThreadID: result.Thread.ID, Cwd: result.Thread.Cwd, Name: result.Thread.Name, Summary: summary, OwnerID: input.UserID})
	if err != nil {
		b.fail(errors.New("cannot persist forked session"))
		return
	}
	b.mu.Lock()
	b.loaded[created.ThreadID] = true
	b.mu.Unlock()
	b.respond(input, fmt.Sprintf("已创建独立副本 #%d，原会话保留。直接回复本消息，或发送 #%d 内容，即可接续。", created.Ref, created.Ref), created.ThreadID)
}
