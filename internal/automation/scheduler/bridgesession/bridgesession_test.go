package bridgesession

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bridgeShapedMux routes the way llm-bridge-server routes: Go 1.22 patterns,
// one wildcard segment for the id, and the sibling sub-resource routes that an
// id carrying a slash would otherwise reach. Requests are recorded so a test can
// assert which route answered and what the router handed it as the id.
//
// This exists because a string comparison against an expected URL cannot tell
// you whether the server would agree. Escaping is only correct if the router
// hands the id back whole, and the router is the only thing that knows.
type routed struct {
	route string
	id    string
	sub   string
}

func bridgeShapedMux(t *testing.T, seen *[]routed) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, routed{route: "GET /sessions"})
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, routed{route: "GET /sessions/{id}", id: r.PathValue("id")})
		w.Write([]byte(`{"state":"idle"}`))
	})
	mux.HandleFunc("GET /sessions/{id}/{sub}", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, routed{route: "GET /sessions/{id}/{sub}", id: r.PathValue("id"), sub: r.PathValue("sub")})
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, routed{route: "fallthrough " + r.URL.Path})
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The property that matters: whatever the id contains, the request reaches
// GET /sessions/{id} and the router hands back the id that went in. A test that
// only compared the built string could not see the second half of that.
func TestEveryIDShapeReachesTheSessionRouteWithTheIDIntact(t *testing.T) {
	ids := []string{
		"br_1777573132534000506",               // the common shape, 1698 of the live links
		"329a6724-1d8f-47e5-9ba6-0a9329459deb", // a UUID, the other common shape
		"autoworker-anthropic-1778361000103970501",
		"a/b",          // would reach a sibling route unescaped
		"a/send",       // names a real sibling route
		"a?state=idle", // would cut the id short unescaped and answer about "a"
		"a#b",
		"a b",   // the space that tells PathEscape from QueryEscape
		"a+b",   // the plus that tells them apart the other way
		"a%2Fb", // an id that already looks escaped must not be double-decoded
		"...",   // three dots is an ordinary segment, unlike one or two
		"a..b",
		"$&+:=@~", // every reserved byte PathEscape leaves alone
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			var seen []routed
			srv := bridgeShapedMux(t, &seen)

			u, err := URL(srv.URL, id)
			if err != nil {
				t.Fatalf("URL(%q) refused a routable id: %v", id, err)
			}
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET %s: %v", u, err)
			}
			resp.Body.Close()

			if len(seen) != 1 {
				t.Fatalf("id %q: expected exactly one request, got %d: %+v", id, len(seen), seen)
			}
			if seen[0].route != "GET /sessions/{id}" {
				t.Fatalf("id %q addressed %q, not the session route — URL was %s", id, seen[0].route, u)
			}
			if seen[0].id != id {
				t.Fatalf("id %q came back from the router as %q — URL was %s", id, seen[0].id, u)
			}
		})
	}
}

// PathEscape and QueryEscape agree on a slash and disagree on a space and a
// plus. An escaping test whose only awkward input is a slash cannot tell the two
// apart, so swapping one for the other stays green — and QueryEscape is wrong
// here: inside a path segment its "+" is a literal plus addressing another id.
func TestTheEscaperIsThePathEscaperAndNotTheQueryEscaper(t *testing.T) {
	u, err := URL("http://bridge", "a b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "/sessions/a%20b") {
		t.Fatalf("a space must become %%20 in a path segment, got %s", u)
	}
	if strings.Contains(u, "+") {
		t.Fatalf("a space became a plus, which is the query escaper: %s", u)
	}

	u, err = URL("http://bridge", "a+b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "/sessions/a+b") {
		t.Fatalf("a plus is an ordinary path byte and must survive, got %s", u)
	}
}

// The refusals. These two ids cannot be escaped into a segment that names a
// session, because a router resolves dot-segments before it matches a route.
// Returning a URL for them would hand the caller a request that quietly asks a
// different question.
func TestDotSegmentsAndTheEmptyIDAreRefusedRatherThanEscaped(t *testing.T) {
	for _, id := range []string{"", ".", ".."} {
		if u, err := URL("http://bridge", id); err == nil {
			t.Fatalf("id %q was accepted and built %s; no escaping makes it name a session", id, u)
		}
		if u, err := SubresourceURL("http://bridge", id, "messages"); err == nil {
			t.Fatalf("id %q was accepted for a sub-resource and built %s", id, u)
		}
	}
}

// The refusal has to be worth more than the bug it replaces: ".." unescaped
// redirects to "/", which 404s, and a 404 is exactly what frees a card. So the
// test asserts no request is made at all, not merely that an error comes back.
func TestARefusedIDNeverReachesTheBridge(t *testing.T) {
	var seen []routed
	srv := bridgeShapedMux(t, &seen)

	if _, err := URL(srv.URL, ".."); err == nil {
		t.Fatal("`..` must be refused")
	}
	if len(seen) != 0 {
		t.Fatalf("a refused id still reached the bridge: %+v", seen)
	}
}

func TestSubresourceURLEscapesTheIDAndNotTheRouteItNames(t *testing.T) {
	var seen []routed
	srv := bridgeShapedMux(t, &seen)

	u, err := SubresourceURL(srv.URL, "a/b", "messages")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(seen) != 1 || seen[0].route != "GET /sessions/{id}/{sub}" {
		t.Fatalf("expected the sub-resource route, got %+v", seen)
	}
	if seen[0].id != "a/b" {
		t.Fatalf("id came back as %q, not %q", seen[0].id, "a/b")
	}
	if seen[0].sub != "messages" {
		t.Fatalf("sub-resource came back as %q — the route name must not be escaped", seen[0].sub)
	}
}

func TestTrailingSlashesOnTheBaseURLDoNotDoubleUp(t *testing.T) {
	u, err := URL("http://bridge/", "br_1")
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://bridge/sessions/br_1" {
		t.Fatalf("got %s", u)
	}
}
