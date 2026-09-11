package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// default_principal_id is the one pref a person needs to clear, so the merge
// keys on the body carrying the key rather than on the value being non-empty.
func TestBridgePrefsDefaultPrincipalIsSetAndClearedByKeyPresence(t *testing.T) {
	srv, _ := testServer(t)
	read := func() string {
		resp := doJSON(t, srv, "GET", "/bridge-prefs", nil)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(raw)
	}
	put := func(body string) {
		req := httptest.NewRequest("PUT", "/bridge-prefs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("PUT %s = %d: %s", body, w.Code, w.Body.String())
		}
	}

	put(`{"default_principal_id":"principal_000001","last_harness":"claude_code"}`)
	if got := read(); !strings.Contains(got, `"default_principal_id":"principal_000001"`) {
		t.Fatalf("after set: %s", got)
	}
	// A body without the key leaves it alone.
	put(`{"last_harness":"codex"}`)
	if got := read(); !strings.Contains(got, `"default_principal_id":"principal_000001"`) || !strings.Contains(got, `"last_harness":"codex"`) {
		t.Fatalf("after unrelated write: %s", got)
	}
	// A body with the key empty clears it.
	put(`{"default_principal_id":""}`)
	if got := read(); strings.Contains(got, "default_principal_id") {
		t.Fatalf("after clear: %s", got)
	}
}
