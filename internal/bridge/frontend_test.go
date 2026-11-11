package bridge

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os7blue/feishu-codex-bridge/internal/presentation"
)

func TestTextFrontendReusesSessionsTasksAndManualApprovals(t *testing.T) {
	b, rpc := setup(t)
	b.renderer = presentation.Text{}
	ctx := context.Background()
	b.handle(ctx, input("text-new", "/new"))
	b.handle(ctx, input("text-task", "work"))
	b.jobs.Wait()
	if b.active == nil || b.active.turn != "turn-1" {
		t.Fatal("text frontend task failed")
	}
	approval := notification("item/commandExecution/requestApproval", `{"threadId":"thread-1","turnId":"turn-1","command":"fixture-command","reason":"fixture-review"}`)
	approval.ID = json.RawMessage(`77`)
	b.event(approval)
	var request string
	for token := range b.pending {
		request = token
	}
	if request == "" {
		t.Fatal("text frontend lost approval")
	}
	for len(b.out) > 0 {
		item := <-b.out
		if len(item.Card) != 0 {
			t.Fatal("text frontend received native card")
		}
		if item.Interaction == "request" && !strings.Contains(item.Text, "/approve "+request) {
			t.Fatal("manual approval instructions lost")
		}
	}
	if len(rpc.responses) != 0 {
		t.Fatal("rendering automatically approved")
	}
	b.handle(ctx, input("text-approve", "/approve "+request))
	if len(rpc.responses) != 1 {
		t.Fatal("manual text approval not forwarded")
	}
}

func TestStructuredFrontendCommandsShareInboxAndOwnership(t *testing.T) {
	b, rpc := setup(t)
	catalogSession(t, b, "thread-1")
	command := Command{ChatID: "chat-1", UserID: "ou_owner", RequestID: "web-setting", Name: "effort", Argument: "high", ThreadID: "thread-1"}
	if err := b.SubmitCommand(command); err != nil {
		t.Fatal(err)
	}
	if len(rpc.calls) != 0 {
		t.Fatal("structured submission called model")
	}
	b.handle(context.Background(), <-b.in)
	_, overrides := b.store.ModelPreferences()
	if overrides["thread-1"].Effort != "high" {
		t.Fatal("structured command did not share service")
	}
	command.UserID = "stranger"
	if err := b.SubmitCommand(command); err == nil {
		t.Fatal("unauthorized structured command")
	}
	command.UserID = "ou_owner"
	command.ThreadID = "unknown"
	if err := b.SubmitCommand(command); err == nil {
		t.Fatal("unknown session accepted")
	}
}

func TestTextFrontendFilesReuseExportAndDelivery(t *testing.T) {
	b, rpc := setup(t)
	b.renderer = presentation.Text{}
	transport := &fileFixture{}
	b.sender = transport
	catalogSession(t, b, "thread-1")
	if e := os.WriteFile(filepath.Join(b.cfg.ProjectDir, "public.txt"), []byte("text frontend file"), 0600); e != nil {
		t.Fatal(e)
	}
	b.handle(context.Background(), input("text-file", "/file #1 public.txt"))
	if len(b.out) != 1 {
		t.Fatal("file was not retained")
	}
	item := <-b.out
	if !b.deliverOne(context.Background(), item) || string(transport.sent) != "text frontend file" || len(rpc.calls) != 0 {
		t.Fatal("frontend file capability bypassed or lost")
	}
}

func TestFrontendFormatChangeDoesNotReplayNativePayload(t *testing.T) {
	b, _ := setup(t)
	b.respond(input("old-ui", "/status"), "saved text")
	item := <-b.out
	if len(item.Card) == 0 {
		t.Fatal("fixture has no rich payload")
	}
	sender := &fallbackSender{}
	b.sender = sender
	b.renderer = presentation.Text{}
	if !b.deliverOne(context.Background(), item) || sender.cards != 0 || sender.texts != 1 {
		t.Fatal("foreign native payload replayed")
	}
}

func TestApplicationDoesNotImportFrontendSDKOrRenderNativeCards(t *testing.T) {
	entries, e := os.ReadDir(".")
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, e := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if e != nil {
			t.Fatal(e)
		}
		for _, imp := range parsed.Imports {
			if strings.Contains(imp.Path.Value, "larksuite") || strings.Contains(imp.Path.Value, "/internal/feishu") {
				t.Fatal("frontend dependency in application", name, imp.Path.Value)
			}
		}
		data, e := os.ReadFile(filepath.Clean(name))
		if e != nil {
			t.Fatal(e)
		}
		for _, native := range []string{`"interactive_container"`, `"lark_md"`, `"plain_text"`, `"form_submit"`} {
			if strings.Contains(string(data), native) {
				t.Fatal("native presentation in application", name, native)
			}
		}
	}
}
