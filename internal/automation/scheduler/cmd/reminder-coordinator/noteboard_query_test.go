package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// A noteboard tag is user data. "personal" is the common case, but nothing stops
// a tag, a status filter or a parent id from carrying a character that is
// structural inside a query string. Every character in these probes is LEGAL in
// a URL, so an unescaped request is sent successfully and mangled silently —
// which is the failure worth pinning. A probe with a space would redden this
// pin too, but only because net/http refuses to send it.
const (
	probeTag      = "home&status=done"
	probeItemType = "todo&limit=1"
	probeParentID = "id&limit=1"
)

// recordRequestURI answers with an empty item list and keeps the raw request
// line. RequestURI is the only field that survives untouched: URL.Path and a
// re-encoded URL.String() are rebuilt from a parsed value, so they read the
// same whether or not the client escaped anything.
func recordRequestURI(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

// assertParametersAreExactly is the property: the server must read back the
// values the caller passed, and the parameter set must be exactly what the
// caller intended. A value carrying "&" violates the second half by adding a
// parameter nobody asked for — which, on this endpoint, silently changes which
// items come back.
func assertParametersAreExactly(t *testing.T, requestURI string, want map[string]string) {
	t.Helper()
	parsed, err := url.ParseRequestURI(requestURI)
	if err != nil {
		t.Fatalf("server could not parse the request line %q: %v", requestURI, err)
	}
	values := parsed.Query()
	for name, expected := range want {
		if got := values.Get(name); got != expected {
			t.Errorf("parameter %q round-tripped as %q, want %q\n  request line: %s",
				name, got, expected, requestURI)
		}
	}
	for name := range values {
		if _, intended := want[name]; !intended {
			t.Errorf("parameter %q appeared and the caller never set it — a value carried a separator\n  request line: %s",
				name, requestURI)
		}
	}
}

func TestListOpenSendsTheTagItWasGiven(t *testing.T) {
	server, seen := recordRequestURI(t)
	client := &NoteboardClient{BaseURL: server.URL}

	if _, err := client.ListOpen(probeTag); err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	assertParametersAreExactly(t, *seen, map[string]string{
		"type": "todo", "status": "open", "tag": probeTag, "limit": "500", "offset": "0",
	})
}

func TestListAllStatusesSendsTheTagItWasGiven(t *testing.T) {
	server, seen := recordRequestURI(t)
	client := &NoteboardClient{BaseURL: server.URL}

	if _, err := client.ListAllStatuses(probeTag); err != nil {
		t.Fatalf("ListAllStatuses: %v", err)
	}
	// The loop's last status is "archived"; the tag is the value under test.
	assertParametersAreExactly(t, *seen, map[string]string{
		"type": "todo", "status": "archived", "tag": probeTag, "limit": "500", "offset": "0",
	})
}

func TestListChildrenSendsTheParentIDItWasGiven(t *testing.T) {
	server, seen := recordRequestURI(t)
	client := &NoteboardClient{BaseURL: server.URL}

	if _, err := client.ListChildren(probeParentID); err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	assertParametersAreExactly(t, *seen, map[string]string{
		"parent_id": probeParentID, "limit": "500", "offset": "0",
	})
}

func TestFindByTagSendsTheTypeAndTagItWasGiven(t *testing.T) {
	server, seen := recordRequestURI(t)
	client := &NoteboardClient{BaseURL: server.URL}

	if _, err := client.FindByTag(probeItemType, probeTag); err != nil {
		t.Fatalf("FindByTag: %v", err)
	}
	assertParametersAreExactly(t, *seen, map[string]string{
		"type": probeItemType, "tag": probeTag, "limit": "10",
	})
}
