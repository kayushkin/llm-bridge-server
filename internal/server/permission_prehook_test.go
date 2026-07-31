package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/permclient"
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

// TestAutoAllowParkedPermissionAsks pins the Allow All (bypass) settle path:
// switching a session to bypass must deliver "allow" to permission-prompt
// asks already parked on the banner, while leaving AskUserQuestion
// (user_input) asks parked so their answer payload isn't stripped.
func TestAutoAllowParkedPermissionAsks(t *testing.T) {
	srv := &Server{parkedAsks: newParkedAsks()}
	const bridgeID = "bridge-1"

	permCh := srv.parkedAsks.park(bridgeID, "req-perm")
	askCh := srv.parkedAsks.park(bridgeID, "req-ask")

	pending := []msg.Event{
		{Hook: &msg.HookEvent{Source: msg.HookSourcePermission, RequestID: "req-perm", Phase: "awaiting_resolution"}},
		{Hook: &msg.HookEvent{Source: msg.HookSourceUserInput, RequestID: "req-ask", Phase: "awaiting_resolution"}},
	}

	n := srv.autoAllowParkedPermissionAsks(bridgeID, pending)
	if n != 1 {
		t.Fatalf("resolved = %d, want 1 (permission ask only)", n)
	}

	// The permission ask must have received an allow on its channel.
	select {
	case d := <-permCh:
		if d.Behavior != "allow" {
			t.Errorf("permission ask behavior = %q, want allow", d.Behavior)
		}
		if d.ResolvedBy != "auto:bypass-mode" {
			t.Errorf("permission ask resolvedBy = %q, want auto:bypass-mode", d.ResolvedBy)
		}
	default:
		t.Error("permission ask channel got no decision")
	}

	// The user_input ask must remain parked (no delivery).
	select {
	case d := <-askCh:
		t.Errorf("user_input ask should not be auto-allowed, got %+v", d)
	default:
	}
}

// seedPrehookSession puts one session of the given type in the store so the
// prehook handler can look up whether a human is attached.
func seedPrehookSession(t *testing.T, st *store.Store, id string, typ msg.SessionType) {
	t.Helper()
	sess := &store.Session{
		SessionID:   id,
		DisplayName: "disp-" + id,
		Harness:     "claude_code",
		InstanceID:  "inst-1",
		State:       "idle",
		Purpose:     "chat",
		Type:        typ,
		Mode:        msg.SessionModeEvents,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// postPrehook drives the real handler the way Claude Code does and returns the
// permissionDecision and its reason.
func postPrehook(t *testing.T, srv *Server, bridgeID, body string) (decision, reason string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/permission/cc-prehook/"+bridgeID, strings.NewReader(body))
	req.SetPathValue("bridge_id", bridgeID)
	rec := httptest.NewRecorder()
	srv.handleCCPermissionPrehook(rec, req)

	var got struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	return got.HookSpecificOutput.PermissionDecision, got.HookSpecificOutput.PermissionDecisionReason
}

// prehookServerWithDeadStore returns a server whose permission-store client
// points at a port nothing listens on, so Evaluate fails for real rather than
// through a stub. 127.0.0.1:1 is the same dead address the e2e scripts use.
func prehookServerWithDeadStore(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	srv, st := testServer(t)
	srv.permClient = permclient.New("http://127.0.0.1:1")
	return srv, st
}

// TestPrehookUnreachableStoreDeniesUnattendedSession is the defect this file's
// writeHookNoVerdict exists for. An autonomous session has nobody to answer an
// "ask", so when permission-store cannot be reached the gate must fail closed
// with a reason the agent can act on. Measured before the fix on bridge session
// br_1785306448183126697: the Task call got a raw "ask" carrying a dial error
// and the turn gave up without a decision.
func TestPrehookUnreachableStoreDeniesUnattendedSession(t *testing.T) {
	srv, st := prehookServerWithDeadStore(t)
	seedPrehookSession(t, st, "bridge-auto", msg.SessionTypeAutonomous)

	decision, reason := postPrehook(t, srv, "bridge-auto",
		`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)

	if decision != "deny" {
		t.Errorf("permissionDecision = %q, want deny (unattended session cannot answer an ask)", decision)
	}
	if !strings.Contains(reason, "permission-store evaluate failed") || !strings.Contains(reason, "connection refused") {
		t.Errorf("reason = %q, want it to carry the store's own failure verbatim", reason)
	}
	if !strings.Contains(reason, "no human is attached") {
		t.Errorf("reason = %q, want it to explain why the call was denied rather than parked", reason)
	}
}

// TestPrehookUnreachableStoreAsksInteractiveSession is the other half: a human
// IS attached, so the same failure must still reach them as an ask rather than
// being decided for them. Denying here would break every interactive session
// the moment permission-store restarts.
func TestPrehookUnreachableStoreAsksInteractiveSession(t *testing.T) {
	srv, st := prehookServerWithDeadStore(t)
	seedPrehookSession(t, st, "bridge-human", msg.SessionTypeInteractive)

	decision, reason := postPrehook(t, srv, "bridge-human",
		`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)

	if decision != "ask" {
		t.Errorf("permissionDecision = %q, want ask (a human can still answer)", decision)
	}
	if !strings.Contains(reason, "permission-store evaluate failed") || !strings.Contains(reason, "connection refused") {
		t.Errorf("reason = %q, want it to carry the store's own failure verbatim", reason)
	}
}

// TestPrehookUndecodablePayloadDeniesUnattendedSession covers the two failures
// that happen before the tool name is even known. They were reached before the
// session lookup, so they could not tell an unattended session from an
// interactive one; the lookup now happens first.
func TestPrehookUndecodablePayloadDeniesUnattendedSession(t *testing.T) {
	srv, st := prehookServerWithDeadStore(t)
	seedPrehookSession(t, st, "bridge-auto", msg.SessionTypeAutonomous)
	seedPrehookSession(t, st, "bridge-human", msg.SessionTypeInteractive)

	decision, reason := postPrehook(t, srv, "bridge-auto", `{"tool_name":`)
	if decision != "deny" {
		t.Errorf("unattended: permissionDecision = %q, want deny", decision)
	}
	if !strings.Contains(reason, "decode prehook payload") {
		t.Errorf("unattended: reason = %q, want it to name the decode failure", reason)
	}

	decision, _ = postPrehook(t, srv, "bridge-human", `{"tool_name":`)
	if decision != "ask" {
		t.Errorf("interactive: permissionDecision = %q, want ask", decision)
	}
}

// TestPrehookUnknownSessionAsks pins the conservative reading of an absent
// session. GetSession's error is deliberately dropped, so a session the store
// has never heard of is not unattended — inventing a deny for an unknown id
// would let a store hiccup silently block a real user's tools.
func TestPrehookUnknownSessionAsks(t *testing.T) {
	srv, _ := prehookServerWithDeadStore(t)

	decision, _ := postPrehook(t, srv, "bridge-nobody",
		`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)

	if decision != "ask" {
		t.Errorf("permissionDecision = %q, want ask for an unknown session", decision)
	}
}
