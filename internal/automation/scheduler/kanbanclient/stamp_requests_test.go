package kanbanclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTheStampReachesKanbanStoreAndNobodyElse pins both halves of the host
// scope: a binary's own client, left on the default transport, presents the
// token to kanban-store, and the same client calling any other service on the
// box does not hand it kanban-store's token.
func TestTheStampReachesKanbanStoreAndNobodyElse(t *testing.T) {
	seenByKanban := make(chan string, 1)
	kanban := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenByKanban <- r.Header.Get(ServiceTokenHeader)
	}))
	defer kanban.Close()
	seenByOther := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenByOther <- r.Header.Get(ServiceTokenHeader)
	}))
	defer other.Close()

	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original; http.DefaultClient.Transport = nil })
	if err := StampRequestsToKanbanStore(kanban.URL, "the-token"); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	ownClient := &http.Client{} // Transport nil, as demo-worker's is
	for _, target := range []string{kanban.URL + "/api/cards/c1/unhold", other.URL + "/api/items/c1"} {
		response, err := ownClient.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		response.Body.Close()
	}
	if got := <-seenByKanban; got != "the-token" {
		t.Errorf("kanban-store saw token %q, want the-token", got)
	}
	if got := <-seenByOther; got != "" {
		t.Errorf("another service was handed kanban-store's token: %q", got)
	}
}

func TestTheStampRefusesAURLWithNoHost(t *testing.T) {
	if err := StampRequestsToKanbanStore("localhost:8305", "the-token"); err == nil {
		t.Fatal("a URL with no scheme has no host to scope the token to; want an error")
	}
}

func TestNoTokenMeansNoStampAndNoError(t *testing.T) {
	original := http.DefaultTransport
	if err := StampRequestsToKanbanStore("http://localhost:8305", ""); err != nil {
		t.Fatalf("unset token: %v", err)
	}
	if http.DefaultTransport != original {
		t.Fatal("an unset token must leave the default transport alone")
	}
}
