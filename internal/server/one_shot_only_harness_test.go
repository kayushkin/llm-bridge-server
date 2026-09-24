package server

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// A one-shot-only harness has no session mode, so a session on it would spawn
// a wrapper that exits at once. The request is refused at the door instead,
// with the reason, before any row is written.
func TestCreateSession_RefusesAOneShotOnlyHarness(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessDatabricks)
	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Harness: msg.HarnessDatabricks, InstanceID: instanceID,
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "frontend",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("body is not a JSON error: %s", body)
	}
	if envelope.Error.Code != "harness_has_no_sessions" || envelope.Error.Message != oneShotOnlyHarnesses[msg.HarnessDatabricks] {
		t.Errorf("error = %+v", envelope.Error)
	}
}

// Every one-shot-only harness is one this gateway surfaces; a stale entry
// would refuse sessions on a harness nobody can pick.
func TestOneShotOnlyHarnessesAreEnabled(t *testing.T) {
	for harness, reason := range oneShotOnlyHarnesses {
		if !isValidHarness(harness) {
			t.Errorf("oneShotOnlyHarnesses names %q, which this gateway does not surface", harness)
		}
		if reason == "" {
			t.Errorf("oneShotOnlyHarnesses gives %q no reason", harness)
		}
	}
}
