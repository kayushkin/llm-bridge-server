package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// A shortened tool payload on the reading page is expanded through this route, so
// it must reach log-store's entry route unchanged and pass its answer back — a 404
// included, which is how log-store says the event is not an entry of that page.
func TestEntryRouteProxiesToLogStore(t *testing.T) {
	logStore := newCaptureLogStore(t, `{"id":"e_42","eventId":42}`)
	srv, _ := serverWithLogStore(t, logStore.URL)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/br_1/entries/42", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(logStore.requests) != 1 || logStore.requests[0] != "/api/v1/sessions/br_1/entries/42" {
		t.Fatalf("upstream calls: %q", logStore.requests)
	}
	if got := w.Body.String(); got != `{"id":"e_42","eventId":42}` {
		t.Fatalf("body not passed through: %q", got)
	}
}

func TestEntryRoutePassesALogStoreRefusalThrough(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such entry", http.StatusNotFound)
	}))
	t.Cleanup(notFound.Close)
	srv, _ := serverWithLogStore(t, notFound.URL)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/br_1/entries/7", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 passed through", w.Code)
	}
}

// The reading pages chat-core boots from ask log-store for tool payload previews, as
// /messages does.
func TestRecentBundleAsksForPreviews(t *testing.T) {
	logStore := newCaptureLogStore(t, `{}`)
	srv, st := serverWithLogStore(t, logStore.URL)
	if err := st.CreateSession(&store.Session{
		SessionID:  "br_bundle",
		Harness:    "claude-code",
		InstanceID: "inst_test",
		State:      "idle",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/recent-bundle?n=5&turns=30", nil))
	var sawBundle bool
	for _, uri := range logStore.requests {
		if strings.HasPrefix(uri, "/api/v1/sessions/bundle?") {
			sawBundle = true
			if !strings.Contains(uri, "payload=preview") {
				t.Fatalf("bundle request without previews: %s", uri)
			}
		}
	}
	if !sawBundle {
		t.Fatalf("no bundle request reached log-store: %v (status %d %s)", logStore.requests, w.Code, w.Body.String())
	}
}
