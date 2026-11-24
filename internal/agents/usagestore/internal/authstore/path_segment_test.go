package authstore

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// An auth-store credential id is one path segment. Nothing in this client makes
// it one.
//
// ResolveByID pastes the id straight into a URL with fmt.Sprintf. The id is not
// this client's own value: both call sites in cmd/usage-store-server take it
// from auth-store's own GET /api/credentials answer (cred.ID), and nothing on
// this side checks it. An id carrying "/", "?" or "#" does not produce an
// error; it produces a request to a different endpoint, and this client attaches
// its bearer token and its X-Auth-App / X-Auth-Reason audit headers to whatever
// that turns out to be.
//
// The assertions check the property rather than comparing against
// url.PathEscape's output: a test computing its expectation with the same call
// as the code shares the code's opinion of what escaping means and cannot fail
// when that opinion is wrong. What is asserted is that the path is exactly the
// segments this client meant to address, that the id segment decodes back to
// what the caller passed, and that the request carries no query at all.

var probeIDs = []struct {
	name string
	id   string
}{
	{"plain", "cred-a"},
	{"extra segment", "cred-a/resolve"},
	{"traversal", "../credentials/cred-b"},
	{"query", "cred-a?reveal=1"},
	{"fragment", "cred-a#frag"},
	{"space", "cred a"},
	{"already encoded", "cred-a%2Fresolve"},
	{"ampersand", "cred-a&provider=x"},
}

type recorder struct {
	*httptest.Server
	uris []string
}

func newRecorder(t *testing.T, body string) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI is the raw bytes off the wire. r.URL.Path has already been
		// unescaped, so it cannot tell "%2F" from "/" — the difference under test.
		rec.uris = append(rec.uris, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(rec.Server.Close)
	return rec
}

// assertCredentialPath states the contract once. This client's URLs carry no
// query, so any query at all is the id having escaped its segment.
func assertCredentialPath(t *testing.T, requestURI, wantID string) {
	t.Helper()

	if i := strings.IndexByte(requestURI, '#'); i >= 0 {
		t.Errorf("request URI %q carries a fragment: everything after it never reached the server", requestURI)
		return
	}

	path, query := requestURI, ""
	if i := strings.IndexByte(requestURI, '?'); i >= 0 {
		path, query = requestURI[:i], requestURI[i+1:]
	}
	if query != "" {
		t.Errorf("request URI %q has query %q: this client sends none, so the id leaked out of its segment",
			requestURI, query)
	}

	parts := strings.Split(path, "/")
	want := []string{"", "api", "credentials", "<id>", "resolve"}
	if len(parts) != len(want) {
		t.Errorf("request path %q has %d segments, want %d (/api/credentials/<id>/resolve): the id was not one segment",
			path, len(parts), len(want))
		return
	}
	for i, w := range want {
		if w == "<id>" {
			continue
		}
		if parts[i] != w {
			t.Errorf("request path %q: segment %d is %q, want %q", path, i, parts[i], w)
		}
	}

	got, err := url.PathUnescape(parts[3])
	if err != nil {
		t.Errorf("id segment %q of %q does not decode: %v", parts[3], path, err)
		return
	}
	if got != wantID {
		t.Errorf("id segment %q of %q decodes to %q, want %q", parts[3], path, got, wantID)
	}
}

func TestResolveByIDSendsTheIDAsOnePathSegment(t *testing.T) {
	for _, p := range probeIDs {
		t.Run(p.name, func(t *testing.T) {
			rec := newRecorder(t, `{"id":"cred-a","api_key":"sk-test","auth_type":"api_key"}`)
			if _, err := New(rec.URL, "tok", "usage-store").ResolveByID(p.id, "test"); err != nil {
				t.Fatalf("ResolveByID: %v", err)
			}
			if len(rec.uris) != 1 {
				t.Fatalf("server saw %d requests, want 1", len(rec.uris))
			}
			assertCredentialPath(t, rec.uris[0], p.id)
		})
	}
}

// TestResolveByIDStillCarriesItsAuditHeaders pins what the escape must not cost.
// auth-store rejects key-touching routes without these three, so a repair that
// rebuilt the request rather than the path would turn every resolve into a 4xx
// and this is the only thing that would say so.
func TestResolveByIDStillCarriesItsAuditHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"id":"cred-a","api_key":"sk-test"}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := New(srv.URL, "tok", "usage-store").ResolveByID("cred-a/x", "discover:admin-key"); err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	for h, want := range map[string]string{
		"Authorization": "Bearer tok",
		"X-Auth-App":    "usage-store",
		"X-Auth-Reason": "discover:admin-key",
	} {
		if got.Get(h) != want {
			t.Errorf("header %s = %q, want %q", h, got.Get(h), want)
		}
	}
}

// TestListCredentialsAddressesTheCollectionNotACredential is the negative
// control. ListCredentials interpolates nothing — its path is a constant suffix
// on BaseURL — so it must be untouched by any repair made here. A fix that
// reached the base URL breaks this and nothing else.
func TestListCredentialsAddressesTheCollectionNotACredential(t *testing.T) {
	rec := newRecorder(t, `[]`)
	if _, err := New(rec.URL, "tok", "usage-store").ListCredentials(); err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if got := rec.uris[0]; got != "/api/credentials" {
		t.Errorf("ListCredentials addressed %q, want %q", got, "/api/credentials")
	}
}
