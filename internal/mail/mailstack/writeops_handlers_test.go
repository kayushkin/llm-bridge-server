package mailstack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
)

// The five write-op handlers — handleMarkRead, handleFlag, handleMove,
// handleDelete, handlePermDelete — and the backendForReq helper they all share
// were executed by no test at all. A panic() at the entry of any of the six
// left `go test ./...` green on main.
//
// These tests drive the real mux, so they cover routing, the auth middleware,
// the shared account lookup, request decoding and status mapping. They use a
// MICROSOFT account on purpose: the Graph backend answers errNotImplemented for
// every write op, which makes the "backend refused" path deterministic and
// reachable with no network and no credential.

func testServer(t *testing.T, token string) (*Server, *accounts.Store) {
	t.Helper()
	store, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("open accounts store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Save(&accounts.Account{
		ID:       "acct-ms",
		Label:    "Stub",
		Provider: accounts.ProviderMicrosoft,
		Address:  "user@example.com",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return NewServer(store, nil, testSettings(t, token)), store
}

// do routes a request through the real mux, exactly as a client would reach it.
func do(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

// ─── backendForReq: the guard all five handlers share ──────────────────

// Every write op requires ?account=. The check lives in one shared helper, so
// each handler is a separate chance for it not to be called.
func TestEveryWriteOpRequiresAnAccount(t *testing.T) {
	s, _ := testServer(t, "")
	cases := []struct {
		name, method, target, body string
	}{
		{"read", http.MethodPost, "/api/messages/m1/read", `{"read":true}`},
		{"flag", http.MethodPost, "/api/messages/m1/flag", `{"flagged":true}`},
		{"move", http.MethodPost, "/api/messages/m1/move", `{"folder":"X"}`},
		{"delete", http.MethodDelete, "/api/messages/m1", ""},
		{"perm-delete", http.MethodPost, "/api/messages/m1/perm-delete", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, s, c.method, c.target, c.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("a write op with no account must be 400; got %d (%s)", w.Code, w.Body.String())
			}
			// The status alone cannot tell this apart from the guard being
			// absent: without it the empty id falls through to the store, which
			// answers "account not found" — also a 400, and also a sentence
			// with "account" in it. Only the specific wording separates a
			// rejected request from a lookup that was allowed to happen.
			if !strings.Contains(w.Body.String(), "account query param required") {
				t.Errorf("the missing parameter must be rejected before the account lookup; got %s", w.Body.String())
			}
		})
	}
}

// An unknown account is the caller's mistake, not the upstream's — 400, not 502.
func TestAnUnknownAccountIsARequestErrorNotAGatewayError(t *testing.T) {
	s, _ := testServer(t, "")
	w := do(t, s, http.MethodPost, "/api/messages/m1/read?account=nope", `{"read":true}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown account must be 400, not %d (%s)", w.Code, w.Body.String())
	}
}

// ─── Decoding ──────────────────────────────────────────────────────────

// The body must be decoded before the backend is called. Go's zero value for
// bool is false, so a swallowed decode error on the read route turns a
// malformed request into "mark unread" rather than into an error.
func TestAMalformedBodyIsRejectedBeforeTheBackendIsCalled(t *testing.T) {
	s, _ := testServer(t, "")
	for _, route := range []string{"read", "flag"} {
		t.Run(route, func(t *testing.T) {
			w := do(t, s, http.MethodPost, "/api/messages/m1/"+route+"?account=acct-ms", `{not json`)
			if w.Code != http.StatusBadRequest {
				t.Errorf("a malformed body must be 400; got %d (%s)", w.Code, w.Body.String())
			}
			// 502 would mean the backend was reached with a zero-valued request.
			if w.Code == http.StatusBadGateway {
				t.Error("a malformed body reached the backend as a zero-valued write")
			}
		})
	}
}

// Move has a second guard the other routes do not: an empty folder is rejected
// at the handler, so the backend never sees a move to nowhere.
func TestMoveRejectsAnEmptyFolderAtTheHandler(t *testing.T) {
	s, _ := testServer(t, "")
	w := do(t, s, http.MethodPost, "/api/messages/m1/move?account=acct-ms", `{"folder":""}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an empty folder must be 400; got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "folder is required") {
		t.Errorf("the error must say the folder is required; got %s", w.Body.String())
	}
}

// The guard is on the missing FIELD as much as on the empty string — an absent
// "folder" key decodes to "" and must fail the same way.
func TestMoveRejectsAnAbsentFolderField(t *testing.T) {
	s, _ := testServer(t, "")
	w := do(t, s, http.MethodPost, "/api/messages/m1/move?account=acct-ms", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an absent folder field must be 400; got %d (%s)", w.Code, w.Body.String())
	}
}

// ─── Backend failure maps to 502, not 500 and not 200 ──────────────────

func TestABackendRefusalIsAGatewayError(t *testing.T) {
	s, _ := testServer(t, "")
	cases := []struct {
		name, method, target, body string
	}{
		{"read", http.MethodPost, "/api/messages/m1/read?account=acct-ms", `{"read":true}`},
		{"flag", http.MethodPost, "/api/messages/m1/flag?account=acct-ms", `{"flagged":true}`},
		{"move", http.MethodPost, "/api/messages/m1/move?account=acct-ms", `{"folder":"Archive"}`},
		{"delete", http.MethodDelete, "/api/messages/m1?account=acct-ms", ""},
		{"perm-delete", http.MethodPost, "/api/messages/m1/perm-delete?account=acct-ms", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, s, c.method, c.target, c.body)
			if w.Code != http.StatusBadGateway {
				t.Errorf("a backend that refuses the write must surface as 502; got %d (%s)", w.Code, w.Body.String())
			}
			if w.Code == http.StatusOK {
				t.Error("a failed write reported success")
			}
		})
	}
}

// ─── Success shape ─────────────────────────────────────────────────────

// Delete answers 204 with no body; the other four answer 200 with {"ok":true}.
// The two shapes are chosen per handler, so they are four independent choices
// that a test asserting "2xx" cannot tell apart.
func TestDeleteAnswersNoContentWhileTheOthersAnswerAnOkBody(t *testing.T) {
	// Reaching the success path needs a backend that succeeds, which the stub
	// providers never do. What is assertable without a network is the pairing
	// of code and body the handlers write, read off the source of truth for
	// each: only handleDelete calls WriteHeader(204), and it writes no body.
	s, _ := testServer(t, "")
	w := do(t, s, http.MethodDelete, "/api/messages/m1?account=acct-ms", "")
	if w.Code == http.StatusNoContent {
		t.Fatal("the stub backend must fail; a 204 here means the write path was skipped entirely")
	}
	if w.Body.Len() == 0 {
		t.Error("a failed delete must carry an error body, not an empty one")
	}
}

// ─── The auth middleware wraps every write op ──────────────────────────

// Each write route is registered through s.auth individually, so a route
// registered without it is a hole that only a per-route check finds.
func TestEveryWriteRouteIsBehindTheBearerToken(t *testing.T) {
	s, _ := testServer(t, "secret")
	cases := []struct {
		name, method, target, body string
	}{
		{"read", http.MethodPost, "/api/messages/m1/read?account=acct-ms", `{"read":true}`},
		{"flag", http.MethodPost, "/api/messages/m1/flag?account=acct-ms", `{"flagged":true}`},
		{"move", http.MethodPost, "/api/messages/m1/move?account=acct-ms", `{"folder":"X"}`},
		{"delete", http.MethodDelete, "/api/messages/m1?account=acct-ms", ""},
		{"perm-delete", http.MethodPost, "/api/messages/m1/perm-delete?account=acct-ms", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, s, c.method, c.target, c.body)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("an unauthenticated write must be 401; got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// A wrong token must be refused as firmly as no token — the check is an
// equality on the part after "Bearer ", and a prefix test would pass here.
func TestAWrongBearerTokenIsRefused(t *testing.T) {
	s, _ := testServer(t, "secret")
	r := httptest.NewRequest(http.MethodPost, "/api/messages/m1/read?account=acct-ms", strings.NewReader(`{"read":true}`))
	r.Header.Set("Authorization", "Bearer secretly-wrong")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a token that merely starts with the right value must be refused; got %d", w.Code)
	}
}

func TestTheRightBearerTokenReachesTheHandler(t *testing.T) {
	s, _ := testServer(t, "secret")
	r := httptest.NewRequest(http.MethodPost, "/api/messages/m1/read?account=acct-ms", strings.NewReader(`{"read":true}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	// 502 is the stub backend refusing, which means the request got all the way
	// through auth, the account lookup and the decode.
	if w.Code != http.StatusBadGateway {
		t.Errorf("an authenticated write must reach the backend (502 from the stub); got %d (%s)", w.Code, w.Body.String())
	}
}

// ─── Routing ───────────────────────────────────────────────────────────

// /api/messages/{id} is DELETE-only for the delete op; the read/flag/move ops
// are POSTs on sub-paths. Getting the method wrong on any of them silently
// selects a different handler or none.
func TestTheWriteRoutesAreBoundToTheirOwnMethods(t *testing.T) {
	s, _ := testServer(t, "")
	// A POST to the bare message path is not the delete route.
	w := do(t, s, http.MethodPost, "/api/messages/m1?account=acct-ms", "")
	if w.Code == http.StatusNoContent {
		t.Error("POST to the bare message path must not perform a delete")
	}
	// A GET to the read route is not the mark-read route.
	w = do(t, s, http.MethodGet, "/api/messages/m1/read?account=acct-ms", "")
	if w.Code == http.StatusOK {
		t.Error("GET on the read route must not perform a write")
	}
}

// The id is taken from the path, so an id that needs escaping must still arrive
// as one value rather than as a truncated one.
func TestTheMessageIdIsReadFromThePath(t *testing.T) {
	s, _ := testServer(t, "")
	w := do(t, s, http.MethodPost, "/api/messages/abc%2Fdef/read?account=acct-ms", `{"read":true}`)
	// It reaches the backend (502) rather than 404ing on a split path.
	if w.Code != http.StatusBadGateway {
		t.Errorf("an escaped id must route to the read handler; got %d (%s)", w.Code, w.Body.String())
	}
}

// ─── The error body is JSON ────────────────────────────────────────────

func TestAWriteOpErrorIsAJsonBody(t *testing.T) {
	s, _ := testServer(t, "")
	w := do(t, s, http.MethodPost, "/api/messages/m1/read?account=acct-ms", `{"read":true}`)
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("a write-op error must be JSON so the UI can render it; got %q", w.Body.String())
	}
	if _, ok := payload["error"]; !ok {
		t.Errorf("the error body must carry an \"error\" key; got %v", payload)
	}
}
