package bridgeauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOnlyTheBridgeHostGetsTheToken pins the host check: reminder-coordinator
// calls dash with the same default transport, and this token must never reach
// it or anything else that is not the bridge.
func TestOnlyTheBridgeHostGetsTheToken(t *testing.T) {
	const token = "llm-bridge-service-principal-token-v1.worker-0123456789"
	seen := make(chan string, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(AuthorizationHeader)
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

	if err := StampRequestsToBridge(bridge.URL, token); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(bridge.URL + "/sessions"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "Bearer "+token {
		t.Fatalf("the bridge should get the token, got %q", got)
	}
	if _, err := http.Get(somethingElse.URL + "/api/calendar/google"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "" {
		t.Fatalf("another service must not see the bridge token, got %q", got)
	}
	if err := StampRequestsToBridge("not a url", token); err == nil {
		t.Fatal("a bridge URL with no host must be refused, not ignored")
	}
}
