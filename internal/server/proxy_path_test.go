package server

import "testing"

// The endpoint literal is everything AFTER the id segment, not the last segment.
//
// This distinction had no observable consequence until `/messages/raw` — the first
// proxied route with a two-segment endpoint. The previous `path.Base` read "raw",
// proxied to `/api/v1/sessions/{id}/raw`, and 404ed with the route registered and
// present in the binary, which is a confusing way to fail.
func TestLogStoreEndpointFromPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"one-segment endpoint", "/sessions/br_1/messages", "messages"},
		{"the other one-segment endpoint", "/sessions/br_1/history", "history"},
		{"two-segment endpoint", "/sessions/br_1/messages/raw", "messages/raw"},
		// An id holding an encoded slash stays ONE segment, so it cannot shift the
		// split — the whole reason this reads the escaped path.
		{"id with an encoded slash", "/sessions/a%2Fb/messages/raw", "messages/raw"},
		{"not a session route", "/harnesses", ""},
		{"session route with no endpoint", "/sessions/br_1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := logStoreEndpointFromPath(tc.path); got != tc.want {
				t.Errorf("logStoreEndpointFromPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// The full upstream URL for the raw page, end to end — the thing that was wrong.
func TestLogStoreSessionURL_RawPage(t *testing.T) {
	endpoint := logStoreEndpointFromPath("/sessions/br_1/messages/raw")
	got := logStoreSessionURL("http://ls:8175", "br_1", endpoint)
	want := "http://ls:8175/api/v1/sessions/br_1/messages/raw"
	if got != want {
		t.Errorf("upstream URL = %q, want %q", got, want)
	}
}
