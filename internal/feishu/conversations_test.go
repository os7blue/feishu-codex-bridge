package feishu

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestConversationMainDeliveryAndNativeTopicUseDistinctEndpoints(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New("fixture-conversation-sdk", "fixture-secret", log)
	var main, topic int
	c.api = lark.NewClient("fixture-conversation-sdk", "fixture-secret", lark.WithLogger(sdkLog{log}), lark.WithHttpClient(fixtureHTTP{handle: func(req *http.Request) (*http.Response, error) {
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
		if strings.HasSuffix(req.URL.Path, "/reply") {
			topic++
			if body["reply_in_thread"] != true || !strings.Contains(req.URL.Path, "/om_root/reply") {
				t.Fatal("topic target or flag lost")
			}
		} else {
			main++
			if req.URL.Path != "/open-apis/im/v1/messages" || body["receive_id"] != "oc_chat" || !strings.Contains(content, "client/thread/open") {
				t.Fatal("main reply missing full content or topic entry")
			}
		}
		return slashResponse(req, 200, `{"code":0,"data":{"message_id":"fixture-message"}}`), nil
	}}))
	text := "complete visible current reply"
	if _, err := c.SendTextWithConversation(context.Background(), "oc_chat", "om_root", text, "main-text"); err != nil {
		t.Fatal(err)
	}
	card := json.RawMessage(`{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"complete current card"}]}}`)
	before := string(card)
	if _, err := c.SendCardWithConversation(context.Background(), "oc_chat", "om_root", card, "main-card"); err != nil {
		t.Fatal(err)
	}
	if string(card) != before {
		t.Fatal("source card mutated")
	}
	if _, err := c.SendConversationText(context.Background(), "oc_chat", "om_root", text, "copy-text"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendConversationFile(context.Background(), "oc_chat", "om_root", "report.txt", []byte("complete result"), "copy-file"); err != nil {
		t.Fatal(err)
	}
	if main != 2 || topic != 2 {
		t.Fatal("full main replies and topic copies not both sent")
	}
	link, _ := url.Parse(c.ConversationLink("oc_chat&unsafe=1", "om_root?unsafe=1"))
	if link.Query().Get("openchatid") != "oc_chat&unsafe=1" || link.Query().Get("openthreadid") != "om_root?unsafe=1" || link.Query().Has("unsafe") {
		t.Fatal("native link query was injectable")
	}
}
