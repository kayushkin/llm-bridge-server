package server

import (
	"context"
	"encoding/json"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
)

// newTestServer wires a Server over a throwaway store. No forwarder: none of
// the read paths touch it, and a nil one keeps logstack out of a unit test.
// newTestStore opens the store on a Postgres database of the test's own, with
// the module provisioned and migrated, as the server opens it.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	module, err := store.Module()
	if err != nil {
		t.Fatalf("store.Module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	writer := database.Open(module)
	reader, err := store.OpenReader(context.Background(), database.URL(), module)
	if err != nil {
		t.Fatalf("store.OpenReader: %v", err)
	}
	s := store.New(writer, reader)
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	s := newTestStore(t)
	return New(s, nil), s
}

func harnessBody(t *testing.T, harnessSessionID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"harness_session_id": harnessSessionID,
		"harness":            "claude_code",
		"text":               "hello",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

type byHarnessIDResponse struct {
	HarnessSessionID string              `json:"harness_session_id"`
	Sessions         []store.HeldSession `json:"sessions"`
}

func getByHarnessID(t *testing.T, srv *Server, query string) (*httptest.ResponseRecorder, byHarnessIDResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/by-harness-id"+query, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var out byHarnessIDResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

// The route has to answer over HTTP, not just in the store: the only caller
// that matters is in another process.
func TestSessionsByHarnessIDOverHTTP(t *testing.T) {
	srv, st := newTestServer(t)
	st.StoreEvent(context.Background(), "br_held", "user_message", harnessBody(t, "cc-uuid-held"))
	st.StoreEvent(context.Background(), "br_held", "assistant", harnessBody(t, "cc-uuid-held"))
	st.StoreEvent(context.Background(), "br_other", "user_message", harnessBody(t, "cc-uuid-other"))

	rec, out := getByHarnessID(t, srv, "?harness_session_id=cc-uuid-held")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if out.HarnessSessionID != "cc-uuid-held" {
		t.Errorf("echoed id = %q, want cc-uuid-held", out.HarnessSessionID)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != "br_held" {
		t.Fatalf("sessions = %+v, want one br_held", out.Sessions)
	}
	if out.Sessions[0].EventCount != 2 {
		t.Errorf("event_count = %d, want 2", out.Sessions[0].EventCount)
	}
}

// A harness session log-store has never seen is a 200 with an empty list, and
// the list must marshal as [] rather than null — a caller checking length on
// null still works, but one that iterates a decoded field does not.
func TestSessionsByHarnessIDUnknownIsEmptyList(t *testing.T) {
	srv, st := newTestServer(t)
	st.StoreEvent(context.Background(), "br_held", "user_message", harnessBody(t, "cc-uuid-held"))

	rec, out := getByHarnessID(t, srv, "?harness_session_id=cc-uuid-never-seen")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(out.Sessions) != 0 {
		t.Errorf("sessions = %+v, want none", out.Sessions)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if string(raw["sessions"]) != "[]" {
		t.Errorf("sessions marshalled as %s, want []", raw["sessions"])
	}
}

// A missing id is a 400. Answering it as "no match" would tell an importer to
// import, which is the behavior this endpoint exists to stop; answering it as
// a lookup for ” would report every id-less transcript on the host.
func TestSessionsByHarnessIDRequiresTheID(t *testing.T) {
	srv, st := newTestServer(t)
	st.StoreEvent(context.Background(), "br_anonymous", "user_message", []byte(`{"text":"no harness id here"}`))

	for _, query := range []string{"", "?harness_session_id="} {
		rec, _ := getByHarnessID(t, srv, query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %q status = %d, want 400: %s", query, rec.Code, rec.Body.String())
		}
	}
}

// Registering the route as /sessions/by-harness-id/{id} would overlap
// /sessions/{id}/messages and panic ServeMux at construction. New() running at
// all is the assertion; the sibling routes still resolving is the rest of it.
func TestRoutesDoNotCollide(t *testing.T) {
	srv, st := newTestServer(t)
	st.StoreEvent(context.Background(), "br_held", "user_message", harnessBody(t, "cc-uuid-held"))

	for _, path := range []string{
		"/api/v1/sessions/br_held/messages?limit=30",
		"/api/v1/sessions/br_held/events?after=0",
		"/api/v1/sessions/br_held/turn-state",
		"/api/v1/sessions/search?q=hello",
		"/api/v1/sessions/by-harness-id?harness_session_id=cc-uuid-held",
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}
}
