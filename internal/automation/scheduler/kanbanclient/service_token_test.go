package kanbanclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEveryCallCarriesTheServiceToken pins the transport: kanban-store gates
// every route, so a job whose client forgets the header does no work and says
// nothing useful about why.
func TestEveryCallCarriesTheServiceToken(t *testing.T) {
	const token = "scheduler-test-service-token-0123456789"
	seen := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(ServiceTokenHeader)
		w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	client := New(upstream.URL, token)
	if _, err := client.HTTP.Get(upstream.URL + "/api/boards"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != token {
		t.Fatalf("header = %q, want the service token", got)
	}

	withoutToken := New(upstream.URL, "")
	if _, err := withoutToken.HTTP.Get(upstream.URL + "/api/boards"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "" {
		t.Fatalf("with no token configured the header must be absent, got %q", got)
	}
}
