package model

import (
	"strings"
	"testing"
)

// kanban-store has no auth and listens on *:8305, so the length of a 400 body is
// whatever an unauthenticated caller decides to make it. Every validation error
// below embeds a value that came straight off the request, and each one was
// measured live against the deployed binary on 2026-09-07 with a 5 000-byte
// value:
//
//	PATCH /api/boards/{id}       business_hours.tzid       400,  5 050 bytes
//	PATCH /api/boards/{id}       business_hours.days[0]    400,  5 055
//	PATCH /api/boards/{id}       business_hours.start      400,  5 036
//	PATCH /api/boards/{id}       business_hours.start hh   400,  5 042
//	PATCH /api/boards/{id}       business_hours.start mm   400,  5 044
//	POST  /api/cards/{id}/events kind                      400,  5 077
//	PUT   .../priority-levels    duplicate label           400,  5 033
//	PUT   .../priority-levels    budget_seconds label      400,  5 088
//
// The same requests with a 10-byte value answer between 43 and 98 bytes, so the
// caller chose ~4 990 of those bytes at every site. The ceiling below is what
// stops that, and it is deliberately loose: it is not a style rule about message
// length, it is the bound that makes the body's size a property of this code
// rather than of the request.
const errorCeiling = 300

// caller is a value an unauthenticated caller can put in a request body. It is
// far past any plausible real tzid, day code, clock time or label.
var caller = strings.Repeat("x", 5000)

func checkBounded(t *testing.T, site string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a validation error, got nil — the case has stopped exercising the site", site)
	}
	if n := len(err.Error()); n > errorCeiling {
		t.Errorf("%s: error is %d bytes against a ceiling of %d; an unauthenticated caller is choosing the length of a 400\n\tgot: %.200q…",
			site, n, errorCeiling, err.Error())
	}
}

func TestABusinessHoursErrorDoesNotCarryTheCallersWholeValue(t *testing.T) {
	ok := func(mut func(*BusinessHours)) *BusinessHours {
		b := &BusinessHours{TZID: "America/Los_Angeles", Days: []string{"MO"}, Start: "09:00", End: "17:00"}
		mut(b)
		return b
	}
	for _, tc := range []struct {
		site string
		h    *BusinessHours
	}{
		{"tzid", ok(func(b *BusinessHours) { b.TZID = caller })},
		{"days[0]", ok(func(b *BusinessHours) { b.Days = []string{caller} })},
		{"start is not HH:MM", ok(func(b *BusinessHours) { b.Start = caller })},
		{"start hour", ok(func(b *BusinessHours) { b.Start = caller + ":00" })},
		{"start minute", ok(func(b *BusinessHours) { b.Start = "09:" + caller })},
	} {
		t.Run(tc.site, func(t *testing.T) { checkBounded(t, tc.site, tc.h.Validate()) })
	}
}

func TestAnEventKindErrorDoesNotCarryTheCallersWholeValue(t *testing.T) {
	r := &CreateCardEventRequest{Kind: EventKind(caller)}
	checkBounded(t, "event kind", r.Validate())
}

func TestAPriorityLadderErrorDoesNotCarryTheCallersWholeValue(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		site string
		req  *SetPriorityLadderRequest
	}{
		{"duplicate label", &SetPriorityLadderRequest{Levels: []BoardPriorityLevel{
			{PriorityValue: 1, Label: caller}, {PriorityValue: 2, Label: caller},
		}}},
		{"budget_seconds for label", &SetPriorityLadderRequest{Levels: []BoardPriorityLevel{
			{PriorityValue: 1, Label: caller, BudgetSeconds: &zero},
		}}},
	} {
		t.Run(tc.site, func(t *testing.T) { checkBounded(t, tc.site, tc.req.Validate()) })
	}
}

// The end-before-start message renders Start and End too, and it is NOT a
// defect: both have already parsed as HH:MM to reach that line, so they are five
// bytes each whatever the caller sent. It is here as the negative control — it
// passes before the repair as well as after, so a run where every case is green
// does not by itself prove the repair landed.
func TestCONTROLTheEndBeforeStartErrorWasNeverUnbounded(t *testing.T) {
	h := &BusinessHours{TZID: "America/Los_Angeles", Days: []string{"MO"}, Start: "09:00", End: "08:00"}
	checkBounded(t, "CONTROL end before start", h.Validate())
}

// A bound that swallows the value would also pass the ceiling, and would be a
// worse defect than the one being repaired: an operator debugging a rejected
// tzid needs to see which tzid was rejected. These pin that the message still
// names the value and still says what was wrong with it.
func TestTheBoundedErrorStillIdentifiesTheValueAndTheProblem(t *testing.T) {
	h := &BusinessHours{TZID: "Nope/Nope", Days: []string{"MO"}, Start: "09:00", End: "17:00"}
	err := h.Validate()
	if err == nil {
		t.Fatal("expected an error for tzid Nope/Nope")
	}
	if !strings.Contains(err.Error(), "Nope/Nope") {
		t.Errorf("a short tzid must survive intact so the message is actionable; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown tzid") {
		t.Errorf("the message must still say what was wrong; got %q", err.Error())
	}
}

// A value cut at the ceiling still has to be recognizable as the caller's, which
// means the leading bytes survive.
func TestALongValueIsCutRatherThanReplaced(t *testing.T) {
	h := &BusinessHours{TZID: "Europe/" + caller, Days: []string{"MO"}, Start: "09:00", End: "17:00"}
	err := h.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Europe/") {
		t.Errorf("the head of the value must survive the cut; got %q", err.Error())
	}
}
