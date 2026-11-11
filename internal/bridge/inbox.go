package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

// Private receipts survive a crash between callback acknowledgement and handling.
// Recovery reports uncertainty, never executes or replays saved input.
type inputReceipt struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	Created int64  `json:"created"`
	Input   Input  `json:"input"`
}

// Recover must complete before starting the message transport. Run also calls it
// once for other callers, but the CLI orders it before accepting fresh messages.
func (b *Bridge) Recover() error {
	b.recoveryOnce.Do(func() {
		b.recoveryErr = b.recoverInputs()
		if b.recoveryErr == nil {
			b.recoveryErr = b.recoverFiles()
		}
		if b.recoveryErr == nil {
			b.recoveryErr = b.recoverQueue()
		}
	})
	return b.recoveryErr
}

func recoveryNotice(count int) string {
	return fmt.Sprintf("桥接已重启：有 %d 条已接收消息尚未确认处理完成，接收记录已保留，未自动重新执行。\n可先查看 /status 及对应会话的 /history，再确认是否重新发送。旧审批随重启失效，请等待新的审批请求。", count)
}

func (b *Bridge) inputPath(id string) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(b.cfg.StateDir, "inbox-"+hex.EncodeToString(hash[:])+".json")
}

func (b *Bridge) readInput(path string) (inputReceipt, error) {
	var receipt inputReceipt
	info, err := os.Lstat(path)
	if err != nil {
		return receipt, err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return receipt, errors.New("invalid input receipt file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return receipt, err
	}
	if json.Unmarshal(data, &receipt) != nil || receipt.Version != 1 || receipt.Input.MessageID == "" || receipt.Input.ChatID == "" || receipt.Input.UserID == "" || b.inputPath(receipt.Input.MessageID) != path || receipt.Created <= 0 {
		return receipt, errors.New("invalid input receipt")
	}
	switch receipt.Status {
	case "received", "handling", "pending", "handled", "interrupted":
		return receipt, nil
	default:
		return receipt, errors.New("unknown input receipt status")
	}
}

func (b *Bridge) retainInput(input Input) (bool, error) {
	b.inboxMu.Lock()
	defer b.inboxMu.Unlock()
	path := b.inputPath(input.MessageID)
	if receipt, err := b.readInput(path); err == nil {
		oldAttachment, _ := json.Marshal(receipt.Input.Attachment)
		newAttachment, _ := json.Marshal(input.Attachment)
		if receipt.Input.ChatID != input.ChatID || receipt.Input.UserID != input.UserID || receipt.Input.Text != input.Text || receipt.Input.ThreadID != input.ThreadID || receipt.Input.ReplyTo != input.ReplyTo || receipt.Input.ReplyRoot != input.ReplyRoot || receipt.Input.SourceCardID != input.SourceCardID || string(oldAttachment) != string(newAttachment) {
			return false, errors.New("input receipt identity mismatch")
		}
		return receipt.Status == "received", nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, "inbox-*.json"))
	if err != nil {
		return false, errors.New("input receipt capacity reached")
	}
	remaining := len(paths)
	if remaining >= 20000 {
		for _, existing := range paths {
			receipt, err := b.readInput(existing)
			if err != nil {
				return false, errors.New("cannot prune input receipts")
			}
			if (receipt.Status == "handled" || receipt.Status == "interrupted") && time.Since(time.Unix(receipt.Created, 0)) > 7*24*time.Hour {
				if err := os.Remove(existing); err != nil {
					return false, errors.New("cannot prune input receipts")
				}
				remaining--
			}
		}
	}
	if remaining >= 20000 {
		return false, errors.New("input receipt capacity reached")
	}
	err = state.WritePrivate(path, inputReceipt{Version: 1, Status: "received", Created: time.Now().Unix(), Input: input})
	return err == nil, err
}

func (b *Bridge) markInput(id, status string) bool {
	b.inboxMu.Lock()
	defer b.inboxMu.Unlock()
	path := b.inputPath(id)
	receipt, err := b.readInput(path)
	if os.IsNotExist(err) {
		return true // Direct internal inputs and pre-upgrade queued tasks have no receipt.
	}
	if err == nil && (receipt.Status == "handled" || receipt.Status == "interrupted") {
		return true
	}
	if err == nil {
		receipt.Status = status
		err = state.WritePrivate(path, receipt)
	}
	if err != nil {
		b.fail(errors.New("cannot update input receipt"))
		return false
	}
	return true
}

func (b *Bridge) finishInput(input Input) {
	b.mu.Lock()
	status := "handled"
	if b.active != nil {
		for _, id := range b.active.inputs {
			if id == input.MessageID {
				status = "pending"
			}
		}
	}
	for _, queued := range b.queue {
		if queued.Input.MessageID == input.MessageID {
			status = "pending"
		}
	}
	if b.failed.Load() {
		status = "pending"
	}
	if b.transfers[input.MessageID] {
		status = "pending"
	}
	b.mu.Unlock()
	b.markInput(input.MessageID, status)
}

func (b *Bridge) recoverInputs() error {
	paths, err := filepath.Glob(filepath.Join(b.cfg.StateDir, "inbox-*.json"))
	if err != nil || len(paths) > 20000 {
		return errors.New("cannot list input receipts")
	}
	type destination struct{ chat, owner string }
	counts := map[destination]int{}
	var interrupted []inputReceipt
	for _, path := range paths {
		receipt, err := b.readInput(path)
		if err != nil {
			return errors.New("cannot recover input receipt")
		}
		if receipt.Status == "handled" || receipt.Status == "interrupted" {
			if time.Since(time.Unix(receipt.Created, 0)) > 7*24*time.Hour {
				if err := os.Remove(path); err != nil {
					return errors.New("cannot prune input receipt")
				}
			}
			continue
		}
		if b.authorized(receipt.Input.UserID) {
			counts[destination{receipt.Input.ChatID, receipt.Input.UserID}]++
		}
		interrupted = append(interrupted, receipt)
	}
	for destination, count := range counts {
		if !b.recoveryReply(destination.chat, destination.owner, recoveryNotice(count), interrupted...) {
			return errors.New("cannot retain input recovery notice")
		}
	}
	for _, receipt := range interrupted {
		if !b.markInput(receipt.Input.MessageID, "interrupted") {
			return errors.New("cannot retain interrupted input")
		}
	}
	return nil
}
