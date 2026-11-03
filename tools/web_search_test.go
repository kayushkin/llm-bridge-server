package tools

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// web_search asked Brave for gzip and never decoded it. net/http decompresses
// transparently ONLY when its own transport added Accept-Encoding; setting the
// header by hand hands the caller a still-compressed body. Measured against the
// live Brave API on 2026-08-14: Brave honors the request and gzips, so every
// search this tool ever ran returned "error parsing response: invalid character
// '\x1f'". These tests pin the fix by driving a server that gzips only when the
// client asks — the fix and the bug send byte-identical requests, so the defect
// is observable only in whether the tool can READ what comes back.

const braveResultsJSON = `{"web":{"results":[
	{"title":"First hit","url":"https://example.com/one","description":"one"},
	{"title":"Second hit","url":"https://example.com/two","description":"two"}
]}}`

// braveServer answers with body, gzipping it when the client's Accept-Encoding
// says gzip. A client that asks and cannot decode gets the compressed bytes,
// which is exactly the production defect.
func braveServer(t *testing.T, status int, body string, sawRequest func(*http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sawRequest != nil {
			sawRequest(r)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(status)
			zw := gzip.NewWriter(w)
			if _, err := zw.Write([]byte(body)); err != nil {
				t.Errorf("gzip write: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Errorf("gzip close: %v", err)
			}
			return
		}
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The load-bearing case. It fails the moment anyone re-adds the hand-set
// Accept-Encoding header, because the server will gzip and the transport will
// then decline to decode.
func TestSearchReadsAGzippedResponse(t *testing.T) {
	srv := braveServer(t, 200, braveResultsJSON, nil)

	got := searchBrave(context.Background(), srv.URL, "key", "anything", 5, "US")

	if strings.Contains(got, "error") {
		t.Fatalf("gzipped response was not decoded, tool returned: %q", got)
	}
	for _, want := range []string{"First hit", "https://example.com/one", "Second hit"} {
		if !strings.Contains(got, want) {
			t.Errorf("result %q missing from output: %q", want, got)
		}
	}
}

// A control that must stay green whether or not the header is set: an
// uncompressed response parses either way. It is here so the gzip case above
// is known to be the one carrying the assertion, rather than the suite simply
// reddening at any edit.
func TestSearchReadsAnUncompressedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(braveResultsJSON)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	got := searchBrave(context.Background(), srv.URL, "key", "anything", 5, "US")

	if !strings.Contains(got, "First hit") {
		t.Fatalf("uncompressed response did not parse: %q", got)
	}
}

// The status path builds its message from the raw body. A gzipped error body
// would put the gzip magic number into text handed to the model, so the error
// surface needs the same decoding the success path does.
func TestSearchReportsAGzippedErrorBodyAsReadableText(t *testing.T) {
	srv := braveServer(t, 429, `{"error":"rate limited"}`, nil)

	got := searchBrave(context.Background(), srv.URL, "key", "anything", 5, "US")

	if !strings.Contains(got, "429") {
		t.Errorf("status code missing from error: %q", got)
	}
	if !strings.Contains(got, "rate limited") {
		t.Errorf("error body not readable, got: %q", got)
	}
	if strings.Contains(got, "\x1f\x8b") {
		t.Errorf("error message carries raw gzip bytes: %q", got)
	}
}

// A 202 is not a 200. The check must not be loosened to "any 2xx" — Brave
// returning an accepted-but-empty body should not read as a successful search.
func TestSearchTreatsNon200SuccessAsAnError(t *testing.T) {
	srv := braveServer(t, 202, `{"web":{"results":[]}}`, nil)

	got := searchBrave(context.Background(), srv.URL, "key", "anything", 5, "US")

	if !strings.Contains(got, "202") {
		t.Errorf("202 not reported as an error: %q", got)
	}
}

func TestSearchReportsAnEmptyResultSet(t *testing.T) {
	srv := braveServer(t, 200, `{"web":{"results":[]}}`, nil)

	got := searchBrave(context.Background(), srv.URL, "key", "anything", 5, "US")

	if got != "no results found" {
		t.Errorf("want %q, got %q", "no results found", got)
	}
}

// The query the caller asked for, the clamped count and the country all have to
// reach the wire; a default applied and then not sent is the same as no default.
func TestSearchSendsTheQueryParametersAndTheAPIKey(t *testing.T) {
	var got *http.Request
	srv := braveServer(t, 200, braveResultsJSON, func(r *http.Request) { got = r })

	searchBrave(context.Background(), srv.URL, "secret-key", "go gzip", 3, "GB")

	if got == nil {
		t.Fatal("server saw no request")
	}
	if got.URL.Path != "/res/v1/web/search" {
		t.Errorf("path = %q", got.URL.Path)
	}
	for key, want := range map[string]string{"q": "go gzip", "count": "3", "country": "GB"} {
		if of := got.URL.Query().Get(key); of != want {
			t.Errorf("query %s = %q, want %q", key, of, want)
		}
	}
	if got.Header.Get("X-Subscription-Token") != "secret-key" {
		t.Errorf("api key header = %q", got.Header.Get("X-Subscription-Token"))
	}
}

// A canceled context must stop the call rather than be ignored.
func TestSearchHonorsACanceledContext(t *testing.T) {
	srv := braveServer(t, 200, braveResultsJSON, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := searchBrave(ctx, srv.URL, "key", "anything", 5, "US")

	if !strings.Contains(got, "error") {
		t.Errorf("canceled context did not surface an error: %q", got)
	}
}

// The wanted values below are written as literals on purpose. Naming the
// constant the code reads makes the assertion move with the code: a sabotage
// run that changed defaultResultCount to 0 and defaultCountry to "" scored
// UNNOTICED against an earlier draft of this table that used the constants.
// The numbers here are the ones web_search's InputSchema promises the caller
// ("1-10, default 5", "default US"), so this table is what pins the schema's
// text to the behavior.
func TestSearchParameterDefaults(t *testing.T) {
	cases := []struct {
		name        string
		count       int
		country     string
		wantCount   int
		wantCountry string
	}{
		{"unset count defaults to 5", 0, "US", 5, "US"},
		{"negative count defaults to 5", -3, "US", 5, "US"},
		{"count above the advertised max clamps to 10", 99, "US", 10, "US"},
		{"count at the max is kept", 10, "US", 10, "US"},
		{"count in range is kept", 1, "US", 1, "US"},
		{"empty country defaults to US", 5, "", 5, "US"},
		{"country is kept", 5, "GB", 5, "GB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			count, country := applySearchParameterDefaults(c.count, c.country)
			if count != c.wantCount {
				t.Errorf("count = %d, want %d", count, c.wantCount)
			}
			if country != c.wantCountry {
				t.Errorf("country = %q, want %q", country, c.wantCountry)
			}
		})
	}
}
