package server

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestCreateSession_LinksUnderTheManagerSessionNamed(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	create := func(manager string) msg.ManagedSession {
		t.Helper()
		resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
			Harness: "claude_code", InstanceID: instID,
			Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeDelegate, Origin: "agent-test",
			ManagerSessionID: manager,
		})
		if resp.StatusCode != 201 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
		}
		return decodeJSON[msg.ManagedSession](t, resp)
	}

	top := create("")
	child := create(top.SessionID)
	grandchild := create(child.SessionID)

	for _, tc := range []struct {
		id, wantManager, wantRoot string
		wantDepth                 int
	}{
		{top.SessionID, "", "", 0},
		{child.SessionID, top.SessionID, top.SessionID, 1},
		{grandchild.SessionID, child.SessionID, top.SessionID, 2},
	} {
		stored, err := st.GetSession(tc.id)
		if err != nil {
			t.Fatalf("get session %s: %v", tc.id, err)
		}
		if stored.ManagerSessionID != tc.wantManager || stored.RootSessionID != tc.wantRoot || stored.Depth != tc.wantDepth {
			t.Errorf("%s: manager=%q root=%q depth=%d, want manager=%q root=%q depth=%d",
				tc.id, stored.ManagerSessionID, stored.RootSessionID, stored.Depth, tc.wantManager, tc.wantRoot, tc.wantDepth)
		}
	}
}

func TestCreateSession_RefusesAManagerSessionThatDoesNotExist(t *testing.T) {
	srv, _, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Harness: "claude_code", InstanceID: instID,
		Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeDelegate, Origin: "agent-test",
		ManagerSessionID: "br_0",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code != "unknown_manager_session" {
		t.Errorf("body = %s, want error code unknown_manager_session", body)
	}
}
