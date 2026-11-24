package modelstore

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	_ "modernc.org/sqlite"
)

// awkwardCredential carries the three characters that make concatenating a
// value into a query string wrong. "+" is the one that bites: a receiving
// server decodes a raw "+" in a query string to a space, so the server compares
// against a credential the caller never held. Google API keys and the base64
// pageToken the same endpoint hands back both live in this alphabet, which is
// why a test using ordinary letters passes against the broken code.
const awkwardCredential = "aB+cD/eF=="

// recordedQueries collects what a stand-in upstream actually received, so the
// assertions can run in the test's own goroutine. t.Fatalf is illegal anywhere
// else and would stop failing the test properly.
type recordedQueries struct {
	queries []url.Values
}

func (r *recordedQueries) record(req *http.Request) {
	r.queries = append(r.queries, req.URL.Query())
}

// TestGoogleSyncSendsTheApiKeyUnmangled pins that the credential syncGoogle is
// handed is the credential the upstream reads back out. It drives the real
// syncGoogle rather than a extracted URL builder, so deleting the escaping from
// the call site is what the test catches.
func TestGoogleSyncSendsTheApiKeyUnmangled(t *testing.T) {
	var seen recordedQueries

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[]}`)
	}))
	t.Cleanup(ts.Close)

	googleModelsEndpoint = ts.URL
	t.Cleanup(func() {
		googleModelsEndpoint = "https://generativelanguage.googleapis.com/v1beta/models"
	})

	s := tempStore(t)
	if _, err := s.syncGoogle(awkwardCredential); err != nil {
		t.Fatalf("syncGoogle: %v", err)
	}

	if len(seen.queries) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen.queries))
	}
	q := seen.queries[0]
	if got := q.Get("key"); got != awkwardCredential {
		t.Errorf("api key arrived mangled:\n got %q\nwant %q", got, awkwardCredential)
	}
	if got := q.Get("pageSize"); got != "100" {
		t.Errorf("pageSize = %q, want \"100\"", got)
	}
	if len(q) != 2 {
		t.Errorf("query has %d parameters, want 2 (pageSize, key): %v", len(q), q)
	}
}

// TestGooglePageTokenSurvivesTheSecondRequest pins the value the card did not
// name. A Google nextPageToken is base64, so it carries the same characters the
// api key does — and it is concatenated on the *second* trip round the loop,
// which a single-page fixture never reaches. A mangled token makes the upstream
// reject the page, so pagination stops early and the sync silently imports a
// prefix of the model list.
func TestGooglePageTokenSurvivesTheSecondRequest(t *testing.T) {
	const pageToken = "cGFnZQ+/Zg=="

	var seen recordedQueries

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprintf(w, `{"models":[],"nextPageToken":%q}`, pageToken)
			return
		}
		fmt.Fprint(w, `{"models":[]}`)
	}))
	t.Cleanup(ts.Close)

	googleModelsEndpoint = ts.URL
	t.Cleanup(func() {
		googleModelsEndpoint = "https://generativelanguage.googleapis.com/v1beta/models"
	})

	s := tempStore(t)
	if _, err := s.syncGoogle(awkwardCredential); err != nil {
		t.Fatalf("syncGoogle: %v", err)
	}

	if len(seen.queries) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (the loop must paginate)", len(seen.queries))
	}
	second := seen.queries[1]
	if got := second.Get("pageToken"); got != pageToken {
		t.Errorf("pageToken arrived mangled:\n got %q\nwant %q", got, pageToken)
	}
	if got := second.Get("key"); got != awkwardCredential {
		t.Errorf("api key arrived mangled on the second page:\n got %q\nwant %q", got, awkwardCredential)
	}
	if len(second) != 3 {
		t.Errorf("second query has %d parameters, want 3: %v", len(second), second)
	}
}

// TestAnthropicAfterIdStaysOneQueryParameter covers the same shape in the
// sibling function. after_id is upstream-minted rather than a credential, but
// it is concatenated the same way, and a value carrying "&" would split into a
// second parameter and truncate.
func TestAnthropicAfterIdStaysOneQueryParameter(t *testing.T) {
	const afterID = "model&limit=1"

	var seen recordedQueries

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			fmt.Fprintf(w, `{"data":[],"has_more":true,"last_id":%q}`, afterID)
			return
		}
		fmt.Fprint(w, `{"data":[],"has_more":false}`)
	}))
	t.Cleanup(ts.Close)

	anthropicModelsEndpoint = ts.URL
	t.Cleanup(func() {
		anthropicModelsEndpoint = "https://api.anthropic.com/v1/models"
	})

	s := tempStore(t)
	if _, err := s.syncAnthropic("unused-key"); err != nil {
		t.Fatalf("syncAnthropic: %v", err)
	}

	if len(seen.queries) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (the loop must paginate)", len(seen.queries))
	}
	second := seen.queries[1]
	if got := second.Get("after_id"); got != afterID {
		t.Errorf("after_id split across parameters:\n got %q\nwant %q", got, afterID)
	}
	if got := second.Get("limit"); got != "100" {
		t.Errorf("after_id overwrote limit: limit = %q, want \"100\"", got)
	}
	if len(second) != 2 {
		t.Errorf("second query has %d parameters, want 2: %v", len(second), second)
	}
}
