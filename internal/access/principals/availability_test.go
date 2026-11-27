package principals

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Europe/Amsterdam moved to summer time on 2026-03-29 at 02:00. The two Mondays
// either side of it, at the same UTC instant, land on different local clocks:
// that is what a zone gives and an offset cannot.
var (
	mondayBeforeDST = time.Date(2026, 3, 23, 7, 30, 0, 0, time.UTC) // 08:30 CET
	mondayAfterDST  = time.Date(2026, 3, 30, 7, 30, 0, 0, time.UTC) // 09:30 CEST
	saturday        = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
)

func amsterdamWeek() *Availability {
	return &Availability{TZID: "Europe/Amsterdam", Days: []string{"MO", "TU", "WE", "TH", "FR"}, Start: "09:00", End: "17:00"}
}

func patchAvailability(t *testing.T, s *Store, id string, a *Availability) *Principal {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Patch(testContext, id, Patch{Availability: raw})
	if err != nil {
		t.Fatalf("patch availability: %v", err)
	}
	return p
}

func TestAvailabilityValidateRefusesWhatAClockCannotRead(t *testing.T) {
	cases := map[string]Availability{
		"no zone":      {Days: []string{"MO"}, Start: "09:00", End: "17:00"},
		"offset":       {TZID: "+02:00", Days: []string{"MO"}, Start: "09:00", End: "17:00"},
		"unknown zone": {TZID: "Mars/Olympus", Days: []string{"MO"}, Start: "09:00", End: "17:00"},
		"no days":      {TZID: "Europe/Amsterdam", Start: "09:00", End: "17:00"},
		"bad day":      {TZID: "Europe/Amsterdam", Days: []string{"MONDAY"}, Start: "09:00", End: "17:00"},
		"bad clock":    {TZID: "Europe/Amsterdam", Days: []string{"MO"}, Start: "9am", End: "17:00"},
		"start=end":    {TZID: "Europe/Amsterdam", Days: []string{"MO"}, Start: "09:00", End: "09:00"},
		"start>end":    {TZID: "Europe/Amsterdam", Days: []string{"MO"}, Start: "17:00", End: "09:00"},
	}
	for name, a := range cases {
		if err := a.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	ok := Availability{TZID: "Europe/Amsterdam", Days: []string{"mo", " MO", "fr"}, Start: "09:00", End: "17:00"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid week refused: %v", err)
	}
	if strings.Join(ok.Days, ",") != "MO,FR" {
		t.Fatalf("days not normalized and de-duplicated: %v", ok.Days)
	}
}

func TestAvailabilityIsReadInThePrincipalsOwnZoneAcrossDST(t *testing.T) {
	s := newTestStore(t)
	helena := mustCreate(t, s, KindHuman, "Helena Vos", "")
	patchAvailability(t, s, helena.ID, amsterdamWeek())

	before, err := s.Availability(testContext, helena.ID, mondayBeforeDST)
	if err != nil {
		t.Fatal(err)
	}
	if before.Available || before.Reason != ReasonOffHours {
		t.Fatalf("07:30 UTC on 2026-03-23 is 08:30 CET, before the day starts; got %+v", before)
	}
	after, err := s.Availability(testContext, helena.ID, mondayAfterDST)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Available || after.Reason != ReasonInHours {
		t.Fatalf("07:30 UTC on 2026-03-30 is 09:30 CEST, inside the day; got %+v", after)
	}
	weekend, err := s.Availability(testContext, helena.ID, saturday)
	if err != nil {
		t.Fatal(err)
	}
	if weekend.Available || weekend.Reason != ReasonOffHours {
		t.Fatalf("Saturday should be off_hours; got %+v", weekend)
	}
	if after.At != mondayAfterDST.Unix() || after.PrincipalID != helena.ID {
		t.Fatalf("answer does not echo what was asked: %+v", after)
	}
}

func TestAvailabilityReasonsInOrder(t *testing.T) {
	s := newTestStore(t)
	inHours := mondayAfterDST
	nobody := mustCreate(t, s, KindHuman, "No Schedule", "")
	answer, _ := s.Availability(testContext, nobody.ID, inHours)
	if answer.Available || answer.Reason != ReasonNoSchedule {
		t.Fatalf("no schedule must be unknown, never available; got %+v", answer)
	}

	away := mustCreate(t, s, KindHuman, "Away", "")
	patchAvailability(t, s, away.ID, amsterdamWeek())
	if _, err := s.AddTimeOff(testContext, away.ID, inHours.Add(-time.Hour).Unix(), inHours.Add(time.Hour).Unix(), "dentist"); err != nil {
		t.Fatal(err)
	}
	answer, _ = s.Availability(testContext, away.ID, inHours)
	if answer.Available || answer.Reason != ReasonTimeOff {
		t.Fatalf("time off must override the week; got %+v", answer)
	}
	answer, _ = s.Availability(testContext, away.ID, inHours.Add(2*time.Hour))
	if !answer.Available {
		t.Fatalf("after the absence ends the week applies again; got %+v", answer)
	}

	gone := mustCreate(t, s, KindHuman, "Gone", "")
	patchAvailability(t, s, gone.ID, amsterdamWeek())
	if _, err := s.Disable(testContext, gone.ID); err != nil {
		t.Fatal(err)
	}
	answer, _ = s.Availability(testContext, gone.ID, inHours)
	if answer.Available || answer.Reason != ReasonDisabled {
		t.Fatalf("disabled wins over hours; got %+v", answer)
	}

	group := mustCreate(t, s, KindGroup, "Team", "")
	if _, err := s.Availability(testContext, group.ID, inHours); err == nil {
		t.Fatal("a group has no hours of its own and must be refused")
	}
}

func TestTimeOffRangeMustBeNonEmptyAndOnAHuman(t *testing.T) {
	s := newTestStore(t)
	human := mustCreate(t, s, KindHuman, "H", "")
	group := mustCreate(t, s, KindGroup, "G", "")
	if _, err := s.AddTimeOff(testContext, human.ID, 200, 100, ""); err == nil {
		t.Fatal("starts_at after ends_at must be refused")
	}
	if _, err := s.AddTimeOff(testContext, human.ID, 100, 100, ""); err == nil {
		t.Fatal("an empty range must be refused")
	}
	if _, err := s.AddTimeOff(testContext, group.ID, 100, 200, ""); err == nil {
		t.Fatal("a group cannot take time off")
	}
	if _, err := s.AddTimeOff(testContext, "principal_999999", 100, 200, ""); err == nil {
		t.Fatal("unknown principal must be refused")
	}
	row, err := s.AddTimeOff(testContext, human.ID, 100, 200, "  holiday ")
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "timeoff_000001" || row.Note != "holiday" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if err := s.RemoveTimeOff(testContext, group.ID, row.ID); err == nil {
		t.Fatal("a time_off row must not be removable through another principal's path")
	}
	if err := s.RemoveTimeOff(testContext, human.ID, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTimeOff(testContext, human.ID, row.ID); err == nil {
		t.Fatal("second remove must be a not-found")
	}
}

func TestPatchClearsAvailabilityWithEmptyObjectOrNull(t *testing.T) {
	s := newTestStore(t)
	human := mustCreate(t, s, KindHuman, "H", "")
	p := patchAvailability(t, s, human.ID, amsterdamWeek())
	if p.Availability == nil || p.Availability.TZID != "Europe/Amsterdam" {
		t.Fatalf("availability not stored: %+v", p.Availability)
	}
	// A patch that says nothing about availability leaves it alone.
	name := "Renamed"
	p, err := s.Patch(testContext, human.ID, Patch{DisplayName: &name})
	if err != nil {
		t.Fatal(err)
	}
	if p.Availability == nil {
		t.Fatal("a patch of display_name alone must not clear the week")
	}
	for _, clear := range []string{"{}", "null"} {
		patchAvailability(t, s, human.ID, amsterdamWeek())
		p, err := s.Patch(testContext, human.ID, Patch{Availability: json.RawMessage(clear)})
		if err != nil {
			t.Fatalf("clear with %s: %v", clear, err)
		}
		if p.Availability != nil {
			t.Fatalf("clear with %s left %+v", clear, p.Availability)
		}
	}
	group := mustCreate(t, s, KindGroup, "G", "")
	raw, _ := json.Marshal(amsterdamWeek())
	if _, err := s.Patch(testContext, group.ID, Patch{Availability: raw}); err == nil {
		t.Fatal("a group must not carry a week")
	}
	if _, err := s.Patch(testContext, human.ID, Patch{Availability: json.RawMessage(`{"tzid":"Europe/Amsterdam","dyas":["MO"],"start":"09:00","end":"17:00"}`)}); err == nil {
		t.Fatal("a misspelled key inside availability must be refused, not dropped")
	}
}

func TestListAvailableAtFiltersHumansAndPagesAfterTheFilter(t *testing.T) {
	s := newTestStore(t)
	inHours := mondayAfterDST
	names := []string{"A Available", "B Available", "C Available", "D Off", "E Unknown"}
	for _, name := range names {
		p := mustCreate(t, s, KindHuman, name, "")
		switch {
		case strings.HasSuffix(name, "Available"):
			patchAvailability(t, s, p.ID, amsterdamWeek())
		case strings.HasSuffix(name, "Off"):
			patchAvailability(t, s, p.ID, amsterdamWeek())
			if _, err := s.AddTimeOff(testContext, p.ID, inHours.Add(-time.Hour).Unix(), inHours.Add(time.Hour).Unix(), ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	group := mustCreate(t, s, KindGroup, "Team", "")
	all, _ := s.List(testContext, Filter{})
	for _, p := range all {
		if p.Kind == KindHuman {
			if _, err := s.AddMember(testContext, group.ID, p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	got, err := s.List(testContext, Filter{AvailableAt: &inHours})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].DisplayName != "A Available" || got[2].DisplayName != "C Available" {
		t.Fatalf("expected the three available humans, got %v", displayNames(got))
	}
	paged, err := s.List(testContext, Filter{AvailableAt: &inHours, Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(paged) != 2 || paged[0].DisplayName != "B Available" || paged[1].DisplayName != "C Available" {
		t.Fatalf("page must be cut after the filter, got %v", displayNames(paged))
	}
	n, err := s.Count(testContext, Filter{AvailableAt: &inHours})
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := s.List(testContext, Filter{AvailableAt: &inHours, Kind: KindGroup}); err == nil {
		t.Fatal("available_at with kind=group must be refused")
	}
	members, err := s.ListAvailableMembers(testContext, group.ID, inHours)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("expected 3 available members, got %v", displayNames(members))
	}
	if _, err := s.ListAvailableMembers(testContext, all[0].ID, inHours); err == nil {
		t.Fatal("members of a human must be refused")
	}
}

func displayNames(principals []*Principal) []string {
	out := make([]string, 0, len(principals))
	for _, p := range principals {
		out = append(out, p.DisplayName)
	}
	return out
}

func TestAvailabilityOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	helena := post(t, srv, KindHuman, "Helena Vos", "")
	group := post(t, srv, KindGroup, "Team", "")
	do(t, srv, http.MethodPut, "/principals/"+group.ID+"/members/"+helena.ID, nil)

	status, body := do(t, srv, http.MethodPatch, "/principals/"+helena.ID, map[string]any{"availability": amsterdamWeek()})
	if status != 200 {
		t.Fatalf("patch availability: %d %s", status, body)
	}
	var got Principal
	json.Unmarshal(body, &got)
	if got.Availability == nil || got.Availability.End != "17:00" {
		t.Fatalf("availability not echoed: %s", body)
	}

	status, body = do(t, srv, http.MethodGet, fmt.Sprintf("/principals/%s/availability?at=%d", helena.ID, mondayAfterDST.Unix()), nil)
	if status != 200 || !strings.Contains(string(body), `"reason":"in_hours"`) {
		t.Fatalf("availability: %d %s", status, body)
	}
	status, body = do(t, srv, http.MethodGet, "/principals/"+helena.ID+"/availability?at=2026-09-11", nil)
	if status != 400 {
		t.Fatalf("a non-integer at must be a 400, got %d %s", status, body)
	}
	status, body = do(t, srv, http.MethodGet, "/principals/"+group.ID+"/availability", nil)
	if status != 400 {
		t.Fatalf("a group's availability must be a 400, got %d %s", status, body)
	}

	status, body = do(t, srv, http.MethodGet, fmt.Sprintf("/principals/%s/members?available_at=%d", group.ID, mondayAfterDST.Unix()), nil)
	if status != 200 || !strings.Contains(string(body), helena.ID) {
		t.Fatalf("members available: %d %s", status, body)
	}
	status, body = do(t, srv, http.MethodGet, fmt.Sprintf("/principals/%s/members?available_at=%d", group.ID, saturday.Unix()), nil)
	if status != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("nobody works Saturday: %d %s", status, body)
	}
	status, body = do(t, srv, http.MethodGet, fmt.Sprintf("/principals?available_at=%d", mondayAfterDST.Unix()), nil)
	if status != 200 || !strings.Contains(string(body), helena.ID) || strings.Contains(string(body), group.ID) {
		t.Fatalf("list available: %d %s", status, body)
	}

	status, body = do(t, srv, http.MethodPost, "/principals/"+helena.ID+"/time-off",
		map[string]any{"starts_at": mondayAfterDST.Add(-time.Hour).Unix(), "ends_at": mondayAfterDST.Add(time.Hour).Unix(), "note": "dentist"})
	if status != 201 {
		t.Fatalf("add time off: %d %s", status, body)
	}
	var off TimeOff
	json.Unmarshal(body, &off)
	status, body = do(t, srv, http.MethodGet, fmt.Sprintf("/principals/%s/availability?at=%d", helena.ID, mondayAfterDST.Unix()), nil)
	if !strings.Contains(string(body), `"reason":"time_off"`) {
		t.Fatalf("expected time_off: %d %s", status, body)
	}
	status, body = do(t, srv, http.MethodGet, "/principals/"+helena.ID+"/time-off", nil)
	if status != 200 || !strings.Contains(string(body), off.ID) {
		t.Fatalf("list time off: %d %s", status, body)
	}
	status, _ = do(t, srv, http.MethodDelete, "/principals/"+helena.ID+"/time-off/"+off.ID, nil)
	if status != 204 {
		t.Fatalf("delete time off: %d", status)
	}
	status, _ = do(t, srv, http.MethodDelete, "/principals/"+helena.ID+"/time-off/"+off.ID, nil)
	if status != 404 {
		t.Fatalf("second delete: %d", status)
	}
	status, body = do(t, srv, http.MethodPost, "/principals/"+helena.ID+"/time-off", map[string]any{"start_at": 1, "ends_at": 2})
	if status != 400 {
		t.Fatalf("misspelled key must be a 400: %d %s", status, body)
	}

	status, body = do(t, srv, http.MethodGet, "/availability-reasons", nil)
	if status != 200 || !strings.Contains(string(body), "no_schedule") {
		t.Fatalf("reasons: %d %s", status, body)
	}
	status, body = do(t, srv, http.MethodPatch, "/principals/"+helena.ID, map[string]any{"availability": map[string]any{}})
	if status != 200 || strings.Contains(string(body), `"availability"`) {
		t.Fatalf("clearing with {} must drop the key from the row: %d %s", status, body)
	}
}
