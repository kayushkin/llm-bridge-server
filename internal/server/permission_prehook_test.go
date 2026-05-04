package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestWriteHookDecision verifies the response body matches CC's
// hookSpecificOutput contract — wrong field names or shape silently
// fall back to CC's no-permission-prompt behavior (the mode that bit
// the MCP path during the 2026-05-04 rollout).
func TestWriteHookDecision(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		reason   string
	}{
		{"allow", "allow", "rule allow:Bash:ls"},
		{"deny", "deny", "rule deny:Bash:rm -rf /"},
		{"ask", "ask", "no rule matched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeHookDecision(rec, tc.decision, tc.reason)

			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}

			var got struct {
				HookSpecificOutput struct {
					HookEventName            string `json:"hookEventName"`
					PermissionDecision       string `json:"permissionDecision"`
					PermissionDecisionReason string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response body unmarshal: %v\nbody: %s", err, rec.Body.String())
			}
			if got.HookSpecificOutput.HookEventName != "PreToolUse" {
				t.Errorf("hookEventName = %q, want PreToolUse", got.HookSpecificOutput.HookEventName)
			}
			if got.HookSpecificOutput.PermissionDecision != tc.decision {
				t.Errorf("permissionDecision = %q, want %q", got.HookSpecificOutput.PermissionDecision, tc.decision)
			}
			if got.HookSpecificOutput.PermissionDecisionReason != tc.reason {
				t.Errorf("permissionDecisionReason = %q, want %q", got.HookSpecificOutput.PermissionDecisionReason, tc.reason)
			}
		})
	}
}
