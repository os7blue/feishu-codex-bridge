package bridge

import "encoding/json"

// Observe only public protocol response/notification fields. No configuration,
// history, raw diagnostic files, thread IDs or reviewer rationale are retained.
func (b *Bridge) observeApprovalSettings(raw json.RawMessage) {
	var settings struct {
		Reviewer json.RawMessage `json:"approvalsReviewer"`
		Policy   json.RawMessage `json:"approvalPolicy"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return
	}
	var reviewer, policy string
	_ = json.Unmarshal(settings.Reviewer, &reviewer)
	if json.Unmarshal(settings.Policy, &policy) != nil {
		var granular map[string]json.RawMessage
		if json.Unmarshal(settings.Policy, &granular) == nil && len(granular) == 1 && validGranularPolicy(granular["granular"]) {
			policy = "granular"
		}
	}
	b.diagnostics.ApprovalSettings(reviewer, policy)
}

func validGranularPolicy(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, required := range []string{"mcp_elicitations", "rules", "sandbox_approval"} {
		if _, ok := fields[required]; !ok {
			return false
		}
	}
	for key, value := range fields {
		switch key {
		case "mcp_elicitations", "rules", "sandbox_approval", "request_permissions", "skill_approval":
		default:
			return false
		}
		var flag *bool
		if json.Unmarshal(value, &flag) != nil || flag == nil {
			return false
		}
	}
	return true
}
