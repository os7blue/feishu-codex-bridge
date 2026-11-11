package feishu

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func receive(t *testing.T, sender, chat, kind string, timestamp time.Time) *larkim.P2MessageReceiveV1 {
	t.Helper()
	content, _ := json.Marshal(map[string]string{"text": "你好，继续开发"})
	data, _ := json.Marshal(map[string]any{"event": map[string]any{
		"sender":  map[string]any{"sender_type": sender, "sender_id": map[string]string{"open_id": "ou_owner"}},
		"message": map[string]any{"chat_type": chat, "message_type": kind, "message_id": "om_test", "chat_id": "oc_test", "create_time": fmt.Sprint(timestamp.UnixMilli()), "content": string(content)},
	}})
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	return &event
}

func TestParseOnlyRecentHumanPrivateText(t *testing.T) {
	now := time.Now()
	if input, ok := Parse(receive(t, "user", "p2p", "text", now), now); !ok || input.Text != "你好，继续开发" || input.UserID != "ou_owner" {
		t.Fatal("valid private message rejected")
	}
	for _, event := range []*larkim.P2MessageReceiveV1{nil, receive(t, "bot", "p2p", "text", now), receive(t, "user", "group", "text", now), receive(t, "user", "p2p", "image", now), receive(t, "user", "p2p", "text", now.Add(-48*time.Hour)), receive(t, "user", "p2p", "text", now.Add(time.Hour))} {
		if _, ok := Parse(event, now); ok {
			t.Fatal("unsupported or replayed event accepted")
		}
	}
}

func TestChunkingPreservesUnicodeAndByteLimits(t *testing.T) {
	text := strings.Repeat("你好🙂\\\n", 3000)
	parts := Split(text, 2000)
	if strings.Join(parts, "") != text {
		t.Fatal("chunking corrupted text")
	}
	for _, part := range parts {
		if len(part) > 2000 || !utf8.ValidString(part) {
			t.Fatal("invalid chunk boundary")
		}
		body, err := json.Marshal(map[string]string{"text": part})
		if err != nil || len(body) > 15000 {
			t.Fatal("chunk exceeds conservative payload limit")
		}
	}
}

func TestReplyParentIDIsPreservedForSessionRouting(t *testing.T) {
	now := time.Now()
	event := receive(t, "user", "p2p", "text", now)
	parent, root := "parent-message", "root-message"
	event.Event.Message.ParentId = &parent
	event.Event.Message.RootId = &root
	input, ok := Parse(event, now)
	if !ok || input.ReplyTo != parent || input.ReplyRoot != root {
		t.Fatal("reply did not retain its direct parent")
	}
	event.Event.Message.ParentId = nil
	input, ok = Parse(event, now)
	if !ok || input.ReplyTo != root {
		t.Fatal("thread root fallback was lost")
	}
}
