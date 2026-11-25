package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
)

// The boundary VALUES, ported from 081b8ac6.
//
// The other tests pin mechanisms, and the ones that touch a number are written
// in terms of it: `time.Now().Add(-2*reviveWindow)` sits past the window
// whatever the window is. That is the right way to test the mechanism and it
// cannot fail for a wrong value, so each value here has a second fixture that
// quotes the number itself. scripts/score-dispatcher-degradations.py moves each
// value by one unit and expects a test in this package to go red.

// siblingIdleFor is a board whose one Backlog card has an idle sibling session
// last updated age ago, and a bridge that reports it so. Everything else is the
// shape that IS revived, so the age alone decides.
func siblingIdleFor(t *testing.T, age time.Duration) (*fakeKanban, map[string]string) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
		columnBacklog: {testCard("card-next", "group:a", "cat:product")},
		columnDone:    {testCard("card-finished", "group:a", "cat:product")},
	})
	kanban.effective["card-next"] = productCardDefaults()
	kanban.status["card-finished"] = "done"
	kanban.links = map[string][]kanbanclient.CardLink{
		"card-finished": {{CardID: "card-finished", EntityType: "session", EntityRef: "sess-sibling"}},
	}
	body := `{"state":"idle","updated_at":"` + time.Now().Add(-age).UTC().Format(time.RFC3339) +
		`","instance_id":"inst-codex-local","agent_id":"northwind-builder","bundle_id":"6"}`
	return kanban, map[string]string{"sess-sibling": body}
}

// Two fixtures thirty seconds either side of one hour, so a drift of a minute
// in either direction puts one of them on the wrong side. Thirty seconds, not
// one: the timestamp is formatted to whole seconds and compared against the
// clock when the tick runs.
func TestTheReviveWindowIsOneHour(t *testing.T) {
	t.Run("thirty seconds inside the hour is revived", func(t *testing.T) {
		kanban, sessions := siblingIdleFor(t, time.Hour-30*time.Second)
		err, published := tick(t, kanban, sessions)
		if err != nil || len(published) != 1 || published[0].BusSessionID != "sess-sibling" || published[0].Create != nil {
			t.Errorf("a sibling idle for 59m30s must be revived; the window has shrunk below an hour "+
				"and every tick now pays for a fresh session: err=%v published=%+v", err, published)
		}
	})
	t.Run("thirty seconds past the hour is not revived", func(t *testing.T) {
		kanban, sessions := siblingIdleFor(t, time.Hour+30*time.Second)
		err, published := tick(t, kanban, sessions)
		if err != nil || len(published) != 1 || published[0].BusSessionID == "sess-sibling" || published[0].Create == nil {
			t.Errorf("a sibling idle for 60m30s must not be revived; the window has grown past an hour: "+
				"err=%v published=%+v", err, published)
		}
	})
}

// The cap reaches run only as the -max flag's default, which main() defines, so
// no tick can be driven with it. TestThePerTickCapIsNotAShortfall pins the
// mechanism; this pins the number, which bounds what one tick spends.
func TestThePerTickDispatchCapIsThree(t *testing.T) {
	if maxDispatchPerTick != 3 {
		t.Errorf("maxDispatchPerTick = %d, want 3", maxDispatchPerTick)
	}
}

// registerSchedulerJob reads any status at or above 300 as a refusal. 201 and
// 400 agree for every threshold from 202 to 400; 300 and 299 are the two
// statuses that tell `>= 300` from `>= 301` and from `>= 299`. Go's client
// follows no 300, so the status reaches the caller as sent.
func TestRegisterRefusesAt300AndAcceptsAt299(t *testing.T) {
	if err := registerAgainst(t, &schedulerStub{postStatus: http.StatusMultipleChoices}); err == nil {
		t.Error("the scheduler answered 300 and -register reported the tick as registered")
	}
	if err := registerAgainst(t, &schedulerStub{postStatus: 299}); err != nil {
		t.Errorf("the scheduler answered 299 and -register called it a refusal: %v", err)
	}
}
