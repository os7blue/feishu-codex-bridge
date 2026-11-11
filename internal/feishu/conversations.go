package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The native thread AppLink is assembled only from the authenticated chat and
// server-issued root. Client rendering still needs desktop/mobile acceptance.
func (c *Client) ConversationLink(chat, root string) string {
	values := url.Values{"openthreadid": {root}, "openchatid": {chat}, "open_thread_id": {root}, "open_chat_id": {chat}}
	return "https://applink.feishu.cn/client/thread/open?" + values.Encode()
}

func (c *Client) SendTextWithConversation(ctx context.Context, chat, root, text, key string) ([]string, error) {
	return c.sendConversationText(ctx, chat, "", text, key, "\n\n查看本会话话题记录："+c.ConversationLink(chat, root))
}

func (c *Client) SendConversationText(ctx context.Context, chat, root, text, key string) ([]string, error) {
	return c.sendConversationText(ctx, chat, root, text, key, "")
}

func (c *Client) sendConversationText(ctx context.Context, chat, root, text, key, footer string) ([]string, error) {
	if c.secret != "" {
		text = strings.ReplaceAll(text, c.secret, "[REDACTED]")
	}
	var ids []string
	parts := Split(text, 2000)
	for i, part := range parts {
		if len(parts) > 1 {
			part = fmt.Sprintf("[%d/%d]\n%s", i+1, len(parts), part)
		}
		body, _ := json.Marshal(map[string]string{"text": part + footer})
		id, err := c.sendContent(ctx, chat, "text", string(body), fmt.Sprintf("%s:%d", key, i), root)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (c *Client) SendCardWithConversation(ctx context.Context, chat, root string, card json.RawMessage, key string) ([]string, error) {
	var value map[string]any
	if json.Unmarshal(card, &value) != nil || value == nil {
		return nil, errors.New("invalid outgoing card")
	}
	container := value
	if body, ok := value["body"].(map[string]any); ok {
		container = body
	}
	elements, _ := container["elements"].([]any)
	container["elements"] = append(elements, map[string]any{"tag": "markdown", "content": "[查看本会话话题记录](" + c.ConversationLink(chat, root) + ")"})
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("invalid outgoing card")
	}
	return c.SendCardRouted(ctx, chat, encoded, key)
}
