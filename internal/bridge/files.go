package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

const MaxFileBytes = 20 << 20

type Attachment struct {
	Kind, Key, Name string
	Size            int64
}
type fileTransport = FileTransport

func inside(root, path string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func safeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." || name == "/" || strings.TrimSpace(name) == "" {
		return "attachment.bin"
	}
	if len(name) > 200 {
		end := 200
		for end > 0 && !utf8.RuneStart(name[end]) {
			end--
		}
		name = name[:end]
	}
	return name
}

func privateExport(c config.Config, path string) bool {
	if path == c.ConfigPath || inside(c.StateDir, path) || inside(c.CodexHome, path) {
		return true
	}
	for _, home := range c.CatalogHomes {
		if inside(home, path) {
			return true
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		part = strings.ToLower(part)
		switch part {
		case ".git", ".ssh", ".aws", ".codex", "codex-home", "feishu-bridge", "credentials", "auth.json", "config.toml", "id_rsa", "id_ed25519":
			return true
		}
		if part == ".env" || strings.HasPrefix(part, ".env.") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") {
			return true
		}
	}
	return false
}

// Snapshot a regular workspace file before upload. Check policy before opening
// and inode identity before reading; root-relative open cannot escape its root.
func ReadExport(c config.Config, cwd, path string) (string, []byte, error) {
	fail := errors.New("file unavailable, private, outside workspace, empty or larger than 20 MiB")
	if cwd == "" {
		cwd = c.ProjectDir
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || privateExport(c, filepath.Clean(path)) || privateExport(c, resolved) {
		return "", nil, fail
	}
	roots := append([]string{c.ProjectDir}, c.WritableRoots...)
	var root string
	for _, r := range roots {
		if inside(r, resolved) && len(r) > len(root) {
			root = r
		}
	}
	if root == "" {
		return "", nil, fail
	}
	before, err := os.Lstat(resolved)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > MaxFileBytes {
		return "", nil, fail
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", nil, fail
	}
	defer dir.Close()
	rel, _ := filepath.Rel(root, resolved)
	f, err := dir.Open(rel)
	if err != nil {
		return "", nil, fail
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", nil, fail
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil || len(data) == 0 || len(data) > MaxFileBytes || (c.AppSecret != "" && bytes.Contains(data, []byte(c.AppSecret))) {
		return "", nil, fail
	}
	after, err := f.Stat()
	if err != nil || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return "", nil, fail
	}
	return safeFileName(resolved), data, nil
}

func (b *Bridge) fileCommand(ctx context.Context, input Input, rest string) {
	ref, body := splitWord(rest)
	if strings.HasPrefix(ref, "#") {
		if !b.target(&input, ref) {
			return
		}
		rest = body
	} else if !b.routeInput(&input) {
		return
	}
	if strings.TrimSpace(rest) == "" {
		b.respond(input, "用法：/file [#编号] 文件路径。相对路径按目标会话项目解析；也可直接向机器人发送文件或图片。单文件上限20 MiB。")
		return
	}
	cwd := b.cfg.ProjectDir
	if s, ok := b.store.Session(input.ThreadID); ok && s.Cwd != "" {
		cwd = s.Cwd
	}
	name, data, err := ReadExport(b.cfg, cwd, strings.TrimSpace(rest))
	if err != nil {
		b.respond(input, "文件未发送：请检查路径、授权范围和大小（1字节至20 MiB）；私有配置、凭证和会话文件不能导出。", input.ThreadID)
		return
	}
	b.queueFile(input, name, data)
}

// Only validated exports or owner-scoped result records may enter this port.
func (b *Bridge) queueFile(input Input, name string, data []byte, reissue ...bool) {
	if _, ok := b.sender.(fileTransport); !ok {
		b.respond(input, "当前发送端不支持文件传输。", input.ThreadID)
		return
	}
	key, err := randomToken(16)
	if err != nil {
		b.fail(err)
		return
	}
	snapshot := filepath.Join(b.cfg.StateDir, "attachment-out-"+key+".bin")
	f, err := os.OpenFile(snapshot, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		b.fail(errors.New("cannot retain outgoing file"))
		return
	}
	digest := sha256.Sum256(data)
	item := outbound{Chat: input.ChatID, Key: key, Created: time.Now().UnixNano(), ThreadID: input.ThreadID, OwnerID: input.UserID, FilePath: snapshot, FileName: name, FileDigest: hex.EncodeToString(digest[:])}
	if len(reissue) == 0 || !reissue[0] {
		if err := os.Link(snapshot, filepath.Join(b.cfg.StateDir, "artifact-data-"+key+".bin")); err != nil {
			b.fail(errors.New("cannot retain artifact data"))
			return
		}
		if err := state.WritePrivate(filepath.Join(b.cfg.StateDir, "artifact-"+key+".json"), resultRecord{Chat: input.ChatID, Owner: input.UserID, Thread: input.ThreadID, Name: name, Created: time.Now().Unix(), FileKey: key}); err != nil {
			b.fail(errors.New("cannot retain artifact record"))
			return
		}
	}
	if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
		b.fail(errors.New("cannot retain outgoing file receipt"))
		return
	}
	select {
	case b.out <- item:
	default:
		b.fail(errors.New("outgoing queue full; file retained"))
	}
}

func (b *Bridge) sendFile(ctx context.Context, item outbound) ([]string, error) {
	if item.FilePath != filepath.Join(b.cfg.StateDir, "attachment-out-"+item.Key+".bin") || safeFileName(item.FileName) != item.FileName {
		return nil, errors.New("invalid outgoing file receipt")
	}
	info, err := os.Lstat(item.FilePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return nil, errors.New("file snapshot unavailable")
	}
	data, err := os.ReadFile(item.FilePath)
	if err != nil {
		return nil, errors.New("file snapshot unavailable")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != item.FileDigest {
		return nil, errors.New("file snapshot changed")
	}
	sender, ok := b.sender.(fileTransport)
	if !ok {
		return nil, errors.New("file transport unavailable")
	}
	return sender.SendFile(ctx, item.Chat, item.FileName, data, item.Key)
}

func (b *Bridge) receiveFile(ctx context.Context, input Input) {
	if !b.routeInput(&input) {
		return
	}
	cwd := b.cfg.ProjectDir
	if s, ok := b.store.Session(input.ThreadID); ok {
		if s.OwnerID != "" && s.OwnerID != input.UserID {
			return
		}
		if s.Cwd != "" {
			cwd = s.Cwd
		}
	}
	if !b.workspaceAllowed(cwd) {
		b.respond(input, "目标会话项目尚未授权，附件未下载。", input.ThreadID)
		return
	}
	a := *input.Attachment
	if a.Size < 0 || a.Size > MaxFileBytes || (a.Kind != "file" && a.Kind != "image") || a.Key == "" {
		b.respond(input, "附件格式不支持或超过20 MiB，未下载。", input.ThreadID)
		return
	}
	transport, ok := b.sender.(fileTransport)
	if !ok {
		b.respond(input, "当前接收端不支持附件下载。", input.ThreadID)
		return
	}
	b.mu.Lock()
	if b.transfers == nil {
		b.transfers = map[string]bool{}
	}
	if len(b.transfers) >= 4 {
		b.mu.Unlock()
		b.respond(input, "同时传输的文件已达上限，请稍后重发。", input.ThreadID)
		return
	}
	b.transfers[input.MessageID] = true
	b.mu.Unlock()
	b.jobs.Add(1)
	b.beginOperation() // Retain the guard until file metadata and receipt finish.
	go func() {
		defer b.jobs.Done()
		defer b.endOperation()
		defer func() {
			b.mu.Lock()
			delete(b.transfers, input.MessageID)
			b.mu.Unlock()
			status := "handled"
			if b.failed.Load() {
				status = "pending"
			}
			b.markInput(input.MessageID, status)
		}()
		callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		data, err := transport.DownloadFile(callCtx, input.MessageID, a)
		if err != nil || len(data) == 0 || len(data) > MaxFileBytes {
			b.respond(input, "附件下载失败：请检查机器人资源权限、文件大小后重新发送。", input.ThreadID)
			return
		}
		path, err := saveAttachment(cwd, a.Name, data)
		if err != nil {
			b.respond(input, "附件未能保存到目标项目，请检查项目写入权限后重发。", input.ThreadID)
			return
		}
		if err := b.rememberFile(receivedFile{Chat: input.ChatID, Owner: input.UserID, Thread: input.ThreadID, Path: path, Name: safeFileName(a.Name), Kind: a.Kind, Created: time.Now().Unix()}); err != nil {
			b.fail(errors.New("cannot retain received attachment metadata"))
			return
		}
		text := fmt.Sprintf("已接收文件：%s\n大小：%d 字节\n保存位置：%s\n点击下方命令生成草稿，或继续说明希望怎样处理。", safeFileName(a.Name), len(data), path)
		choices := []replyChoice{{"read", "查看文件", "请查看这个附件，说明内容并等待我的后续要求：" + path}, {"summarize", "总结文件", "请总结这个附件：" + path}, {"file", "发回文件", "/file " + path}}
		b.emitChoices(input, text, choices, input.ThreadID)
	}()
}

func saveAttachment(cwd, name string, data []byte) (string, error) {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, part := range []string{".cache", ".cache/feishu-files"} {
		if err := root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err := root.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("attachment directory unavailable")
		}
	}
	dir, err := root.OpenRoot(".cache/feishu-files")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	token, err := randomToken(16)
	if err != nil {
		return "", err
	}
	if err := dir.Mkdir(token, 0700); err != nil {
		return "", err
	}
	relative := filepath.Join(token, safeFileName(name))
	f, err := dir.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	// Persist new directory entries before acknowledging the received file.
	for _, part := range []string{filepath.Join(".cache/feishu-files", token), ".cache/feishu-files", ".cache", "."} {
		directory, openErr := root.Open(part)
		if openErr != nil {
			return "", openErr
		}
		syncErr := directory.Sync()
		directory.Close()
		if syncErr != nil {
			return "", syncErr
		}
	}
	return filepath.Join(cwd, ".cache/feishu-files", relative), nil
}
