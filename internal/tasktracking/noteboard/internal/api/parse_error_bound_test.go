package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kayushkin/noteboard/model"
)

// noteboard hands a caller's own input back inside its 400s, and this service
// has no auth: ~/CLAUDE.md records it on *:8191 with no firewall rule. So the
// length of a response body is the caller's to choose, which is what these
// cases deny.
//
// Every ceiling below was measured against the DEPLOYED binary before the
// repair, not inferred from reading the source. Three shapes, and the middle
// one is the reason a ceiling has to be asserted on the rendered field rather
// than checked by grepping for a truncate call:
//
//	5 034 – 5 114 bytes  the statement quotes the value with %q
//	10 114 – 10 116      time.Parse quotes its whole input back, twice
//	15 183               both at once: the value AND a wrapped parser error
//
// At the 15 KB site a 60-byte cut on the value alone leaves 10 KB and reads
// exactly like a bound that holds.
const (
	// Long enough that an unbounded echo is unmistakable, short enough to stay
	// a legal query string.
	oversizedLength = 5000

	// The ceiling one rendered `error` must stay under. Wide enough for every
	// real diagnosis these parsers emit, far below anything carrying a 5 KB
	// echo, and — deliberately — far below 5000 so a single surviving copy of
	// the value fails rather than squeaks under.
	errorFieldCeiling = 300
)

func oversized() string { return strings.Repeat("x", oversizedLength) }

// errorFieldOf returns just the `error` field, which is the one every claim
// here is about. Asserting on the whole response body would be answered by
// whatever else the envelope happens to carry.
func errorFieldOf(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response was not a JSON error envelope: %v (body %.200q)", err, body)
	}
	return env.Error
}

// assertBounded is the pair the digest's rule asks for: a ceiling on a field is
// satisfied by moving the unbounded text somewhere else, or by blanking the
// field entirely, so the ceiling never travels alone.
func assertBounded(t *testing.T, site string, body []byte) string {
	t.Helper()
	msg := errorFieldOf(t, body)
	if len(msg) > errorFieldCeiling {
		t.Errorf("%s: a %d-byte value produced a %d-byte error field (ceiling %d)",
			site, oversizedLength, len(msg), errorFieldCeiling)
	}
	if msg == "" {
		t.Errorf("%s: error field is empty, so the caller is told the value is bad and not why", site)
	}
	// The whole response, not just the field — a repair that relocates the
	// echo into a sibling key passes the ceiling above while changing nothing
	// a caller on the wire can measure.
	if len(body) > errorFieldCeiling+200 {
		t.Errorf("%s: error field is %d bytes but the whole body is %d — the echo moved rather than went",
			site, len(msg), len(body))
	}
	return msg
}

func TestAnOversizedFromIsNotEchoedBackByTheOccurrencesRoute(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createScheduledItem(t, h)

	req := httptest.NewRequest("GET", "/api/items/"+id+"/occurrences?from="+url.QueryEscape(oversized()), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("want 400 for an unparseable from, got %d: %.200s", w.Code, w.Body.String())
	}
	assertBounded(t, "occurrences from", w.Body.Bytes())
}

func TestAnOversizedToIsNotEchoedBackByTheOccurrencesRoute(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createScheduledItem(t, h)

	// `from` is valid so the handler reaches the `to` branch. Without it the
	// from-site answers for the to-site and this case passes while testing
	// nothing.
	req := httptest.NewRequest("GET",
		"/api/items/"+id+"/occurrences?from=2026-01-01T00:00:00Z&to="+url.QueryEscape(oversized()), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("want 400 for an unparseable to, got %d: %.200s", w.Code, w.Body.String())
	}
	assertBounded(t, "occurrences to", w.Body.Bytes())
}

func TestAnOversizedItemActionIsNotEchoedBack(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createScheduledItem(t, h)

	req := httptest.NewRequest("GET", "/api/items/"+id+"/"+oversized(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("want 404 for an unknown action, got %d: %.200s", w.Code, w.Body.String())
	}
	assertBounded(t, "unknown item action", w.Body.Bytes())
}

// The four schedule fields below are validated on write, so the bound is on a
// POST body rather than a query string. `tzid`, `mode` and `channels` are 1x
// sites; `remind.lead` is the 3x one and is the only one where bounding a
// single half would have left the body over 10 KB.
func TestAnOversizedScheduleFieldIsNotEchoedBackOnWrite(t *testing.T) {
	digits := strings.Repeat("9", oversizedLength)
	cases := []struct {
		site     string
		schedule map[string]interface{}
	}{
		{"schedule.tzid", map[string]interface{}{
			"dtstart": "2026-08-01T09:00:00-07:00", "tzid": oversized(), "rrule": "FREQ=WEEKLY;BYDAY=MO;COUNT=2"}},
		{"schedule.mode", map[string]interface{}{
			"dtstart": "2026-08-01T09:00:00-07:00", "tzid": "America/Los_Angeles",
			"rrule": "FREQ=WEEKLY;BYDAY=MO;COUNT=2", "mode": oversized()}},
		{"schedule.remind.lead", map[string]interface{}{
			"dtstart": "2026-08-01T09:00:00-07:00", "tzid": "America/Los_Angeles",
			"rrule":  "FREQ=WEEKLY;BYDAY=MO;COUNT=2",
			"remind": map[string]interface{}{"lead": []string{"-P" + digits + "D"}}}},
		{"schedule.remind.channels", map[string]interface{}{
			"dtstart": "2026-08-01T09:00:00-07:00", "tzid": "America/Los_Angeles",
			"rrule":  "FREQ=WEEKLY;BYDAY=MO;COUNT=2",
			"remind": map[string]interface{}{"channels": []string{oversized()}}}},
	}

	for _, c := range cases {
		t.Run(c.site, func(t *testing.T) {
			a, cleanup := setup(t)
			defer cleanup()
			h := a.Handler()

			body, _ := json.Marshal(map[string]interface{}{
				"type": "todo", "title": "bound probe", "schedule": c.schedule})
			req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != 400 {
				t.Fatalf("want 400 for a bad %s, got %d: %.200s", c.site, w.Code, w.Body.String())
			}
			assertBounded(t, c.site, w.Body.Bytes())
		})
	}
}

// TestAnOrdinaryBadValueStillSaysWhy is the discriminating negative. A repair
// that blanks the message, or replaces it with a fixed string, satisfies every
// ceiling above while destroying the only thing the field is for.
func TestAnOrdinaryBadValueStillSaysWhy(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createScheduledItem(t, h)

	req := httptest.NewRequest("GET", "/api/items/"+id+"/occurrences?from=2026-13-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %.200s", w.Code, w.Body.String())
	}
	msg := errorFieldOf(t, w.Body.Bytes())
	if !strings.Contains(msg, "month out of range") {
		t.Errorf("error %q dropped the parser's diagnosis; a caller cannot tell a bad month from a bad format", msg)
	}
}

// TestAnOrdinaryBadScheduleFieldStillNamesTheValue is the second discriminating
// negative, on the other half. A repair that drops the value instead of cutting
// it also passes every ceiling, and then the caller is told a field is wrong
// without being told which value was rejected.
func TestAnOrdinaryBadScheduleFieldStillNamesTheValue(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	body, _ := json.Marshal(map[string]interface{}{
		"type": "todo", "title": "bound probe", "schedule": map[string]interface{}{
			"dtstart": "2026-08-01T09:00:00-07:00", "tzid": "America/Los_Angeles",
			"rrule":  "FREQ=WEEKLY;BYDAY=MO;COUNT=2",
			"remind": map[string]interface{}{"channels": []string{"pigeon"}}}})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %.200s", w.Code, w.Body.String())
	}
	msg := errorFieldOf(t, w.Body.Bytes())
	if !strings.Contains(msg, "pigeon") {
		t.Errorf("error %q dropped the rejected value; a caller with several channels cannot tell which one was refused", msg)
	}
}

// createScheduledItem returns the id of an item that really carries a schedule.
func createScheduledItem(t *testing.T, h http.Handler) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"type": "note", "title": "scheduled probe",
		"schedule": map[string]interface{}{
			"dtstart": "2026-08-01T09:00:00-07:00",
			"tzid":    "America/Los_Angeles",
			"rrule":   "FREQ=WEEKLY;BYDAY=MO;COUNT=3",
		}})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create scheduled item: %d %.200s", w.Code, w.Body.String())
	}
	var item model.Item
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode created item: %v", err)
	}
	return item.ID
}
