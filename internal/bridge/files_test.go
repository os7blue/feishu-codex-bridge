package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

type fileFixture struct {
	fakeSender
	downloads atomic.Int64
	gate      chan struct{}
	sent      []byte
	key       string
	fail      bool
}

func (f *fileFixture) DownloadFile(ctx context.Context, _ string, _ Attachment) ([]byte, error) {
	f.downloads.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []byte("fixture attachment"), nil
}
func (f *fileFixture) SendFile(_ context.Context, _, _ string, data []byte, key string) ([]string, error) {
	f.sent = append([]byte(nil), data...)
	if f.key != "" && key != f.key {
		return nil, errors.New("delivery key changed")
	}
	f.key = key
	if f.fail {
		return nil, errors.New("fixture send failure")
	}
	return []string{"file-message"}, nil
}

func TestExportRejectsPrivateOutsideAndOversizedFiles(t *testing.T) {
	b, _ := setup(t)
	b.cfg.AppSecret = "fixture-sensitive-token"
	b.cfg.ConfigPath = filepath.Join(b.cfg.ProjectDir, "private-config.json")
	b.cfg.CodexHome = filepath.Join(b.cfg.ProjectDir, "fixture-home")
	paths := []string{b.cfg.ConfigPath, filepath.Join(b.cfg.StateDir, "state.json"), filepath.Join(b.cfg.CodexHome, "session.json"), filepath.Join(b.cfg.ProjectDir, ".env"), filepath.Join(b.cfg.ProjectDir, ".ssh", "id_rsa"), filepath.Join(t.TempDir(), "outside.txt"), filepath.Join(b.cfg.ProjectDir, "contains-secret.txt")}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b.cfg.AppSecret), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(b.cfg.ProjectDir, "innocent.txt")
	if err := os.Symlink(b.cfg.ConfigPath, link); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, link)
	oversized := filepath.Join(b.cfg.ProjectDir, "large.bin")
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	paths = append(paths, oversized)
	for _, p := range paths {
		_, data, err := ReadExport(b.cfg, b.cfg.ProjectDir, p)
		if err == nil || len(data) != 0 || strings.Contains(err.Error(), b.cfg.AppSecret) {
			t.Fatalf("private/invalid export accepted: %s", filepath.Base(p))
		}
	}
	good := filepath.Join(b.cfg.ProjectDir, "report with spaces.txt")
	os.WriteFile(good, []byte("public report"), 0600)
	name, data, err := ReadExport(b.cfg, b.cfg.ProjectDir, "report with spaces.txt")
	if err != nil || name != "report with spaces.txt" || string(data) != "public report" {
		t.Fatal("public file rejected", err)
	}
}

func TestReceiveFileKeepsCapturedSessionAndDeduplicates(t *testing.T) {
	b, rpc := setup(t)
	transport := &fileFixture{gate: make(chan struct{})}
	b.sender = transport
	a, err := b.store.RememberSession(state.Session{ThreadID: "thread-a", Cwd: b.cfg.ProjectDir, OwnerID: "ou_owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := b.store.RememberSession(state.Session{ThreadID: "thread-b", Cwd: b.cfg.ProjectDir, OwnerID: "ou_owner"})
	b.store.Bind("chat-1", state.Binding{ThreadID: a.ThreadID, OwnerID: "ou_owner"})
	in := input("attachment-message", "")
	in.Attachment = &Attachment{Kind: "file", Key: "fixture-key", Name: "../../report.txt"}
	stranger := in
	stranger.UserID = "ou_stranger"
	b.Submit(stranger)
	if len(b.in) != 0 {
		t.Fatal("unauthorized file queued")
	}
	if err := b.Submit(in); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), <-b.in)
	b.store.Bind("chat-1", state.Binding{ThreadID: other.ThreadID, OwnerID: "ou_owner"})
	close(transport.gate)
	b.jobs.Wait()
	if transport.downloads.Load() != 1 || len(rpc.calls) != 0 {
		t.Fatal("file launched inference or downloaded twice")
	}
	item := <-b.out
	if item.ThreadID != a.ThreadID || !strings.Contains(item.Text, "report.txt") || strings.Contains(item.Text, "../") {
		t.Fatal("file routing/name lost")
	}
	matches, _ := filepath.Glob(filepath.Join(b.cfg.ProjectDir, ".cache/feishu-files/*/report.txt"))
	if len(matches) != 1 {
		t.Fatal("attachment not stored once")
	}
	st, _ := os.Stat(matches[0])
	if st.Mode().Perm() != 0600 {
		t.Fatal("attachment is not private")
	}
	if err := b.Submit(in); err != nil {
		t.Fatal(err)
	}
	if len(b.in) != 0 || transport.downloads.Load() != 1 {
		t.Fatal("duplicate attachment replayed")
	}
	var menu string
	for token, g := range b.drafts {
		if g.menu && len(g.replies) > 0 {
			menu = token
		}
	}
	draft, _ := openDraft(t, b, menu, "read")
	if b.drafts[draft].thread != a.ThreadID || len(b.in) != 0 {
		t.Fatal("opening attachment draft submitted or retargeted it")
	}
	changed := in
	changed.Attachment = &Attachment{Kind: "file", Key: "different", Name: "report.txt"}
	if b.Submit(changed) == nil {
		t.Fatal("changed duplicate identity accepted")
	}
}

func TestOutgoingFileSnapshotRetriesAndRoutesOnce(t *testing.T) {
	b, _ := setup(t)
	transport := &fileFixture{fail: true}
	b.sender = transport
	s, _ := b.store.RememberSession(state.Session{ThreadID: "thread-a", Cwd: b.cfg.ProjectDir, OwnerID: "ou_owner"})
	path := filepath.Join(b.cfg.ProjectDir, "report.txt")
	os.WriteFile(path, []byte("original report"), 0600)
	b.handle(context.Background(), input("send-file", fmt.Sprintf("/file #%d report.txt", s.Ref)))
	item := <-b.out
	if item.FilePath == "" {
		t.Fatal("file was not durably queued")
	}
	os.WriteFile(path, []byte("changed later"), 0600)
	if b.deliverOne(context.Background(), item) || string(transport.sent) != "original report" {
		t.Fatal("snapshot not retained on failure")
	}
	if _, err := os.Stat(item.FilePath); err != nil {
		t.Fatal("retry snapshot removed")
	}
	transport.fail = false
	if !b.deliverOne(context.Background(), item) {
		t.Fatal("file retry failed")
	}
	if string(transport.sent) != "original report" {
		t.Fatal("retry reread changed source")
	}
	if _, err := os.Stat(item.FilePath); !os.IsNotExist(err) {
		t.Fatal("delivered snapshot retained")
	}
	route, ok := b.store.Route("file-message", "chat-1", "ou_owner")
	if !ok || route.ThreadID != s.ThreadID {
		t.Fatal("file reply routing lost")
	}
	if !b.deliverOne(context.Background(), item) {
		t.Fatal("delivered entry replayed")
	}
}

func TestAttachmentStorageRejectsSymlinkCache(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".cache")); err != nil {
		t.Fatal(err)
	}
	if _, err := saveAttachment(root, "report.txt", []byte("fixture")); err == nil {
		t.Fatal("symlink cache accepted")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("attachment escaped project")
	}
}

func TestAttachmentContextSurvivesRestartAndStaysInItsSession(t *testing.T) {
	b, _ := setup(t)
	name := strings.Repeat("测试", 100) + ".txt"
	path, err := saveAttachment(b.cfg.ProjectDir, name, []byte("fixture content must not enter prompt automatically"))
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Base(path)) > 200 || !utf8.ValidString(filepath.Base(path)) {
		t.Fatal("long filename was not safely bounded")
	}
	if err := b.rememberFile(receivedFile{Chat: "chat-1", Owner: "ou_owner", Thread: "thread-a", Path: path, Name: filepath.Base(path), Kind: "file", Created: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	b.receivedFiles = nil
	if err := b.recoverFiles(); err != nil {
		t.Fatal(err)
	}
	in := input("inspect", "看一下")
	in.ThreadID = "thread-a"
	text := b.fileContext(in, in.Text)
	if !strings.Contains(text, path) || strings.Contains(text, "fixture content") {
		t.Fatal("attachment context missing or file content eagerly read")
	}
	for _, which := range []string{"chat", "owner", "thread"} {
		other := in
		switch which {
		case "chat":
			other.ChatID = "other"
		case "owner":
			other.UserID = "other"
		case "thread":
			other.ThreadID = "other"
		}
		if b.fileContext(other, other.Text) != other.Text {
			t.Fatal("attachment context leaked across " + which)
		}
	}
}
