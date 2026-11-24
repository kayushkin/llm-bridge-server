package noteboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Written from noteboard's own source (internal/api/api.go,
// expectedUpdatedAtFromIfMatch and the PATCH branch of itemByID) and measured
// against the live service on 2026-09-18: If-Match is the quoted updated_at; a
// stored version that differs is 412 {"error":…,"current":{…}} with an ETag;
// no header applies the patch whatever the version.
func TestPatchItemIfMatchCarriesTheHeaderAndTypesThe412(t *testing.T) {
	const version = `"2026-09-18T20:01:13.903591254Z"`
	const refusal = `{"error":"item abc was changed","current":{"id":"abc","title":"theirs"}}`
	var sentIfMatch []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentIfMatch = r.Header.Values("If-Match")
		if len(sentIfMatch) == 0 {
			w.Write([]byte(`{"id":"abc"}`))
			return
		}
		w.Header().Set("ETag", `"newer"`)
		w.WriteHeader(http.StatusPreconditionFailed)
		w.Write([]byte(refusal))
	}))
	defer srv.Close()
	c := New(srv.URL)

	_, err := c.PatchItemIfMatch("abc", map[string]any{"title": "mine"}, version)
	if len(sentIfMatch) != 1 || sentIfMatch[0] != version {
		t.Fatalf("If-Match on the wire = %q, want the caller's %s unchanged", sentIfMatch, version)
	}
	var changed *ItemChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("a 412 came back as %T (%v), want *ItemChangedError so the API can relay it rather than report a 502", err, err)
	}
	if string(changed.Body) != refusal || changed.ETag != `"newer"` {
		t.Errorf("relayed body %s and ETag %s, want noteboard's own, unchanged", changed.Body, changed.ETag)
	}

	// No version asked for: no header at all, not an empty one. noteboard
	// reads an absent header as "apply whatever the version".
	if _, err := c.PatchItem("abc", map[string]any{"title": "mine"}); err != nil {
		t.Fatalf("PatchItem: %v", err)
	}
	if len(sentIfMatch) != 0 {
		t.Errorf("PatchItem sent If-Match %q; every caller that predates the header must send none", sentIfMatch)
	}
}
