package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// pathProbeIDs is the id table every caller below is driven with. Each entry
// carries a character that changes which endpoint the request addresses if it
// reaches the path unescaped.
var pathProbeIDs = []struct {
	name string
	id   string
}{
	{"well formed", "ses_01HXYZ"},
	{"fragment", "ses#frag"},
	{"query", "ses?replay=1"},
	{"extra segment", "a/b"},
	{"climbs out of the collection", "../sessions"},
	{"space", "ses one"},
	{"already percent encoded", "ses%2Fb"},
}

// TestASessionIDStaysOnePathSegment pins that the session id llm-bridge-server
// minted in its create answer occupies exactly one path segment when this CLI
// aims the follow-up calls at it.
//
// It asserts r.RequestURI, NOT r.URL.Path. Go's server has already decoded
// %2F back to a slash by the time it fills URL.Path, so a URL.Path assertion
// reads identically whether or not the client escaped anything — it cannot
// hold this property at all.
func TestASessionIDStaysOnePathSegment(t *testing.T) {
	callers := []struct {
		name string
		verb string
		call func(d *delegate, id string)
	}{
		{"send", "/send", func(d *delegate, id string) {
			_ = d.send(context.Background(), id, "hello")
		}},
		{"stop", "/stop", func(d *delegate, id string) {
			d.stop(id)
		}},
		{"subscribe", "/events", func(d *delegate, id string) {
			_, closeStream, err := d.subscribe(context.Background(), id)
			if err == nil {
				closeStream()
			}
		}},
	}

	for _, caller := range callers {
		for _, probe := range pathProbeIDs {
			t.Run(caller.name+"/"+probe.name, func(t *testing.T) {
				var got string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got = r.RequestURI
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				defer srv.Close()

				d := &delegate{client: srv.Client(), server: srv.URL}
				caller.call(d, probe.id)

				want := "/sessions/" + url.PathEscape(probe.id) + caller.verb
				if got != want {
					t.Errorf("id %q addressed %q, want %q", probe.id, got, want)
				}
			})
		}
	}
}
