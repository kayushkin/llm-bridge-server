package bridgeauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOnlyTheBridgeHostGetsTheToken pins the host check: these binaries call
// noteboard and kanban-store with the same default transport, and this token
// must never reach them.
func TestOnlyTheBridgeHostGetsTheToken(t *testing.T) {
	const token = "scheduler-bridge-service-token-0123456789"
	seen := make(chan string, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(ServiceTokenHeader)
		w.Write([]byte(`{}`))
	})
	bridge := httptest.NewServer(handler)
	defer bridge.Close()
	somethingElse := httptest.NewServer(handler)
	defer somethingElse.Close()

	originalTransport, originalClientTransport := http.DefaultTransport, http.DefaultClient.Transport
	t.Cleanup(func() {
		http.DefaultTransport, http.DefaultClient.Transport = originalTransport, originalClientTransport
	})

	t.Setenv(ServiceTokenEnvironmentVariable, token)
	if err := StampRequestsToBridge(bridge.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(bridge.URL + "/sessions"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != token {
		t.Fatalf("the bridge should get the token, got %q", got)
	}
	if _, err := http.Get(somethingElse.URL + "/api/items"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "" {
		t.Fatalf("another service must not see the bridge token, got %q", got)
	}
	if err := StampRequestsToBridge("not a url"); err == nil {
		t.Fatal("a bridge URL with no host must be refused, not ignored")
	}
}
