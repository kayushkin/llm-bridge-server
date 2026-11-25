package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The scheduler's HTTP edge parses three caller-supplied timestamps with
// time.Parse and hands the parser's error straight back to the caller as the
// `details` field of a 400.
//
// time.Parse quotes its whole input back inside its own message, twice, so a
// 5 KB `until` renders a 10 KB error. The route reads as if the 400 were a small
// structured answer; it is not, and nothing above it bounds the input.
//
// These are ceiling assertions on the rendered line, not greps for a truncate
// call. The first draft of the repair on the sibling card had a truncate in
// place and was still unbounded, and it read as correct — only rendering the
// line and measuring it tells you whether the bound holds.
const (
	// A value long enough that an unbounded echo is unmistakable, and short
	// enough to stay a legal query string.
	oversizedTimestampLength = 5000
	// The ceiling one 400's `details` must stay under. Wide enough for every
	// real time.Parse message ("month out of range", "extra text: ...") and far
	// below anything that could carry a 5 KB echo.
	detailsCeiling = 200
)

func oversizedTimestamp() string { return strings.Repeat("x", oversizedTimestampLength) }

// detailsOf returns just the `details` field, which is the one this claim is
// about. Asserting on the whole response body would be answered by whatever
// else the envelope happens to carry.
func detailsOf(t *testing.T, body []byte) string {
	t.Helper()
	var apiErr APIError
	if err := json.Unmarshal(body, &apiErr); err != nil {
		t.Fatalf("response was not an APIError: %v (body %.200q)", err, body)
	}
	return apiErr.Details
}

func TestAnOversizedUntilIsNotEchoedBackByTheHoldRoute(t *testing.T) {
	srv := setup(t, "tok")

	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo hi"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID any `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created job: %v", err)
	}

	hold, _ := json.Marshal(map[string]string{"until": oversizedTimestamp()})
	req = httptest.NewRequest("POST", "/api/jobs/1/hold", bytes.NewReader(hold))
	req.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unparseable until, got %d: %s", w.Code, w.Body.String())
	}
	details := detailsOf(t, w.Body.Bytes())
	if len(details) > detailsCeiling {
		t.Errorf("a %d-byte until produced a %d-byte details field (ceiling %d)",
			oversizedTimestampLength, len(details), detailsCeiling)
	}
	if details == "" {
		t.Error("details is empty, so the caller is told the value is invalid and not why")
	}
}

func TestAnOversizedFromIsNotEchoedBackByTheCalendarRoute(t *testing.T) {
	srv := setup(t, "tok")

	req := httptest.NewRequest("GET", "/api/calendar/events?from="+url.QueryEscape(oversizedTimestamp()), nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unparseable from, got %d: %s", w.Code, w.Body.String())
	}
	details := detailsOf(t, w.Body.Bytes())
	if len(details) > detailsCeiling {
		t.Errorf("a %d-byte from produced a %d-byte details field (ceiling %d)",
			oversizedTimestampLength, len(details), detailsCeiling)
	}
	if details == "" {
		t.Error("details is empty, so the caller is told the value is invalid and not why")
	}
}

func TestAnOversizedToIsNotEchoedBackByTheCalendarRoute(t *testing.T) {
	srv := setup(t, "tok")

	// `from` is valid so the handler reaches the `to` branch. Without it the
	// from-site would answer for the to-site and this case would pass while
	// testing nothing.
	req := httptest.NewRequest("GET",
		"/api/calendar/events?from=2026-01-01T00:00:00Z&to="+url.QueryEscape(oversizedTimestamp()), nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unparseable to, got %d: %s", w.Code, w.Body.String())
	}
	details := detailsOf(t, w.Body.Bytes())
	if len(details) > detailsCeiling {
		t.Errorf("a %d-byte to produced a %d-byte details field (ceiling %d)",
			oversizedTimestampLength, len(details), detailsCeiling)
	}
	if details == "" {
		t.Error("details is empty, so the caller is told the value is invalid and not why")
	}
}

// TestAnOrdinaryBadTimestampStillSaysWhy is the discriminating negative. A
// repair that blanks `details`, or replaces it with a fixed string, satisfies
// every ceiling above while destroying the only thing the field is for.
func TestAnOrdinaryBadTimestampStillSaysWhy(t *testing.T) {
	srv := setup(t, "tok")

	req := httptest.NewRequest("GET", "/api/calendar/events?from=2026-13-01T00:00:00Z", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	details := detailsOf(t, w.Body.Bytes())
	if !strings.Contains(details, "month out of range") {
		t.Errorf("details %q dropped the parser's diagnosis; a caller cannot tell a bad month from a bad format", details)
	}
}
