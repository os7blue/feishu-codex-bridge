package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var nativeConversationThreadID = regexp.MustCompile(`^omt_[A-Za-z0-9_-]{1,128}$`)

func (c *Client) SendConversationText(ctx context.Context, chat, root, text, key string) ([]string, error) {
	if c.secret != "" {
		text = strings.ReplaceAll(text, c.secret, "[REDACTED]")
	}
	var ids []string
	parts := Split(text, 2000)
	for i, part := range parts {
		if len(parts) > 1 {
			part = fmt.Sprintf("[%d/%d]\n%s", i+1, len(parts), part)
		}
		body, _ := json.Marshal(map[string]string{"text": part})
		id, err := c.sendContent(ctx, chat, "text", string(body), fmt.Sprintf("%s:%d", key, i), root)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (c *Client) rememberConversationThread(chat, root string, returnedChat, thread *string) {
	// An om_ message root is never an omt_ topic ID. Cache only server-issued
	// topics whose response explicitly confirms the requested chat.
	if returnedChat == nil || *returnedChat != chat || thread == nil || !nativeConversationThreadID.MatchString(*thread) {
		return
	}
	c.conversationMu.Lock()
	defer c.conversationMu.Unlock()
	if c.conversationThreads == nil || len(c.conversationThreads) >= 20000 {
		c.conversationThreads = make(map[string]string)
	}
	c.conversationThreads[chat+"\x00"+root] = *thread
}

func (c *Client) conversationThread(ctx context.Context, chat, root string) (string, error) {
	c.conversationMu.Lock()
	thread := c.conversationThreads[chat+"\x00"+root]
	c.conversationMu.Unlock()
	if thread != "" {
		return thread, nil
	}
	// Receipt recovery after restart queries only this authenticated root through
	// the normal API. Never infer a topic ID or retain message contents.
	resp, err := c.api.Im.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(root).Build())
	status, code := 0, 0
	if resp != nil {
		code = resp.Code
		if resp.ApiResp != nil {
			status = resp.StatusCode
		}
	}
	c.diagnostics.API(code, status)
	if err != nil || resp == nil || !resp.Success() {
		return "", &DeliveryError{Kind: "conversation_lookup", Code: code, HTTPStatus: status}
	}
	if resp.Data == nil || len(resp.Data.Items) != 1 {
		return "", errors.New("invalid conversation metadata")
	}
	item := resp.Data.Items[0]
	if item == nil || item.MessageId == nil || *item.MessageId != root || item.ChatId == nil || *item.ChatId != chat || item.ThreadId == nil || !nativeConversationThreadID.MatchString(*item.ThreadId) {
		return "", errors.New("conversation metadata scope mismatch")
	}
	c.rememberConversationThread(chat, root, item.ChatId, item.ThreadId)
	return *item.ThreadId, nil
}

// Forwarding the actual topic lets Feishu render its native expandable record
// card. Plain text and interactive cards keep their original complete content;
// no guessed AppLink or visible internal IDs are appended to them.
func (c *Client) SendConversationEntry(ctx context.Context, chat, root, key string) ([]string, error) {
	thread, err := c.conversationThread(ctx, chat, root)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(key))
	req := larkim.NewForwardThreadReqBuilder().ThreadId(thread).ReceiveIdType("chat_id").Uuid(hex.EncodeToString(hash[:16])).Body(larkim.NewForwardThreadReqBodyBuilder().ReceiveId(chat).Build()).Build()
	id, err := c.sendMessage(ctx, "merge_forward", func() (*larkim.CreateMessageResp, error) {
		forwarded, err := c.api.Im.Thread.Forward(ctx, req)
		if forwarded == nil {
			return nil, err
		}
		resp := &larkim.CreateMessageResp{ApiResp: forwarded.ApiResp, CodeError: forwarded.CodeError}
		if forwarded.Data != nil {
			resp.Data = &larkim.CreateMessageRespData{MessageId: forwarded.Data.MessageId}
		}
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}
