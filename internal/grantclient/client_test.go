package grantclient

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestTheServiceTokenIsSentOnlyWhenConfigured(t *testing.T) {
	var mutex sync.Mutex
	var seen []http.Header
	grantStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		seen = append(seen, r.Header.Clone())
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer grantStore.Close()

	if _, err := New(grantStore.URL, "grant-store-service-token").EffectiveResourceIDs(t.Context(), "principal_000001", "can_dispatch_on", "instance"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(grantStore.URL, "grant-store-service-token").EffectiveToolIDs(t.Context(), "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(grantStore.URL, "").EffectiveResourceIDs(t.Context(), "principal_000001", "can_dispatch_on", "instance"); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 3 {
		t.Fatalf("grant-store saw %d requests, want 3", len(seen))
	}
	for index, header := range seen[:2] {
		if got := header.Values(ServiceTokenHeader); len(got) != 1 || got[0] != "grant-store-service-token" {
			t.Errorf("call %d with a token configured sent %s %v, want the token", index, ServiceTokenHeader, got)
		}
	}
	if _, present := seen[2][http.CanonicalHeaderKey(ServiceTokenHeader)]; present {
		t.Errorf("a client with no token configured sent %s", ServiceTokenHeader)
	}
}
