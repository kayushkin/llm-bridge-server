package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
)

// A Google event id reaches UpdateEvent and DeleteEvent from splitCalendarLinks,
// which trims a prefix off an entry in a noteboard item's `links` array. noteboard
// puts no constraint on the strings in that array, and this same file PATCHes it a
// few lines away, so the value is caller-written free text.
//
// Every character in these probes is LEGAL in a URL, so an unescaped id is sent
// successfully and re-points the request silently — which is the failure worth
// pinning. A probe with a space would redden this pin too, but only because
// net/http refuses to send it, which would prove nothing about escaping.
//
// The requests these ids ride on carry `Authorization: Bearer <DASH_TOKEN>`, and
// dash proxies far more than the calendar, so a re-pointed path is an authenticated
// call to a route the coordinator never meant to reach.
const (
	// "/" opens a new path segment: the DELETE lands on a different dash route.
	probeEventIDWithSlash = "evt123/../../bridge/instances"
	// "?" ends the path: everything after it becomes a query the caller never set.
	probeEventIDWithQuery = "evt123?calendarId=primary"
	// "#" ends the path too, and the remainder is never even sent.
	probeEventIDWithFragment = "evt123#gone"
	// A space is a WRONG-REPAIR control and nothing else. Measured: net/url escapes
	// a raw space to "%20" on its own, so this probe round-trips correctly even
	// unescaped and cannot witness the defect — the three probes above do that.
	// What it separates is the two escape functions. PathEscape writes "%20", which
	// decodes back to a space; QueryEscape writes "+", which in a PATH segment is a
	// literal plus, not a space. So a repair reaching for QueryEscape — the function
	// the query-string sites a few hundred lines up correctly use — reddens here and
	// nowhere else.
	probeEventIDWithSpace = "evt 123"
)

// pathProbes are the ids driven through both write methods.
var pathProbes = []string{
	probeEventIDWithSlash,
	probeEventIDWithQuery,
	probeEventIDWithFragment,
	probeEventIDWithSpace,
}

// recordRequestLine answers 200 with an empty body and keeps the raw request line.
// RequestURI is the only field that survives untouched: r.URL.Path is a decoded,
// re-parsed value and reads the same whether or not the client escaped anything,
// so asserting on it would pass against the defect.
func recordRequestLine(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

// assertEventIDIsOneSegment is the property: whatever the id contains, the request
// must address /api/calendar/google/<id> — the id occupying exactly one path
// segment, with no query and no fragment that the caller did not set.
//
// It reads the ESCAPED path and never url.URL.Path. Path is decoded, so "%2F" and
// a raw "/" are the same string by the time it is populated: an assertion on Path
// cannot tell an escaped id from an unescaped one in either direction, and would
// keep failing after a correct repair. Split on the escaped form, then unescape the
// one segment to check the id survived intact.
func assertEventIDIsOneSegment(t *testing.T, requestLine, eventID string) {
	t.Helper()
	if requestLine == "" {
		t.Fatalf("the recording server was never reached, so nothing was measured")
	}
	parsed, err := url.ParseRequestURI(requestLine)
	if err != nil {
		t.Fatalf("server could not parse the request line %q: %v", requestLine, err)
	}
	if parsed.RawQuery != "" {
		t.Errorf("the id carried a separator and added the query %q, which no caller set\n  request line: %s",
			parsed.RawQuery, requestLine)
	}
	const prefix = "/api/calendar/google/"
	escaped := parsed.EscapedPath()
	if !strings.HasPrefix(escaped, prefix) {
		t.Fatalf("request addressed %q, want a path under %q\n  request line: %s",
			escaped, prefix, requestLine)
	}
	segment := strings.TrimPrefix(escaped, prefix)
	if strings.Contains(segment, "/") {
		t.Errorf("the id opened %d extra path segment(s): the request addresses %q, not the event\n  request line: %s",
			strings.Count(segment, "/"), escaped, requestLine)
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		t.Fatalf("the id segment %q is not valid escaping: %v\n  request line: %s", segment, err, requestLine)
	}
	if decoded != eventID {
		t.Errorf("event id round-tripped as %q, want %q\n  request line: %s", decoded, eventID, requestLine)
	}
}

func probeItem() *model.Item {
	due := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	return &model.Item{ID: "item-1", Title: "Water the plants", DueAt: &due}
}

func TestDeleteEventKeepsAnEventIDInOnePathSegment(t *testing.T) {
	for _, eventID := range pathProbes {
		t.Run(eventID, func(t *testing.T) {
			server, seen := recordRequestLine(t)
			mirror := &GoogleCalendarMirror{DashURL: server.URL}

			if err := mirror.DeleteEvent("token", eventID); err != nil {
				t.Fatalf("DeleteEvent: %v", err)
			}
			assertEventIDIsOneSegment(t, *seen, eventID)
		})
	}
}

func TestUpdateEventKeepsAnEventIDInOnePathSegment(t *testing.T) {
	for _, eventID := range pathProbes {
		t.Run(eventID, func(t *testing.T) {
			server, seen := recordRequestLine(t)
			mirror := &GoogleCalendarMirror{DashURL: server.URL}

			if err := mirror.UpdateEvent("token", eventID, probeItem()); err != nil {
				t.Fatalf("UpdateEvent: %v", err)
			}
			assertEventIDIsOneSegment(t, *seen, eventID)
		})
	}
}

// An ordinary id must still reach the wire unchanged. Without this, escaping every
// id into something the calendar cannot resolve would satisfy the pins above.
func TestAnOrdinaryEventIDReachesTheWireUnchanged(t *testing.T) {
	const ordinary = "6f8k2m4p1q9r3s5t7u0v2w4x6y"

	server, seen := recordRequestLine(t)
	mirror := &GoogleCalendarMirror{DashURL: server.URL}

	if err := mirror.DeleteEvent("token", ordinary); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if want := "/api/calendar/google/" + ordinary; *seen != want {
		t.Errorf("request line %q, want %q", *seen, want)
	}
}
