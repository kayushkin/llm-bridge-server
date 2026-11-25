package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/kayushkin/noteboard/model"
)

// A noteboard item id reaches Patch and SetSchedule off a list answer: noteboard
// assigns the id and owns it, so the coordinator neither chooses it nor constrains
// its shape.
//
// Every character in these probes is LEGAL in a URL, so an unescaped id is sent
// successfully and re-points the request silently — which is the failure worth
// pinning.
//
// ⚠️ This is a WRITE, and that is what separates it from the read sites on the same
// card. A re-pointed PATCH does not return the wrong answer, it EDITS THE WRONG ROW
// and answers 200 — on a service that ~/CLAUDE.md records as having no auth at all.
// And an UPDATE is the one noteboard operation that can still destroy data: its
// revision snapshot covers 8 of the 20 item columns and not `rank`, `due_at` or
// `schedule`, which are the fields SetSchedule writes through this very path.
const (
	// "/" opens a new path segment: the PATCH lands on a different noteboard route.
	probeItemIDWithSlash = "item-1/../lists"
	// "?" ends the path: everything after it becomes a query the caller never set.
	probeItemIDWithQuery = "item-1?type=note"
	// "#" ends the path too, and the remainder is never even sent — so the call
	// edits item "item-1" and reports success. The quietest of the three.
	probeItemIDWithFragment = "item-1#gone"
	// Already-encoded. A repair that skips a second escape sends "%2F" on the wire,
	// which noteboard's router decodes back to a slash.
	probeItemIDPreEncoded = "item%2F1"
	// A space is a WRONG-REPAIR control and nothing else, exactly as
	// calendar_path_test.go records for the sibling helper: net/url escapes a raw
	// space to "%20" on its own, so this probe round-trips correctly even unescaped
	// and cannot witness the defect. What it separates is the two escape functions —
	// PathEscape writes "%20", QueryEscape writes "+", a literal plus in a path.
	probeItemIDWithSpace = "item 1"
)

var itemPathProbes = []string{
	probeItemIDWithSlash,
	probeItemIDWithQuery,
	probeItemIDWithFragment,
	probeItemIDPreEncoded,
	probeItemIDWithSpace,
}

// assertItemIDIsOneSegment is the property: whatever the id contains, the request
// must address /api/items/<id> — the id occupying exactly one path segment, with no
// query and no fragment the caller did not set.
//
// It reads the ESCAPED path and never url.URL.Path, for the reason
// assertEventIDIsOneSegment gives: Path is decoded, so "%2F" and a raw "/" are the
// same string by the time it is populated, and an assertion on it cannot tell an
// escaped id from an unescaped one in either direction.
func assertItemIDIsOneSegment(t *testing.T, requestLine, itemID string) {
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
	const prefix = "/api/items/"
	escaped := parsed.EscapedPath()
	if !strings.HasPrefix(escaped, prefix) {
		t.Fatalf("request addressed %q, want a path under %q\n  request line: %s",
			escaped, prefix, requestLine)
	}
	segment := strings.TrimPrefix(escaped, prefix)
	if strings.Contains(segment, "/") {
		t.Errorf("the id opened %d extra path segment(s): the request addresses %q, not the item\n  request line: %s",
			strings.Count(segment, "/"), escaped, requestLine)
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		t.Fatalf("the id segment %q is not valid escaping: %v\n  request line: %s", segment, err, requestLine)
	}
	if decoded != itemID {
		t.Errorf("item id round-tripped as %q, want %q\n  request line: %s", decoded, itemID, requestLine)
	}
}

func TestPatchKeepsAnItemIDInOnePathSegment(t *testing.T) {
	for _, itemID := range itemPathProbes {
		t.Run(itemID, func(t *testing.T) {
			server, seen := recordRequestLine(t)
			client := &NoteboardClient{BaseURL: server.URL}

			if err := client.Patch(itemID, map[string]any{"status": "done"}); err != nil {
				t.Fatalf("Patch: %v", err)
			}
			assertItemIDIsOneSegment(t, *seen, itemID)
		})
	}
}

// SetSchedule is the caller that makes this sharpest: it writes `schedule`, which
// noteboard's revision snapshot does NOT cover, so a PATCH that lands on the wrong
// row overwrites a recurrence rule that no revision holds.
func TestSetScheduleKeepsAnItemIDInOnePathSegment(t *testing.T) {
	for _, itemID := range itemPathProbes {
		t.Run(itemID, func(t *testing.T) {
			server, seen := recordRequestLine(t)
			client := &NoteboardClient{BaseURL: server.URL}

			schedule := &model.Schedule{RRule: "FREQ=DAILY", TZID: "America/Los_Angeles"}
			if err := client.SetSchedule(itemID, schedule, false); err != nil {
				t.Fatalf("SetSchedule: %v", err)
			}
			assertItemIDIsOneSegment(t, *seen, itemID)
		})
	}
}

// An ordinary id must still reach the wire unchanged. Without this, escaping every
// id into something noteboard cannot resolve would satisfy the pins above.
func TestAnOrdinaryItemIDReachesTheWireUnchanged(t *testing.T) {
	const ordinary = "018f2c1a-4d3b-7e91-a0c5-2b6f8d19e4aa"

	server, seen := recordRequestLine(t)
	client := &NoteboardClient{BaseURL: server.URL}

	if err := client.Patch(ordinary, map[string]any{"status": "done"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if want := "/api/items/" + ordinary; *seen != want {
		t.Errorf("request line %q, want %q", *seen, want)
	}
}
