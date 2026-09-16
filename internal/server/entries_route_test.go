package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
