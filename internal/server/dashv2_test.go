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
