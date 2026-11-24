package api_test

import (
	"reflect"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// bridge-ui's settings form sends business_hours as {tzid, days, start, end}
// and knows nothing of holidays. The store replaces that object whole, so
// without a rule the first save of the working week would wipe every holiday.
func TestSavingTheWorkingWeekKeepsTheHolidaysItDidNotMention(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Support Desk")

	holidaysAfter := func(what string, patch map[string]any, wantStatus int) []string {
		t.Helper()
		w := do(t, h, "PATCH", "/api/boards/"+boardID, patch)
		if w.Code != wantStatus {
			t.Fatalf("%s: %d %s, want %d", what, w.Code, w.Body.String(), wantStatus)
		}
		var board model.Board
		decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID, nil), &board)
		if board.BusinessHours == nil {
			return nil
		}
		return board.BusinessHours.Holidays
	}
	week := func(extra map[string]any) map[string]any {
		hours := map[string]any{"tzid": "America/Los_Angeles", "days": []string{"MO", "TU", "WE", "TH", "FR"}, "start": "09:00", "end": "17:00"}
		for key, value := range extra {
			hours[key] = value
		}
		return map[string]any{"business_hours": hours}
	}

	got := holidaysAfter("set the week with two holidays, out of order", week(map[string]any{"holidays": []string{"2026-12-25", "2026-11-26"}}), 200)
	if want := []string{"2026-11-26", "2026-12-25"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("holidays = %v, want %v, sorted", got, want)
	}
	got = holidaysAfter("save the week again, as a client that has never heard of holidays", week(map[string]any{"end": "18:00"}), 200)
	if want := []string{"2026-11-26", "2026-12-25"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("holidays = %v after a save that did not mention them, want %v kept", got, want)
	}
	got = holidaysAfter("a holiday that is not a date", week(map[string]any{"holidays": []string{"Christmas"}}), 400)
	if len(got) != 2 {
		t.Fatalf("a refused save changed the holidays: %v", got)
	}
	if got = holidaysAfter("send an empty list", week(map[string]any{"holidays": []string{}}), 200); len(got) != 0 {
		t.Fatalf("holidays = %v after an empty list, want none", got)
	}
	holidaysAfter("set one again", week(map[string]any{"holidays": []string{"2027-01-01"}}), 200)
	if got = holidaysAfter("clear the business hours", map[string]any{"business_hours": map[string]any{}}, 200); got != nil {
		t.Fatalf("holidays = %v survived the hours they were dates in", got)
	}
}
