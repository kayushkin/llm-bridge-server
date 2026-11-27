package mail

import (
	"net/http"
	"testing"
)

// q is passed to a provider that searches itself; a provider that does not
// must say so rather than answer the unfiltered list. The Microsoft account in
// testServer is a Graph backend, which has no search.
func TestAQueryNeedsAProviderThatSearches(t *testing.T) {
	s, _ := testServer(t)
	if w := do(t, s, http.MethodGet, "/api/messages?account=acct-ms&q=from:unr.edu", ""); w.Code != http.StatusNotImplemented {
		t.Errorf("q on a provider with no search = %d, want 501 (%s)", w.Code, w.Body.String())
	}
}
