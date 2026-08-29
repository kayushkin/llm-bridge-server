package server

import (
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func seedSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	sess := &store.Session{
		SessionID:   id,
		DisplayName: "disp-" + id,
		Harness:     "claude_code",
		InstanceID:  "inst-1",
		State:       "idle",
		AgentID:     "agent-1",
		Purpose:     "chat",
		Type:        msg.SessionType("interactive"),
		Mode:        msg.SessionModeEvents,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// seedSubagentSession seeds a session that another session spawned. The parent
// pointer is set at CREATE because the store has no generic session update — the
// promoter writes it on insert too (internal/harness/subagent.go).
func seedSubagentSession(t *testing.T, st *store.Store, id, parentID string) {
	t.Helper()
	sess := &store.Session{
		SessionID:        id,
		DisplayName:      "disp-" + id,
		Harness:          "claude_code",
		InstanceID:       "inst-1",
		State:            "idle",
		AgentID:          "agent-1",
		Purpose:          "subagent",
		Type:             msg.SessionType("system"),
		Mode:             msg.SessionModeEvents,
		ManagerSessionID: parentID,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("seed subagent %s: %v", id, err)
	}
}

// The summary endpoint returns the projected list, a revision, an ETag header,
// and honors If-None-Match with a 304.
func TestHandleSessionsSummary_ETagAnd304(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "a")
	seedSession(t, st, "b")

	resp := doJSON(t, srv, "GET", "/sessions/summary?limit=100", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("missing ETag header")
	}
	body := decodeJSON[SummaryResponse](t, resp)
	if len(body.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(body.Sessions))
	}
	if body.Revision == "" {
		t.Errorf("revision must be set")
	}
	if `"`+body.Revision+`"` != etag {
		t.Errorf("ETag %q should quote revision %q", etag, body.Revision)
	}
	// camelCase + RFC3339 timestamps.
	if body.Sessions[0].UpdatedAt == "" || body.Sessions[0].DisplayName == "" {
		t.Errorf("projected wire fields missing: %+v", body.Sessions[0])
	}

	// Conditional GET with the matching ETag → 304.
	req := httptest.NewRequest("GET", "/sessions/summary?limit=100", nil)
	req.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 304 {
		t.Errorf("If-None-Match match should 304, got %d", w.Code)
	}
}

// A mutation must bust the ETag (revision advances) and invalidate the cache so
// the next read reflects the new row.
func TestHandleSessionsSummary_RevisionMovesOnMutation(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "a")

	r1 := decodeJSON[SummaryResponse](t, doJSON(t, srv, "GET", "/sessions/summary", nil))
	seedSession(t, st, "b")
	r2 := decodeJSON[SummaryResponse](t, doJSON(t, srv, "GET", "/sessions/summary", nil))

	if r1.Revision == r2.Revision {
		t.Errorf("revision should advance after inserting a session")
	}
	if len(r2.Sessions) != 2 {
		t.Errorf("cache should have been invalidated; got %d sessions", len(r2.Sessions))
	}
}

// The response cache serves identical bytes for a repeated request and clears on
// notify.
func TestResponseCache_InvalidateOnNotify(t *testing.T) {
	c := newResponseCache()
	c.put("k", []byte("v"))
	if b, ok := c.get("k"); !ok || string(b) != "v" {
		t.Fatalf("get after put failed")
	}
	c.OnSessionChanged("any")
	if _, ok := c.get("k"); ok {
		t.Errorf("cache should be empty after OnSessionChanged")
	}
}

// The notifier fanout forwards to every target.
func TestNotifierFanout_ForwardsToAll(t *testing.T) {
	var a, b []string
	fa := notifierRecorder{onChange: func(id string) { a = append(a, id) }}
	fb := notifierRecorder{onChange: func(id string) { b = append(b, id) }}
	f := newNotifierFanout(&fa, &fb)
	f.OnSessionChanged("x")
	if len(a) != 1 || len(b) != 1 || a[0] != "x" || b[0] != "x" {
		t.Errorf("fanout did not reach both targets: a=%v b=%v", a, b)
	}
}

type notifierRecorder struct {
	onChange  func(string)
	onSignals func(string)
}

func (n *notifierRecorder) OnSessionChanged(id string) {
	if n.onChange != nil {
		n.onChange(id)
	}
}
func (n *notifierRecorder) OnSessionDeleted(string) {}
func (n *notifierRecorder) OnSignalsChanged(id string) {
	if n.onSignals != nil {
		n.onSignals(id)
	}
}

// The id lookup, over HTTP: named sessions come back, and an empty one is a 400.
//
// The 400 is the point of the second half. `?session_id=` with nothing after it
// comes from a caller that meant to name sessions and assembled an empty list —
// an inbox holding no signals is the obvious way to get there. Answering that
// with "don't narrow" hands back the newest hundred sessions on the box, and a
// caller that asked "what are these waiting sessions called?" would render every
// one of them as waiting. Wrong, and indistinguishable from right.
func TestHandleSessionsSummary_SessionIDLookup(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "a")
	seedSession(t, st, "b")
	seedSession(t, st, "c")

	// Repeated parameters, and a comma-separated list, name the same two rows.
	for _, query := range []string{
		"/sessions/summary?session_id=a&session_id=c",
		"/sessions/summary?session_id=a,c",
	} {
		resp := doJSON(t, srv, "GET", query, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status = %d", query, resp.StatusCode)
		}
		body := decodeJSON[SummaryResponse](t, resp)
		if len(body.Sessions) != 2 {
			t.Fatalf("%s: got %d sessions, want the 2 named", query, len(body.Sessions))
		}
		names := map[string]string{}
		for _, s := range body.Sessions {
			names[s.SessionID] = s.DisplayName
		}
		if names["a"] != "disp-a" || names["c"] != "disp-c" {
			t.Errorf("%s: names not carried back: %+v", query, names)
		}
		if _, unwanted := names["b"]; unwanted {
			t.Errorf("%s: returned a session nobody asked for", query)
		}
	}

	// Present but empty is refused, NOT treated as an unfiltered listing.
	resp := doJSON(t, srv, "GET", "/sessions/summary?session_id=", nil)
	if resp.StatusCode != 400 {
		body := decodeJSON[SummaryResponse](t, resp)
		t.Fatalf("empty session_id: status = %d with %d sessions, want 400",
			resp.StatusCode, len(body.Sessions))
	}

	// Absent still lists everything — the lookup must not have become mandatory.
	resp = doJSON(t, srv, "GET", "/sessions/summary?limit=100", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("unfiltered: status = %d", resp.StatusCode)
	}
	if got := len(decodeJSON[SummaryResponse](t, resp).Sessions); got != 3 {
		t.Errorf("unfiltered listing returned %d sessions, want 3", got)
	}
}

// The PARENT lookup over HTTP: name parents, get their children, and get the
// parent pointer back on every row.
//
// Two halves, and the second is the one that was actually missing.
// `manager_session_id` has always been on the full session record, so
// `GET /sessions/{id}` and every SSE upsert carried it — the paged LIST did not.
// The sidebar reads only the list, so it could not tell a parent from a child
// however many rows it loaded, and the 1,325 subagent sessions on this host sat
// in it as unrelated rows.
func TestHandleSessionsSummary_ManagerSessionIDLookup(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "parent")
	seedSession(t, st, "other")
	seedSubagentSession(t, st, "kid1", "parent")
	seedSubagentSession(t, st, "kid2", "parent")
	seedSubagentSession(t, st, "kid3", "other")

	// Repeated and comma-separated name the same parents — the sidebar asks
	// about a whole page of rows in one request.
	for _, query := range []string{
		"/sessions/summary?manager_session_id=parent&manager_session_id=other",
		"/sessions/summary?manager_session_id=parent,other",
	} {
		resp := doJSON(t, srv, "GET", query, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status = %d", query, resp.StatusCode)
		}
		body := decodeJSON[SummaryResponse](t, resp)
		if len(body.Sessions) != 3 {
			t.Fatalf("%s: got %d sessions, want the 3 children", query, len(body.Sessions))
		}
		for _, s := range body.Sessions {
			if s.ManagerSessionID == "" {
				t.Errorf("%s: child %s came back with no parent id", query, s.SessionID)
			}
		}
	}

	// The projection carries it on an UNFILTERED listing too, which is how the
	// sidebar learns it at all. A test that only exercised the lookup above would
	// pass with the column missing from the projection.
	resp := doJSON(t, srv, "GET", "/sessions/summary?limit=100", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("unfiltered: status = %d", resp.StatusCode)
	}
	body := decodeJSON[SummaryResponse](t, resp)
	seen := map[string]string{}
	for _, s := range body.Sessions {
		seen[s.SessionID] = s.ManagerSessionID
	}
	if seen["kid1"] != "parent" {
		t.Errorf("unfiltered listing gave kid1 parent %q, want parent", seen["kid1"])
	}
	// Empty, never a placeholder: a top-level session HAS no parent.
	if seen["parent"] != "" {
		t.Errorf("top-level session reported parent %q, want empty", seen["parent"])
	}

	// Present but empty is refused, the same trap and the same answer as the id
	// lookup: treating it as "don't narrow" would answer "what did this session
	// spawn?" with the newest hundred sessions on the box.
	resp = doJSON(t, srv, "GET", "/sessions/summary?manager_session_id=", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("empty manager_session_id: status = %d, want 400", resp.StatusCode)
	}
}

// POST /sessions/summary — the same query in a body, because the id lookups
// outgrow a URL. A sidebar's worth of manager_session_id parameters reached
// 93 KB of query string on this host, and past ~11.5 KB nginx answers by
// destroying the whole HTTP/2 connection, killing every other request in
// flight — a new chat's /send, its /events stream, and the /signals read whose
// failure was the visible bug. This pins that the POST encoding answers the
// same questions the GET does, and refuses the same traps.
func TestHandleSessionsSummaryLookup_PostBody(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "a")
	seedSession(t, st, "b")
	seedSubagentSession(t, st, "kid1", "a")
	seedSubagentSession(t, st, "kid2", "b")

	// The session_ids lookup answers over POST.
	resp := doJSON(t, srv, "POST", "/sessions/summary",
		SummaryLookupRequest{SessionIDs: &[]string{"a"}})
	if resp.StatusCode != 200 {
		t.Fatalf("session_ids lookup: status = %d", resp.StatusCode)
	}
	body := decodeJSON[SummaryResponse](t, resp)
	if len(body.Sessions) != 1 || body.Sessions[0].SessionID != "a" {
		t.Fatalf("session_ids lookup: got %+v, want just session a", body.Sessions)
	}

	// The manager_session_ids lookup answers over POST, several parents at once.
	resp = doJSON(t, srv, "POST", "/sessions/summary",
		SummaryLookupRequest{ManagerSessionIDs: &[]string{"a", "b"}})
	if resp.StatusCode != 200 {
		t.Fatalf("manager_session_ids lookup: status = %d", resp.StatusCode)
	}
	body = decodeJSON[SummaryResponse](t, resp)
	if len(body.Sessions) != 2 {
		t.Fatalf("manager_session_ids lookup: got %d sessions, want the 2 children", len(body.Sessions))
	}
	for _, s := range body.Sessions {
		if s.ManagerSessionID == "" {
			t.Errorf("child %s came back with no parent id", s.SessionID)
		}
	}

	// Present but empty is a 400 over POST too — the pointer fields exist to
	// keep that distinction through JSON.
	for _, req := range []SummaryLookupRequest{
		{SessionIDs: &[]string{}},
		{ManagerSessionIDs: &[]string{}},
		{SessionIDs: &[]string{"", "  "}},
	} {
		resp = doJSON(t, srv, "POST", "/sessions/summary", req)
		if resp.StatusCode != 400 {
			t.Errorf("empty id list %+v: status = %d, want 400", req, resp.StatusCode)
		}
	}

	// A misspelled field is a 400, not a silently unfiltered listing — the
	// strict decoder is what stands between a typo and a wrong answer.
	resp = doJSON(t, srv, "POST", "/sessions/summary",
		map[string]any{"session_id": []string{"a"}})
	if resp.StatusCode != 400 {
		t.Errorf("unknown field: status = %d, want 400", resp.StatusCode)
	}

	// An empty body is the unfiltered listing, same as a bare GET.
	resp = doJSON(t, srv, "POST", "/sessions/summary", SummaryLookupRequest{})
	if resp.StatusCode != 200 {
		t.Fatalf("unfiltered POST: status = %d", resp.StatusCode)
	}
	if got := len(decodeJSON[SummaryResponse](t, resp).Sessions); got != 4 {
		t.Errorf("unfiltered POST returned %d sessions, want 4", got)
	}
}
