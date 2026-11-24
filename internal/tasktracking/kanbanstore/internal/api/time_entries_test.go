package api_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

func logTime(t *testing.T, h http.Handler, who map[string]string, cardID string, request model.CreateTimeEntryRequest, want int, what string) model.TimeEntry {
	t.Helper()
	w := requestAs(t, h, who, "POST", "/api/cards/"+cardID+"/time-entries", request)
	mustStatus(t, w, want, what)
	var entry model.TimeEntry
	if want == 201 {
		decodeSuccessfulResponse(t, w, &entry)
	}
	return entry
}

func timeEntriesOf(t *testing.T, h http.Handler, cardID, query string) model.CardTimeEntries {
	t.Helper()
	w := requestAs(t, h, asService, "GET", "/api/cards/"+cardID+"/time-entries"+query, nil)
	mustStatus(t, w, 200, "time entries"+query)
	var entries model.CardTimeEntries
	decodeSuccessfulResponse(t, w, &entries)
	return entries
}

// alice logs 30 minutes, then sees it was 45. The correction is a new entry
// naming the old one. The old one is not rewritten: it stays on the timeline as
// she logged it, inactive, and only the correction counts.
func TestCorrectingATimeEntryDeactivatesTheOldOneAndKeepsIt(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	first := logTime(t, h, asPrincipal(alice), f.supportCardID, model.CreateTimeEntryRequest{Seconds: 1800, Summary: "call with the carrier"}, 201, "alice logs 30 minutes")
	if first.WorkerPrincipalID != alice || first.LoggedBy != alice || !first.Active {
		t.Fatalf("first entry = %+v, want alice as worker and logger, active", first)
	}
	corrected := logTime(t, h, asPrincipal(alice), f.supportCardID, model.CreateTimeEntryRequest{
		Seconds: 2700, Summary: "call with the carrier, and the write-up", SupersedesEventID: first.EventID,
	}, 201, "alice corrects it to 45")

	all := timeEntriesOf(t, h, f.supportCardID, "")
	if all.ActiveSeconds != 2700 {
		t.Errorf("active seconds = %d, want 2700: the replaced 1800 must not count", all.ActiveSeconds)
	}
	if len(all.Entries) != 2 {
		t.Fatalf("%d entries, want both: the old one is kept", len(all.Entries))
	}
	old, replacement := all.Entries[0], all.Entries[1]
	if old.Active || old.SupersededByEventID != corrected.EventID || old.Seconds != 1800 || old.Summary != "call with the carrier" {
		t.Errorf("the old entry = %+v, want it exactly as logged, inactive, pointing at %s", old, corrected.EventID)
	}
	if !replacement.Active || replacement.SupersedesEventID != first.EventID {
		t.Errorf("the correction = %+v, want active and naming %s", replacement, first.EventID)
	}
	if active := timeEntriesOf(t, h, f.supportCardID, "?active=true"); len(active.Entries) != 1 || active.Entries[0].EventID != corrected.EventID {
		t.Errorf("?active=true = %+v, want only the correction", active.Entries)
	}

	// The timeline shows both, marks the replaced one, and totals the active.
	w := requestAs(t, h, asService, "GET", "/api/cards/"+f.supportCardID+"/timeline?board_id="+f.supportBoardID, nil)
	mustStatus(t, w, 200, "timeline")
	var timeline model.CardTimeline
	decodeSuccessfulResponse(t, w, &timeline)
	if timeline.Summary.LoggedSeconds != 2700 {
		t.Errorf("timeline logged_seconds = %d, want 2700", timeline.Summary.LoggedSeconds)
	}
	marked := map[string]string{}
	for _, entry := range timeline.Entries {
		if entry.Event.Kind == model.EventTimeLogged {
			marked[entry.Event.ID] = entry.SupersededByEventID
		}
	}
	if len(marked) != 2 || marked[first.EventID] != corrected.EventID || marked[corrected.EventID] != "" {
		t.Errorf("timeline marks = %v, want the first superseded by the correction and the correction by nothing", marked)
	}

	// Withdrawing is a correction to zero.
	logTime(t, h, asPrincipal(alice), f.supportCardID, model.CreateTimeEntryRequest{Seconds: 0, Summary: "logged on the wrong ticket", SupersedesEventID: corrected.EventID}, 201, "alice withdraws it")
	if got := timeEntriesOf(t, h, f.supportCardID, "").ActiveSeconds; got != 0 {
		t.Errorf("active seconds after the withdrawal = %d, want 0", got)
	}
}

// Two people correct the same entry at once. If both landed, the entry's
// history would fork and both corrections would count. Exactly one may win, and
// the loser is told which entry to correct instead.
func TestAnEntryCanBeReplacedOnlyOnce(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	first := logTime(t, h, asPrincipal(alice), f.supportCardID, model.CreateTimeEntryRequest{Seconds: 600}, 201, "the entry")

	const correctors = 8
	responses := make([]*responseSnapshot, correctors)
	var wg sync.WaitGroup
	for i := 0; i < correctors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := requestAs(t, h, asService, "POST", "/api/cards/"+f.supportCardID+"/time-entries",
				model.CreateTimeEntryRequest{Seconds: 900, WorkerPrincipalID: alice, SupersedesEventID: first.EventID})
			responses[i] = &responseSnapshot{status: w.Code, body: w.Body.String()}
		}(i)
	}
	wg.Wait()
	won, refused := 0, 0
	for _, response := range responses {
		switch response.status {
		case 201:
			won++
		case 409:
			refused++
			if !strings.Contains(response.body, "correct that entry instead") {
				t.Errorf("the 409 does not name the entry that won: %s", response.body)
			}
		default:
			t.Errorf("unexpected %d %s", response.status, response.body)
		}
	}
	if won != 1 || refused != correctors-1 {
		t.Fatalf("%d won and %d were refused, want 1 and %d", won, refused, correctors-1)
	}
	if got := timeEntriesOf(t, h, f.supportCardID, "").ActiveSeconds; got != 900 {
		t.Errorf("active seconds = %d, want 900: one correction, counted once", got)
	}
}

type responseSnapshot struct {
	status int
	body   string
}

// Saying how long something took says nothing about whether the work is
// runnable. A card that is waiting on the requester must still be waiting
// after someone logs time on it, or every logged hour would restart its SLA.
func TestLoggingTimeLeavesTheClockWhereItWas(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asService, "POST", "/api/cards/"+f.supportCardID+"/events",
		model.CreateCardEventRequest{Kind: model.EventWaitingStarted}), 201, "the card starts waiting")
	entry := logTime(t, h, asPrincipal(alice), f.supportCardID, model.CreateTimeEntryRequest{Seconds: 900}, 201, "time logged while it waits")

	w := requestAs(t, h, asService, "GET", "/api/cards/"+f.supportCardID+"/events", nil)
	mustStatus(t, w, 200, "events")
	var events []model.CardEvent
	decodeSuccessfulResponse(t, w, &events)
	for _, event := range events {
		if event.ID == entry.EventID && event.ClockState != model.ClockPaused {
			t.Errorf("the time_logged event recorded clock state %q; the card was paused and logging time must not restart it", event.ClockState)
		}
	}
}

func TestATimeEntryThatCannotBeRightIsRefused(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	onFinance := logTime(t, h, asService, f.financeCardID, model.CreateTimeEntryRequest{Seconds: 60, WorkerPrincipalID: carol}, 201, "an entry on another card")
	moved := requestAs(t, h, asService, "GET", "/api/cards/"+f.supportCardID+"/events", nil)
	var events []model.CardEvent
	decodeSuccessfulResponse(t, moved, &events)

	for what, c := range map[string]struct {
		who     map[string]string
		request model.CreateTimeEntryRequest
		want    int
	}{
		"no time at all":                         {asPrincipal(alice), model.CreateTimeEntryRequest{}, 400},
		"negative time":                          {asPrincipal(alice), model.CreateTimeEntryRequest{Seconds: -60}, 400},
		"milliseconds sent as seconds":           {asPrincipal(alice), model.CreateTimeEntryRequest{Seconds: 45 * 60 * 1000 * 1000}, 400},
		"a service that names no worker":         {asService, model.CreateTimeEntryRequest{Seconds: 60}, 400},
		"a worker who does not exist":            {asService, model.CreateTimeEntryRequest{Seconds: 60, WorkerPrincipalID: unknownPrincipal}, 400},
		"a worker who is disabled":               {asService, model.CreateTimeEntryRequest{Seconds: 60, WorkerPrincipalID: disabledPrincipal}, 400},
		"a requester as the worker":              {asService, model.CreateTimeEntryRequest{Seconds: 60, WorkerPrincipalID: requesterContact}, 400},
		"replacing an entry on another card":     {asPrincipal(alice), model.CreateTimeEntryRequest{Seconds: 60, SupersedesEventID: onFinance.EventID}, 400},
		"replacing an event that is not time":    {asPrincipal(alice), model.CreateTimeEntryRequest{Seconds: 60, SupersedesEventID: events[0].ID}, 400},
		"replacing an event that does not exist": {asPrincipal(alice), model.CreateTimeEntryRequest{Seconds: 60, SupersedesEventID: "no-such-event"}, 400},
		"a viewer logging time":                  {asPrincipal(bob), model.CreateTimeEntryRequest{Seconds: 60}, 403},
	} {
		cardID := f.supportCardID
		if what == "a viewer logging time" {
			cardID = f.financeCardID
		}
		logTime(t, h, c.who, cardID, c.request, c.want, what)
	}
	// The generic events route must not be a way around the rules.
	mustStatus(t, requestAs(t, h, asService, "POST", "/api/cards/"+f.supportCardID+"/events",
		model.CreateCardEventRequest{Kind: model.EventTimeLogged, ClockState: model.ClockRunning}), 400, "time_logged through /events")
	if got := timeEntriesOf(t, h, f.supportCardID, ""); len(got.Entries) != 0 {
		t.Errorf("refused requests left %d entries", len(got.Entries))
	}
}
