package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// TestIsUnattendedSession pins the gate that decides whether a prehook ask
// parks for a human or resolves deterministically. Only autonomous sessions
// (autoworker and the like) are unattended: getting this wrong either hangs a
// background worker forever (false negative) or silently auto-allows an
// interactive user's tool calls (false positive).
func TestIsUnattendedSession(t *testing.T) {
	cases := []struct {
		name string
		sess *store.Session
		want bool
	}{
		{"nil", nil, false},
		{"interactive", &store.Session{Type: msg.SessionTypeInteractive}, false},
		{"autonomous", &store.Session{Type: msg.SessionTypeAutonomous}, true},
		{"system", &store.Session{Type: msg.SessionTypeSystem}, false},
		{"empty-type", &store.Session{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnattendedSession(tc.sess); got != tc.want {
				t.Errorf("isUnattendedSession(%+v) = %v, want %v", tc.sess, got, tc.want)
			}
		})
	}
}

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
			writeHookDecision(rec, tc.decision, tc.reason, nil)

			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}

			var got struct {
				HookSpecificOutput struct {
					HookEventName            string          `json:"hookEventName"`
					PermissionDecision       string          `json:"permissionDecision"`
					PermissionDecisionReason string          `json:"permissionDecisionReason"`
					UpdatedInput             json.RawMessage `json:"updatedInput"`
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
			if len(got.HookSpecificOutput.UpdatedInput) != 0 {
				t.Errorf("updatedInput = %s, want absent when not provided", string(got.HookSpecificOutput.UpdatedInput))
			}
		})
	}
}

// TestWriteHookDecisionWithUpdatedInput verifies that a non-nil
// updatedInput is forwarded inside hookSpecificOutput. AskUserQuestion's
// answer flow depends on this: the parked-ask resolve carries {answers:…}
// as updatedInput, CC merges it into the tool input, and the tool's call()
// returns those answers without ever invoking the interactive prompt.
func TestWriteHookDecisionWithUpdatedInput(t *testing.T) {
	rec := httptest.NewRecorder()
	updated := json.RawMessage(`{"questions":[{"question":"Which color?"}],"answers":{"Which color?":"Red"}}`)
	writeHookDecision(rec, "allow", "user picked Red", updated)

	var got struct {
		HookSpecificOutput struct {
			HookEventName            string          `json:"hookEventName"`
			PermissionDecision       string          `json:"permissionDecision"`
			PermissionDecisionReason string          `json:"permissionDecisionReason"`
			UpdatedInput             json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if got.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("permissionDecision = %q, want allow", got.HookSpecificOutput.PermissionDecision)
	}
	// Compare as JSON values, not byte-equal — the encoding may reorder keys.
	var want, have any
	if err := json.Unmarshal(updated, &want); err != nil {
		t.Fatalf("unmarshal expected updatedInput: %v", err)
	}
	if err := json.Unmarshal(got.HookSpecificOutput.UpdatedInput, &have); err != nil {
		t.Fatalf("unmarshal received updatedInput: %v", err)
	}
	wantJSON, _ := json.Marshal(want)
	haveJSON, _ := json.Marshal(have)
	if string(wantJSON) != string(haveJSON) {
		t.Errorf("updatedInput mismatch:\nwant: %s\nhave: %s", string(wantJSON), string(haveJSON))
	}
}
