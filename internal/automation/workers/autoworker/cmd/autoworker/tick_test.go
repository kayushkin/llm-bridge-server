package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// Ported from 145c5bae (test/the-no-secret-rule-is-pinned-by-its-message). On
// main fetchLimits takes the usage-store URL as an argument, so the fixtures
// return a URL instead of overriding the environment and a compiled-in default.
//
// The tick has two halves and the suite only ever drove one of them.
//
// evaluate, selectQueuedCard and publishWorker are all reached directly by
// tests. fetchLimits — which supplies the numbers evaluate decides on — and
// markQueueCardDispatched — which records that the fire happened — are reached
// only from main, and no test runs main. Both sat at 0.0% coverage while the
// package reported 57.5%, and the scorer's denominator named neither, so the
// score, the coverage total and the mutation run all read as healthy.

// ------------------------------------------------------------- fetchLimits

// serveLimits stands in for usage-store's /api/usage/limits and returns its URL.
func serveLimits(t *testing.T, status int, payload any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage/limits" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "usage-store is unwell", status)
			return
		}
		writeFakeJSON(w, payload)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestTheLimitsReadKeepsEveryFieldTheCurveDecidesOn. fetchLimits is the only
// source of the numbers evaluate gates on, and it is the boundary where a
// usage-store field rename turns into a zero rather than an error: the response
// is a map of pointers, so a key that stops matching decodes to a nil provider
// and a window that stops matching decodes to a nil window. Both then read as
// "no data", which the tick treats as do-not-fire — silent, and in the safe
// direction, which is exactly why nothing would ever report it.
func TestTheLimitsReadKeepsEveryFieldTheCurveDecidesOn(t *testing.T) {
	resets := int64(1754870400)
	stale := int64(3600)
	usage := serveLimits(t, http.StatusOK, map[string]any{
		"anthropic": map[string]any{
			"provider":    "anthropic",
			"snapshot_at": 1754784000,
			"source":      "oauth",
			"stale_after": stale,
			"windows": map[string]any{
				"seven_day": map[string]any{"used_percent": 42.5, "resets_at": resets},
			},
		},
	})

	lim, err := fetchLimits(usage)
	if err != nil {
		t.Fatalf("fetchLimits: %v", err)
	}

	p := lim["anthropic"]
	if p == nil {
		t.Fatal("the anthropic provider decoded to nil; evaluate reads that as no data " +
			"and declines to fire, so a key rename upstream stops the dispatcher silently")
	}
	if p.SnapshotAt != 1754784000 {
		t.Errorf("snapshot_at = %d, want 1754784000 — the staleness gate reads it", p.SnapshotAt)
	}
	if p.StaleAfter == nil || *p.StaleAfter != stale {
		t.Errorf("stale_after = %v, want %d", p.StaleAfter, stale)
	}
	w := p.Windows["seven_day"]
	if w == nil {
		t.Fatal("the seven_day window decoded to nil; the whole curve is expressed " +
			"against this window")
	}
	if w.UsedPercent != 42.5 {
		t.Errorf("used_percent = %v, want 42.5 — this is the number the curve is "+
			"compared against, so a zero here fires at full budget", w.UsedPercent)
	}
	if w.ResetsAt == nil || *w.ResetsAt != resets {
		t.Errorf("resets_at = %v, want %d — elapsedFraction counts back from it", w.ResetsAt, resets)
	}
}

// TestAUsageStoreThatIsDownIsAnErrorNotAnEmptyReading. main log.Fatalf's on this
// error, which is the whole point: a tick that read a failure as "no providers"
// would exit 0 having fired nothing, and an autoworker that never fires looks
// exactly like an autoworker whose gates are simply closed.
func TestAUsageStoreThatIsDownIsAnErrorNotAnEmptyReading(t *testing.T) {
	usage := serveLimits(t, http.StatusInternalServerError, nil)

	lim, err := fetchLimits(usage)
	if err == nil {
		t.Fatalf("no error from a usage-store returning 500; got limits %v", lim)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not carry the status the store returned: %v", err)
	}
}

// TestAnUnreadableLimitsBodyIsAnError. The reply arrives 200 and is not what the
// tick can read — the shape usage-store returns when something in front of it
// answers instead, which on this box is an ordinary failure mode.
func TestAnUnreadableLimitsBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not the usage store</html>"))
	}))
	t.Cleanup(srv.Close)

	if _, err := fetchLimits(srv.URL); err == nil {
		t.Fatal("a body the tick cannot parse was read as a set of limits")
	}
}

// -------------------------------------------------- markQueueCardDispatched

// dispatchRecorder serves the three writes markQueueCardDispatched makes and
// records each one, so a test can tell "the call was made" from "the call was
// made with the right thing in it".
type dispatchRecorder struct {
	links  []map[string]any
	moves  []map[string]any
	bodies []string

	// failOn names the path fragment that should return 500 instead.
	failOn string
}

func (d *dispatchRecorder) start(t *testing.T) *issuesclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.failOn != "" && strings.Contains(r.URL.Path, d.failOn) {
			http.Error(w, "kanban-store refused it", http.StatusInternalServerError)
			return
		}
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)

		switch {
		case strings.HasSuffix(r.URL.Path, "/links"):
			d.links = append(d.links, payload)
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/move"):
			d.moves = append(d.moves, payload)
			writeFakeJSON(w, map[string]any{"ok": true})
		case r.Method == http.MethodPatch:
			body, _ := payload["body"].(string)
			d.bodies = append(d.bodies, body)
			writeFakeJSON(w, map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	qc := issuesclient.New(srv.URL, "")
	qc.BoardID = "board-1"
	qc.ColumnsByName = map[string]string{
		"Queued":      "col-queued",
		"In Progress": "col-wip",
	}
	return qc
}

func dispatchedCard() *queueCandidate {
	return &queueCandidate{cardID: "card-1", body: "the original card body"}
}

var dispatchProvider = providerSpec{
	Name:       "anthropic",
	Harness:    "claude_code",
	InstanceID: "inst-cc-local",
}

// TestADispatchedCardLeavesTheQueue. The move is the only thing that stops the
// next tick picking the same card again: selection reads the Queued column and
// nothing else records that a card has already been fired. A dispatch that
// publishes a session and does not move the card re-dispatches it every 30
// minutes, each run spending against the card's ceiling until it is exhausted.
func TestADispatchedCardLeavesTheQueue(t *testing.T) {
	d := &dispatchRecorder{}
	qc := d.start(t)

	err := markQueueCardDispatched(qc, dispatchedCard(), dispatchProvider, "sess-1", time.Unix(1754784000, 0))
	if err != nil {
		t.Fatalf("markQueueCardDispatched: %v", err)
	}

	if len(d.moves) != 1 {
		t.Fatalf("made %d moves, want exactly 1: %v", len(d.moves), d.moves)
	}
	// The position the card lands at, which the recorder has always captured and
	// nothing has ever read. 0 is the head of the column: a dispatched card is the
	// most recent thing that happened and the operator should see it first.
	if got := d.moves[0]["position"]; got != float64(0) {
		t.Errorf("moved to position %v, want 0 — a dispatched card goes to the head of "+
			"In Progress, not wherever the payload happened to say", got)
	}
	if got := d.moves[0]["column_id"]; got != "col-wip" {
		t.Errorf("moved to column %v, want col-wip — In Progress is the only column "+
			"selection does not read from", got)
	}
}

// TestTheSessionLinkIsWhatMakesTheSpendCountable. The spend tree bills a card by
// summing the sessions linked to it, and only links of the session type count.
// A dispatch that attaches the wrong type, or no link at all, produces a card
// that has spent money and reads as having spent nothing — so its ceiling never
// holds it and the cap the user set is not enforced.
func TestTheSessionLinkIsWhatMakesTheSpendCountable(t *testing.T) {
	d := &dispatchRecorder{}
	qc := d.start(t)

	err := markQueueCardDispatched(qc, dispatchedCard(), dispatchProvider, "sess-1", time.Unix(1754784000, 0))
	if err != nil {
		t.Fatalf("markQueueCardDispatched: %v", err)
	}

	if len(d.links) != 1 {
		t.Fatalf("attached %d links, want exactly 1: %v", len(d.links), d.links)
	}
	link := d.links[0]
	if got := link["entity_type"]; got != "session" {
		t.Errorf("entity_type = %v, want %q — the spend tree counts this type and no "+
			"other, so any other value bills the card nothing", got, "session")
	}
	if got := link["entity_ref"]; got != "sess-1" {
		t.Errorf("entity_ref = %v, want the session that was actually published", got)
	}
}

// TestTheDispatchNoteIsAppendedToTheCardRatherThanReplacingIt. The note is the
// card's own audit trail and PatchCardBody is a whole-body write, so the
// difference between appending and replacing is the difference between a card
// that records its dispatches and a card whose task description was destroyed by
// the first one — and the body is the worker prompt.
//
// The timestamp is handed in on a non-UTC clock on purpose. main passes
// time.Now(), which on this box is Pacific, and the note is the only durable
// record of when a session was fired. A note stamped in local time reads as an
// hour that never happened for seven months of the year and cannot be lined up
// against a bridge session id, which is UTC.
func TestTheDispatchNoteIsAppendedToTheCardRatherThanReplacingIt(t *testing.T) {
	d := &dispatchRecorder{}
	qc := d.start(t)

	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	fired := time.Date(2026, 8, 10, 17, 30, 0, 0, pacific)

	if err := markQueueCardDispatched(qc, dispatchedCard(), dispatchProvider, "sess-1", fired); err != nil {
		t.Fatalf("markQueueCardDispatched: %v", err)
	}

	if len(d.bodies) != 1 {
		t.Fatalf("patched the body %d times, want exactly 1", len(d.bodies))
	}
	body := d.bodies[0]
	if !strings.HasPrefix(body, "the original card body") {
		t.Fatalf("the dispatch note replaced the card body instead of appending to it: %q", body)
	}
	for _, want := range []string{"sess-1", "anthropic", "claude_code", "inst-cc-local"} {
		if !strings.Contains(body, want) {
			t.Errorf("the dispatch note does not carry %q: %q", want, body)
		}
	}
	if !strings.Contains(body, "2026-08-11T00:30:00Z") {
		t.Errorf("the dispatch note does not carry the fire time in UTC: %q", body)
	}
}

// TestAMissingInProgressColumnIsRefusedBeforeAnythingIsWritten. The board is
// created by whoever got there first and its columns are not guaranteed. Reading
// the column id before any write means a board missing the column leaves the
// card exactly where it was; discovering it after the link was attached would
// leave a card that is linked to a live session and still sitting in Queued,
// which the next tick dispatches again.
func TestAMissingInProgressColumnIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	d := &dispatchRecorder{}
	qc := d.start(t)
	qc.ColumnsByName = map[string]string{"Queued": "col-queued"}

	err := markQueueCardDispatched(qc, dispatchedCard(), dispatchProvider, "sess-1", time.Unix(1754784000, 0))
	if err == nil {
		t.Fatal("no error from a board with no In Progress column")
	}
	if len(d.links) != 0 || len(d.bodies) != 0 {
		t.Errorf("wrote to the card before checking the column existed: links=%v bodies=%v",
			d.links, d.bodies)
	}
}

// TestAFailedSideEffectIsReturnedRatherThanSwallowed. main logs this error and
// carries on, which is the right call — the session is already published and
// cannot be unpublished — but only because the error arrives. A nil return here
// makes a card that was never moved indistinguishable from one that was.
func TestAFailedSideEffectIsReturnedRatherThanSwallowed(t *testing.T) {
	for _, failOn := range []string{"/links", "/move"} {
		t.Run(failOn, func(t *testing.T) {
			d := &dispatchRecorder{failOn: failOn}
			qc := d.start(t)

			err := markQueueCardDispatched(qc, dispatchedCard(), dispatchProvider, "sess-1", time.Now())
			if err == nil {
				t.Fatalf("a %s that returned 500 was reported as a clean dispatch", failOn)
			}
		})
	}
}
