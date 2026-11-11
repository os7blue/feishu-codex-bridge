package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestApprovalSettingsParserWhitelistsEachFieldIndependently(t *testing.T) {
	for _, sample := range []struct{ name, raw, reviewer, policy string }{
		{"known", `{"approvalsReviewer":"user","approvalPolicy":"on-request"}`, "user", "on-request"},
		{"auto_review", `{"approvalsReviewer":"auto_review","approvalPolicy":"untrusted"}`, "auto_review", "untrusted"},
		{"legacy", `{"approvalsReviewer":"guardian_subagent","approvalPolicy":"never"}`, "guardian_subagent", "never"},
		{"granular", `{"approvalsReviewer":"user","approvalPolicy":{"granular":{"mcp_elicitations":true,"rules":false,"sandbox_approval":true,"request_permissions":false,"skill_approval":true}}}`, "user", "granular"},
		{"missing", `{}`, "unknown", "unknown"},
		{"null", `{"approvalsReviewer":null,"approvalPolicy":null}`, "unknown", "unknown"},
		{"unknown", `{"approvalsReviewer":"payload-secret","approvalPolicy":"payload-secret"}`, "unknown", "unknown"},
		{"bad_reviewer", `{"approvalsReviewer":{"rationale":"payload-secret"},"approvalPolicy":"on-request"}`, "unknown", "on-request"},
		{"bad_policy", `{"approvalsReviewer":"user","approvalPolicy":42}`, "user", "unknown"},
		{"granular_null", `{"approvalPolicy":{"granular":null}}`, "unknown", "unknown"},
		{"granular_scalar", `{"approvalPolicy":{"granular":"payload-secret"}}`, "unknown", "unknown"},
		{"granular_empty", `{"approvalPolicy":{"granular":{}}}`, "unknown", "unknown"},
		{"granular_missing_required", `{"approvalPolicy":{"granular":{"rules":true,"sandbox_approval":true}}}`, "unknown", "unknown"},
		{"granular_bad_boolean", `{"approvalPolicy":{"granular":{"mcp_elicitations":true,"rules":"payload-secret","sandbox_approval":true}}}`, "unknown", "unknown"},
		{"granular_null_boolean", `{"approvalPolicy":{"granular":{"mcp_elicitations":true,"rules":null,"sandbox_approval":true}}}`, "unknown", "unknown"},
		{"granular_extra_field", `{"approvalPolicy":{"granular":{"mcp_elicitations":true,"rules":true,"sandbox_approval":true,"payload-secret":true}}}`, "unknown", "unknown"},
		{"granular_extra_variant", `{"approvalPolicy":{"granular":{"mcp_elicitations":true,"rules":true,"sandbox_approval":true},"payload-secret":true}}`, "unknown", "unknown"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			b, rpc := setup(t)
			b.observeApprovalSettings(json.RawMessage(sample.raw))
			s := b.Diagnostics().Snapshot()
			data, _ := json.Marshal(s)
			if s.LastApprovalReviewer != sample.reviewer || s.LastApprovalPolicy != sample.policy || s.Counts["approval_reviewer"][sample.reviewer] != 1 || s.Counts["approval_policy"][sample.policy] != 1 || strings.Contains(string(data), "payload-secret") {
				t.Fatalf("invalid public settings evidence: %s", data)
			}
			if len(rpc.calls) != 0 || len(rpc.responses) != 0 || len(b.out) != 0 || b.cfg.ApprovalPolicy != "on-request" {
				t.Fatal("observation changed runtime policy or emitted traffic")
			}
		})
	}
}

func TestApprovalSettingsObservedOnlyFromSuccessfulThreadRPCs(t *testing.T) {
	b, rpc := setup(t)
	response := json.RawMessage(`{"thread":{"id":"fixture-thread"},"approvalsReviewer":"auto_review","approvalPolicy":"untrusted","rationale":"payload-secret"}`)
	fail := false
	rpc.call = func(_ string, params any) (json.RawMessage, error) {
		if params != "fixture-unchanged-params" {
			t.Fatal("settings observation changed RPC params")
		}
		if fail {
			return response, errors.New("fixture failure")
		}
		return response, nil
	}
	for _, method := range []string{"thread/start", "thread/resume", "thread/fork", "thread/read", "turn/start"} {
		b.rpcCall(context.Background(), method, "fixture-unchanged-params")
	}
	fail = true
	b.rpcCall(context.Background(), "thread/start", "fixture-unchanged-params")
	for _, raw := range []string{`{`, `[]`, `"payload-secret"`} {
		b.observeApprovalSettings(json.RawMessage(raw))
	}
	s := b.Diagnostics().Snapshot()
	if s.Counts["approval_reviewer"]["auto_review"] != 3 || s.Counts["approval_policy"]["untrusted"] != 3 || s.Counts["approval_reviewer"]["unknown"] != 0 {
		t.Fatal("failed, malformed or unrelated RPC updated the observed settings")
	}
}

func TestApprovalSettingsNotificationsRequireActiveThreadAndTurn(t *testing.T) {
	b, _ := setup(t)
	settings := `"threadSettings":{"approvalsReviewer":"user","approvalPolicy":"on-request"}`
	b.event(notification("thread/settings/updated", `{"threadId":"thread-1",`+settings+`}`))
	b.active = &task{chat: "chat-1", owner: "ou_owner", thread: "thread-1", turn: "turn-1"}
	for _, body := range []string{
		`{"threadId":"other",` + settings + `}`,
		`{"threadId":"thread-1","turnId":"other",` + settings + `}`,
	} {
		b.event(notification("thread/settings/updated", body))
	}
	if len(b.Diagnostics().Snapshot().Counts) != 0 {
		t.Fatal("unrelated notification changed settings")
	}
	b.event(notification("thread/settings/updated", `{"threadId":"thread-1",`+settings+`,"rationale":"payload-secret"}`))
	s := b.Diagnostics().Snapshot()
	if s.LastApprovalReviewer != "user" || s.LastApprovalPolicy != "on-request" || s.Counts["approval_reviewer"]["user"] != 1 || len(b.out) != 0 {
		t.Fatal("active thread settings were not observed safely")
	}
	// A missing field replaces a stale known value with unknown.
	b.event(notification("thread/settings/updated", `{"threadId":"thread-1","threadSettings":{}}`))
	s = b.Diagnostics().Snapshot()
	if s.LastApprovalReviewer != "unknown" || s.LastApprovalPolicy != "unknown" {
		t.Fatal("missing fields left stale observed settings")
	}
}
