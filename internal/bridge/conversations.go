package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

var transcriptCredential = regexp.MustCompile(`(?:github_pat_[A-Za-z0-9_]{25,}|gh[pousr]_[A-Za-z0-9]{25,}|sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{24,}|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----)`)

func conversationKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// Called only by the serial durable delivery worker. Persist the native root
// before any child is delivered, so restart and retries keep the same topic.
func (b *Bridge) conversationRoot(ctx context.Context, item outbound) (string, error) {
	if _, ok := b.sender.(ConversationMessenger); !ok || item.ThreadID == "" || item.OwnerID == "" {
		return "", nil
	}
	if root, ok := b.store.ConversationRoot(item.Chat, item.OwnerID, item.ThreadID); ok {
		return root, nil
	}
	session, ok := b.store.Session(item.ThreadID)
	if !ok || session.OwnerID != item.OwnerID {
		return "", errors.New("conversation owner mismatch")
	}
	sender, ok := b.sender.(RoutedMessenger)
	if !ok {
		return "", errors.New("conversation root delivery unavailable")
	}
	text := fmt.Sprintf("会话 #%d · %s\n项目：%s\n\n本话题集中显示该会话的对话、进度、审批和成果。点击话题或查看回复进入，在话题内发消息即可接续。\n已有散落记录：/history #%d", session.Ref, b.sessionLabel(session), filepath.Base(session.Cwd), session.Ref)
	ids, err := sender.SendRouted(ctx, item.Chat, text, "conversation-root:"+conversationKey(item.Chat, item.OwnerID, item.ThreadID))
	if err != nil {
		return "", err
	}
	if len(ids) != 1 || ids[0] == "" {
		return "", errors.New("conversation root receipt invalid")
	}
	err = b.store.RememberConversationRoot(ids[0], state.MessageRoute{ChatID: item.Chat, OwnerID: item.OwnerID, ThreadID: item.ThreadID, Created: time.Now().Unix()})
	return ids[0], err
}

// A user message already inside its topic is displayed natively. Main-chat
// inputs are copied as a clearly attributed transcript, never submitted again.
func (b *Bridge) rememberConversationInput(input Input, created int64) bool {
	if _, ok := b.sender.(ConversationMessenger); !ok || input.demoReply {
		return true
	}
	root, exists := b.store.ConversationRoot(input.ChatID, input.UserID, input.ThreadID)
	if exists && (input.ReplyRoot == root || input.ReplyTo == root) {
		return true
	}
	text := input.Text
	if input.Attachment != nil {
		text += "\n附件：" + safeFileName(input.Attachment.Name)
	}
	if strings.TrimSpace(text) == "" {
		return true
	}
	if b.cfg.AppSecret != "" {
		text = strings.ReplaceAll(text, b.cfg.AppSecret, "[REDACTED]")
	}
	text = transcriptCredential.ReplaceAllString(text, "[凭据已隐藏]")
	key := "input-" + conversationKey(input.ChatID, input.UserID, input.ThreadID, input.MessageID)
	b.conversationMu.Lock()
	defer b.conversationMu.Unlock()
	if b.conversationInputs == nil {
		b.conversationInputs = make(map[string]time.Time)
	}
	if _, exists := b.conversationInputs[key]; exists {
		return true
	}
	for id, at := range b.conversationInputs {
		if time.Since(at) > time.Hour {
			delete(b.conversationInputs, id)
		}
	}

	item := outbound{Chat: input.ChatID, OwnerID: input.UserID, ThreadID: input.ThreadID, Text: "【用户消息 · 从主聊天同步】\n" + text, Key: key, Created: created * int64(time.Second), Transcript: true}
	if err := state.WritePrivate(b.outboxPath(item), item); err != nil {
		b.fail(errors.New("cannot persist conversation transcript"))
		return false
	}
	b.conversationInputs[key] = time.Now()
	select {
	case b.out <- item:
	default:
	}
	return true
}

func (b *Bridge) sendConversationFile(ctx context.Context, item outbound) ([]string, error) {
	sender, ok := b.sender.(ConversationFileTransport)
	if !ok {
		return b.sender.(ConversationMessenger).SendConversationText(ctx, item.Chat, item.ConversationRoot, "成果附件："+item.FileName, item.Key+":conversation-file")
	}
	data, err := b.fileSnapshot(item)
	if err != nil {
		return nil, err
	}
	return sender.SendConversationFile(ctx, item.Chat, item.ConversationRoot, item.FileName, data, item.Key+":conversation")
}
