package feishu

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestConversationNativeEntryUsesServerTopicAndKeepsMainContentUnchanged(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New("fixture-native-topic-sdk", "fixture-secret", log)
	var main, topic, entry int
	c.api = lark.NewClient("fixture-native-topic-sdk", "fixture-secret", lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "tenant_access_token") {
			return slashResponse(req, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
		}
		if req.URL.Path == "/open-apis/im/v1/files" {
			return slashResponse(req, 200, `{"code":0,"data":{"file_key":"fixture-file"}}`), nil
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		content, _ := body["content"].(string)
		switch {
		case strings.HasSuffix(req.URL.Path, "/reply"):
			topic++
			if body["reply_in_thread"] != true || req.URL.Path != "/open-apis/im/v1/messages/om_root/reply" {
				t.Fatal("topic root or native flag lost")
			}
			return slashResponse(req, 200, `{"code":0,"data":{"message_id":"om_topic_copy","chat_id":"oc_chat","thread_id":"omt_real_topic"}}`), nil
		case strings.HasPrefix(req.URL.Path, "/open-apis/im/v1/threads/"):
			entry++
			if req.URL.Path != "/open-apis/im/v1/threads/omt_real_topic/forward" || body["receive_id"] != "oc_chat" || req.URL.Query().Get("receive_id_type") != "chat_id" || req.URL.Query().Get("uuid") == "" {
				t.Fatal("native topic card uses wrong scope, guessed ID or no deduplication")
			}
			return slashResponse(req, 200, `{"code":0,"data":{"message_id":"om_native_card","msg_type":"merge_forward"}}`), nil
		default:
			main++
			if req.URL.Path != "/open-apis/im/v1/messages" || body["receive_id"] != "oc_chat" || strings.Contains(content, "applink.feishu.cn") || strings.Contains(content, "查看本会话话题记录") {
				t.Fatal("normal reply contains the broken URL footer")
			}
			if body["msg_type"] == "text" {
				var text map[string]string
				json.Unmarshal([]byte(content), &text)
				if text["text"] != "complete visible current reply" {
					t.Fatal("main text changed")
				}
			}
			if body["msg_type"] == "interactive" && content != `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"complete current card"}]}}` {
				t.Fatal("main card changed")
			}
			return slashResponse(req, 200, `{"code":0,"data":{"message_id":"om_main"}}`), nil
		}
	}}))
	ctx := context.Background()
	if _, err := c.SendRouted(ctx, "oc_chat", "complete visible current reply", "main-text"); err != nil {
		t.Fatal(err)
	}
	card := json.RawMessage(`{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"complete current card"}]}}`)
	if _, err := c.SendCardRouted(ctx, "oc_chat", card, "main-card"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendConversationText(ctx, "oc_chat", "om_root", "complete topic reply", "copy-text"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendConversationFile(ctx, "oc_chat", "om_root", "report.txt", []byte("complete result"), "copy-file"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendConversationEntry(ctx, "oc_chat", "om_root", "native-entry"); err != nil {
		t.Fatal(err)
	}
	if main != 2 || topic != 2 || entry != 1 {
		t.Fatal("normal reply, topic copy and native entry not all delivered")
	}
}

func TestConversationEntryRestartLooksUpExactRootAndRejectsWrongMetadata(t *testing.T) {
	for _, sample := range []struct {
		name, root, chat, thread string
		valid                    bool
	}{
		{"valid", "om_root", "oc_chat", "omt_real_topic", true},
		{"message-is-not-topic", "om_root", "oc_chat", "om_root", false},
		{"wrong-chat", "om_root", "oc_other", "omt_other", false},
		{"wrong-root", "om_other", "oc_chat", "omt_real_topic", false},
		{"no-topic", "om_root", "oc_chat", "", false},
		{"path-injection", "om_root", "oc_chat", "omt_bad/../../other", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			id := "fixture-topic-recovery-" + sample.name
			c := New(id, "fixture-secret", log)
			lookups, forwards := 0, 0
			var uuid string
			c.api = lark.NewClient(id, "fixture-secret", lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "tenant_access_token") {
					return slashResponse(req, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
				}
				if req.Method == http.MethodGet {
					lookups++
					if req.URL.Path != "/open-apis/im/v1/messages/om_root" {
						t.Fatal("lookup escaped known root")
					}
					body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"items": []any{map[string]string{"message_id": sample.root, "chat_id": sample.chat, "thread_id": sample.thread}}}})
					return slashResponse(req, 200, string(body)), nil
				}
				forwards++
				if req.URL.Path != "/open-apis/im/v1/threads/omt_real_topic/forward" {
					t.Fatal("forwarded wrong topic")
				}
				key := req.URL.Query().Get("uuid")
				if uuid != "" && uuid != key {
					t.Fatal("retry changed native-card UUID")
				}
				uuid = key
				return slashResponse(req, 200, `{"code":0,"data":{"message_id":"om_native_card"}}`), nil
			}}))
			_, err := c.SendConversationEntry(context.Background(), "oc_chat", "om_root", "same-entry")
			if (err == nil) != sample.valid {
				t.Fatalf("unexpected metadata validation result: %v", err)
			}
			if sample.valid {
				if _, err := c.SendConversationEntry(context.Background(), "oc_chat", "om_root", "same-entry"); err != nil {
					t.Fatal(err)
				}
				if forwards != 2 || lookups != 1 || uuid == "" {
					t.Fatal("native topic cache or stable UUID missing")
				}
			} else if forwards != 0 {
				t.Fatal("invalid metadata caused an external forward")
			}
		})
	}
}
