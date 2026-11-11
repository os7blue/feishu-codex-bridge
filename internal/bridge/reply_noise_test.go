package bridge

import (
	"context"
	"strings"
	"testing"
)

func TestTaskRepliesStartWithActualContentAndKeepSessionRouting(t *testing.T) {
	for _, mode := range []string{"new", "resume", "loaded"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := setup(t)
			x := input("fixture-specific-task", "核对桥接恢复记录和目录授权")
			if mode != "new" {
				catalogSession(t, b, "thread-1")
				x.ThreadID = "thread-1"
				b.loaded["thread-1"] = mode == "loaded"
			}
			b.handle(context.Background(), x)
			b.jobs.Wait()
			if len(b.out) != 0 {
				t.Fatal("empty startup acknowledgements preceded the actual reply")
			}
			b.event(notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","phase":"commentary","text":"恢复回执已确认成功；现在检查两个目录的实际写入。"}}`))
			b.event(notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","phase":"final_answer","text":"两个目录的写入与读回均通过，临时文件已删除。"}}`))
			b.event(notification("turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
			if len(b.out) != 2 {
				t.Fatal("specific progress and final answer were lost or duplicated")
			}
			for _, want := range []string{"恢复回执已确认成功", "两个目录的写入与读回均通过"} {
				item := <-b.out
				if item.ThreadID != "thread-1" || !strings.Contains(item.Text, "[会话 #1]") || !strings.Contains(item.Text, want) {
					t.Fatal("actual content lost its original session or text")
				}
			}
		})
	}
}

func TestToolEventsKeepEvidenceAndApprovalPreviewWithoutEmptyProgress(t *testing.T) {
	b, _ := setup(t)
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	for _, body := range []string{
		`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"command","type":"commandExecution","command":"echo fixture-private-value"}}`,
		`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"files","type":"fileChange","changes":[{"path":"docs/example.md","diff":"+fixture approval details"}]}}`,
		`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"tool","type":"mcpToolCall"}}`,
	} {
		b.event(notification("item/started", body))
	}
	b.event(notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","status":"completed","aggregatedOutput":"fixture-private-value"}}`))
	if len(b.out) != 0 {
		t.Fatal("tool events generated empty executing/modifying/completed messages")
	}
	if b.Diagnostics().Snapshot().Counts["command"]["completed"] != 1 || !strings.Contains(b.active.previews["files"], "docs/example.md") {
		t.Fatal("quiet tool events lost outcome evidence or approval details")
	}
	for _, status := range []string{"failed", "declined"} {
		b.event(notification("item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","status":"`+status+`","aggregatedOutput":"fixture-private-value"}}`))
	}
	if len(b.out) != 2 {
		t.Fatal("tool failure or rejection became silent")
	}
	for len(b.out) > 0 {
		if strings.Contains((<-b.out).Text, "fixture-private-value") {
			t.Fatal("raw command output entered user progress")
		}
	}
}
