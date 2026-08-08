package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// A session row that misstates what it is cannot be un-misstated later: the
// caller is gone, and every consumer downstream — permission gating, folder
// routing, the chat list, the kanban classifier — reads the row as fact. So
// the two fields nothing can reconstruct after the fact are checked at the
// door.
func TestCreateSession_RejectsAnUnclassifiedSession(t *testing.T) {
	cases := []struct {
		name     string
		req      msg.CreateSessionRequest
		wantCode string
	}{
		{
			name:     "no type at all",
			req:      msg.CreateSessionRequest{Purpose: msg.PurposeChat, Origin: "frontend"},
			wantCode: "invalid_session_type",
		},
		{
			name:     "a type nobody downstream knows how to handle",
			req:      msg.CreateSessionRequest{Type: "chat", Purpose: msg.PurposeChat, Origin: "frontend"},
			wantCode: "invalid_session_type",
		},
		{
			name:     "no origin, so who asked for this is lost",
			req:      msg.CreateSessionRequest{Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat},
			wantCode: "missing_origin",
		},
		{
			name:     "whitespace is not an origin",
			req:      msg.CreateSessionRequest{Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "   "},
			wantCode: "missing_origin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, instID := testServerWithInstance(t, "claude_code")
			tc.req.Harness = "claude_code"
			tc.req.InstanceID = instID

			resp := doJSON(t, srv, "POST", "/sessions", tc.req)
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("rejection is not the standard error envelope: %s", body)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", env.Error.Code, tc.wantCode, env.Error.Message)
			}
			if env.Error.Message == "" {
				t.Error("rejection carries no message saying how to fix the request")
			}
		})
	}
}

// An unknown purpose is stored, not refused. Refusing would mean every new
// caller had to land a change to llm-bridge before it could create a session,
// which trades a reporting problem for a coupling problem.
func TestCreateSession_AcceptsAnUnregisteredPurpose(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Harness:    "claude_code",
		InstanceID: instID,
		Type:       msg.SessionTypeAutonomous,
		Purpose:    "some-new-thing",
		Origin:     "a-new-service",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	sess := decodeJSON[msg.ManagedSession](t, resp)
	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.Purpose != "some-new-thing" {
		t.Errorf("purpose = %q; an unknown purpose must be stored verbatim, not blanked", stored.Purpose)
	}
}

// The registry is the only place a purpose→folder mapping lives, so a session
// created with a registered purpose must land in that folder without anyone
// configuring an env var.
func TestCreateSession_FilesByTheRegistry(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	for _, tc := range []struct{ purpose, want string }{
		{msg.PurposeAutoworker, "Scheduled"},
		{msg.PurposeSubagent, "Subagents"},
		{msg.PurposeConformance, "Conformance"},
		// A purpose the old hardcoded default string never listed, which is
		// why these sessions silently never filed.
		{msg.PurposeScheduledTask, "Scheduled"},
		{msg.PurposeDispatcher, "Scheduled"},
		// The old spelling files with the new one instead of splitting the folder.
		{"kanban-dispatcher", "Scheduled"},
	} {
		t.Run(tc.purpose, func(t *testing.T) {
			spec, ok := msg.LookupPurpose(tc.purpose)
			if !ok {
				t.Fatalf("%s is not in the registry", tc.purpose)
			}
			resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
				Harness:    "claude_code",
				InstanceID: instID,
				Type:       spec.Type,
				Purpose:    tc.purpose,
				Origin:     "test",
			})
			if resp.StatusCode != 201 {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
			}
			sess := decodeJSON[msg.ManagedSession](t, resp)
			stored, err := st.GetSession(sess.SessionID)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if stored.FolderName != tc.want {
				t.Errorf("folder = %q, want %q", stored.FolderName, tc.want)
			}
		})
	}
}

// The specimen that started this: a `claude -p` one-shot run from a shell,
// imported off disk, must not come back as a human chat.
func TestDiscoveredSessionIsExternalNotAChat(t *testing.T) {
	_, st, _ := testServerWithInstance(t, "claude_code")

	bridgeID, inserted, err := st.UpsertDiscoveredSession(
		"c5840e48-8758-4b50-91a7-f07f84a0b72a",
		"",
		"Reply with exactly: ok",
		"claude_code",
		"inst-cc-local",
		"", // no purpose recognised — the prompt matches no known prefix
		"",
		discoveredAt, discoveredAt,
	)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Fatal("expected a new row")
	}

	sess, err := st.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Type != msg.SessionTypeExternal {
		t.Errorf("type = %q, want %q — the bridge did not run this session", sess.Type, msg.SessionTypeExternal)
	}
	if sess.Purpose != msg.PurposeDiscovered {
		t.Errorf("purpose = %q, want %q", sess.Purpose, msg.PurposeDiscovered)
	}
	if sess.Origin != msg.OriginDiscovery {
		t.Errorf("origin = %q, want %q — the adapter found this session, it did not create it",
			sess.Origin, msg.OriginDiscovery)
	}
	if strings.HasPrefix(sess.Origin, "llm-bridge-") {
		t.Errorf("origin = %q names the discovering adapter, which is the bug this replaced", sess.Origin)
	}
}

// Discovery does recognise some sessions structurally — a subagent from the
// on-disk layout, a conformance probe by its prompt. That is a real signal and
// must beat the external default.
func TestDiscoveredSessionKeepsARecognisedPurpose(t *testing.T) {
	_, st, _ := testServerWithInstance(t, "claude_code")

	bridgeID, _, err := st.UpsertDiscoveredSession(
		"11111111-2222-3333-4444-555555555555",
		"",
		"go find the thing",
		"claude_code",
		"inst-cc-local",
		msg.PurposeSubagent,
		"Subagents",
		discoveredAt, discoveredAt,
	)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sess, err := st.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Purpose != msg.PurposeSubagent {
		t.Errorf("purpose = %q, want %q", sess.Purpose, msg.PurposeSubagent)
	}
	if sess.Type != msg.SessionTypeSystem {
		t.Errorf("type = %q, want %q — a subagent is not external, it ran under a parent the bridge hosted",
			sess.Type, msg.SessionTypeSystem)
	}
}

// A session discovery imported must not be flagged by the taxonomy guard.
//
// The test above pins two of the three classification fields. The third is
// origin, and leaving it unpinned is what let the guard go red: discovery
// writes origin="discovery" — the honest answer, since discovery is what
// created the row — but the subagent registry entry listed only the two
// adapters, so every discovered subagent came back "unexpected-origin". 64
// sessions on this host, and the guard had no way to go green while a caller
// doing the right thing was reported as a fault.
//
// Checking the whole triple through the guard's own classifier, rather than
// field by field, is the point: the guard fails on the combination, so that
// is what a test has to assert.
func TestDiscoveredSubagentPassesTheTaxonomyGuard(t *testing.T) {
	_, st, _ := testServerWithInstance(t, "claude_code")

	bridgeID, _, err := st.UpsertDiscoveredSession(
		"11111111-2222-3333-4444-555555555555",
		"",
		"go find the thing",
		"claude_code",
		"inst-cc-local",
		msg.PurposeSubagent,
		"Subagents",
		discoveredAt, discoveredAt,
	)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sess, err := st.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	problems := msg.ClassifyPurpose(msg.SessionType(sess.Type), sess.Purpose, sess.Origin)
	for _, p := range problems {
		t.Errorf("taxonomy guard flags a session discovery classified correctly: %s (%s) want=%q got=%q",
			p.Kind, p.Detail, p.Want, p.Got)
	}
	if sess.Origin != msg.OriginDiscovery {
		t.Errorf("origin = %q, want %q — discovery created this row; naming an adapter that only found it is the bug this replaced",
			sess.Origin, msg.OriginDiscovery)
	}
}

// discoveredAt is a fixed timestamp for discovery fixtures. Sessions imported
// off disk carry the harness's own created/updated times, so the value only
// has to be stable, not current.
var discoveredAt = time.Date(2026, 7, 27, 19, 7, 23, 0, time.UTC)
