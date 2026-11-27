package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/noteboarditems"
)

// fleet stands up the three services one tick talks to — noteboard for the
// items, the bridge for the cost table, kanban-store for a card's session links
// and its hold. Every route answers the happy case by default; each test breaks
// exactly one, so the shortfall it asserts can only have come from the route it
// broke.
type fleet struct {
	items []noteboardItem
	cost  map[string]float64
	// links maps an item id to the session ids the board says worked it.
	links map[string][]string
	// otherLinks maps an item id to link rows served as written, after its
	// session links. They exist to be ignored: a card links to predictions,
	// notes and PRs through the same route.
	otherLinks map[string][]map[string]any

	// linksStatus, when not 200, is what the links route answers with. This is
	// the read that prices a subtree.
	linksStatus int
	// holdStatus, when not 200, is what the hold route answers with.
	holdStatus int
	// pageSizeCap truncates every page to this many rows regardless of the limit
	// asked for, which is how a test makes the read run to its safety bound.
	alwaysFullPages bool

	mu        sync.Mutex
	holdCalls []string
	// holdReasons maps an item id to the reason its hold was sent with.
	holdReasons map[string]string
	// linksAskedOf records the id of every card whose links were read.
	linksAskedOf []string
	// itemRequests records the offset of every page requested, in order.
	itemRequests []string
}

func (f *fleet) start(t *testing.T) config {
	t.Helper()
	if f.linksStatus == 0 {
		f.linksStatus = http.StatusOK
	}
	if f.holdStatus == 0 {
		f.holdStatus = http.StatusOK
	}

	noteboard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/items" {
			t.Errorf("stub noteboard got an unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		q := r.URL.Query()
		if q.Get("include_held") != "true" {
			t.Errorf("the guard asked noteboard for items without include_held=true; a held ancestor "+
				"missing from the tree dangles every edge below it (query %q)", r.URL.RawQuery)
		}
		f.mu.Lock()
		f.itemRequests = append(f.itemRequests, q.Get("offset"))
		f.mu.Unlock()

		limit := atoiOr(q.Get("limit"), 0)
		offset := atoiOr(q.Get("offset"), 0)
		if f.alwaysFullPages {
			// A board that never runs out: every page comes back exactly full,
			// so the read can only stop at its own bound.
			page := make([]noteboardItem, 0, limit)
			for i := 0; i < limit; i++ {
				page = append(page, noteboardItem{ID: fmt.Sprintf("endless-%d", offset+i)})
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		page := []noteboardItem{}
		for i := offset; i < offset+limit && i < len(f.items); i++ {
			page = append(page, f.items[i])
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(noteboard.Close)

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := []sessionAggregate{}
		for id, c := range f.cost {
			out = append(out, sessionAggregate{SessionID: id, CostUSD: c})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(bridge.Close)

	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/cards/")
		switch {
		case strings.HasSuffix(path, "/links"):
			id := strings.TrimSuffix(path, "/links")
			f.mu.Lock()
			f.linksAskedOf = append(f.linksAskedOf, id)
			f.mu.Unlock()
			if f.linksStatus != http.StatusOK {
				w.WriteHeader(f.linksStatus)
				_, _ = w.Write([]byte(`{"error":"board unavailable"}`))
				return
			}
			out := []map[string]any{}
			for _, sid := range f.links[id] {
				out = append(out, map[string]any{"entity_type": "session", "entity_ref": sid})
			}
			out = append(out, f.otherLinks[id]...)
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(path, "/hold"):
			id := strings.TrimSuffix(path, "/hold")
			var payload struct {
				Reason string `json:"reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.mu.Lock()
			f.holdCalls = append(f.holdCalls, id)
			if f.holdReasons == nil {
				f.holdReasons = map[string]string{}
			}
			f.holdReasons[id] = payload.Reason
			f.mu.Unlock()
			w.WriteHeader(f.holdStatus)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("stub board got an unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(board.Close)

	return config{kanbanURL: board.URL, bridgeURL: bridge.URL, noteboardURL: noteboard.URL}
}

func atoiOr(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func usd(v float64) *float64 { return &v }

// ceilingOwner is an item carrying a ceiling, with one session linked to it.
func ceilingOwner(id string, ceiling float64) noteboardItem {
	return noteboardItem{ID: id, Title: "capped task " + id, AutoHoldAtUSD: usd(ceiling)}
}

// TestAnUnpriceableSubtreeDegradesTheRun — the first of the two shortfalls that
// were already in the code and already logged, and exited 0 anyway.
//
// The guard cannot price a subtree whose card links it cannot read, so it does
// the right local thing and declines to park a task on a guess. What was wrong
// was only the verdict: a tick that enforced NO ceiling at all reported
// `success`, and is indistinguishable in run history from a tick where every
// task was comfortably under its cap.
func TestAnUnpriceableSubtreeDegradesTheRun(t *testing.T) {
	f := &fleet{
		items:       []noteboardItem{ceilingOwner("capped-1", 5)},
		linksStatus: http.StatusInternalServerError,
	}
	if got := run(f.start(t)); got != 1 {
		t.Errorf("a tick that could price nothing exited %d, want 1 so the scheduler records `error`", got)
	}
}

// TestAFailedHoldOnAnOverCeilingTaskDegradesTheRun is the sharpest of the three,
// and it is the write path this card's second rule points at.
//
// By this line the guard has already decided the task is out of money. The hold
// is the only thing that stops it being dispatched again, and the hold did not
// land. The card stays open, the autoworker keeps picking it up, and the run
// says `success`.
func TestAFailedHoldOnAnOverCeilingTaskDegradesTheRun(t *testing.T) {
	f := &fleet{
		items:      []noteboardItem{ceilingOwner("capped-1", 5)},
		links:      map[string][]string{"capped-1": {"sess-a"}},
		cost:       map[string]float64{"sess-a": 12.50},
		holdStatus: http.StatusNotFound,
	}
	cfg := f.start(t)
	if got := run(cfg); got != 1 {
		t.Errorf("a breach whose hold failed exited %d, want 1", got)
	}
	if len(f.holdCalls) != 1 {
		t.Fatalf("the hold was attempted %d times, want 1 — the fixture never reached the write path", len(f.holdCalls))
	}
}

// TestTheFailedHoldNamesTheTaskThatIsStillDispatchable. Scheduler run history
// keeps combined output and that is all a person gets. A summary that says a
// hold failed without saying WHICH task is over its cap cannot be acted on, and
// it is the one field a mutation can drop with every other assertion green.
func TestTheFailedHoldNamesTheTaskThatIsStillDispatchable(t *testing.T) {
	f := &fleet{
		items:      []noteboardItem{ceilingOwner("capped-1", 5)},
		links:      map[string][]string{"capped-1": {"sess-a"}},
		cost:       map[string]float64{"sess-a": 12.50},
		holdStatus: http.StatusNotFound,
	}
	out := captureLog(t, func() { run(f.start(t)) })
	report := shortfallReport(out)
	if report == "" {
		t.Fatalf("the run exited 1 but printed no shortfall report, so run history shows `error` with no reason:\n%s", out)
	}
	// Read the REPORT line, not the whole log. The Printf immediately above the
	// Record names the same task, so `strings.Contains(out, ...)` is answered by
	// a different line and stays green with the id dropped from the shortfall —
	// which is exactly what the scorer caught on the first run.
	if !strings.Contains(report, "capped-1") {
		t.Errorf("the shortfall report does not name the task that is still dispatchable: %q", report)
	}
}

// shortfallReport returns the one line degradations.Report writes, or "" if the
// run wrote none.
func shortfallReport(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "run degraded") {
			return line
		}
	}
	return ""
}

// TestATruncatedItemReadDegradesTheRun is the half that was live on 2026-08-29:
// the read asked for 10,000 items, the board held 10,309, and the guard built
// its tree out of the difference without anything saying so. A missing
// descendant subtracts its cost from its ancestor's total and a missing owner is
// never checked, so a short read can only price a ceiling too LOW.
//
// The bound is now loud rather than absent. This fixture never runs out of
// items, so the read can only stop at noteboarditems.MaxPages.
func TestATruncatedItemReadDegradesTheRun(t *testing.T) {
	f := &fleet{alwaysFullPages: true}
	out := captureLog(t, func() {
		if got := run(f.start(t)); got != 1 {
			t.Errorf("a tick built on a truncated item list exited %d, want 1", got)
		}
	})
	if report := shortfallReport(out); !strings.Contains(report, "incomplete") {
		t.Errorf("the shortfall report does not say the tree is incomplete: %q", report)
	}
	if len(f.itemRequests) != noteboarditems.MaxPages {
		t.Errorf("the read stopped after %d pages, want %d — the bound is not where the constant says it is",
			len(f.itemRequests), noteboarditems.MaxPages)
	}
}

// TestTheWholeItemListIsRead pins the repair rather than the report. The old
// read took one page and stopped; this asserts the guard walks to the end of a
// list longer than one page, that the last (short) page ends it, and that
// nothing in the middle is dropped.
func TestTheWholeItemListIsRead(t *testing.T) {
	var items []noteboardItem
	for i := 0; i < noteboarditems.PageSize*2+7; i++ {
		items = append(items, noteboardItem{ID: fmt.Sprintf("item-%d", i)})
	}
	// The very last item on the very last page owns a ceiling it has breached.
	// Under the old single-page read it was invisible; if it is held, the whole
	// list was read.
	items[len(items)-1] = ceilingOwner("last-row", 1)

	f := &fleet{
		items: items,
		links: map[string][]string{"last-row": {"sess-a"}},
		cost:  map[string]float64{"sess-a": 9},
	}
	cfg := f.start(t)
	if got := run(cfg); got != 0 {
		t.Errorf("a complete read of a healthy board exited %d, want 0", got)
	}
	if len(f.itemRequests) != 3 {
		t.Errorf("the guard made %d page requests for %d items at %d per page, want 3",
			len(f.itemRequests), len(items), noteboarditems.PageSize)
	}
	if len(f.holdCalls) != 1 || f.holdCalls[0] != "last-row" {
		t.Fatalf("the breached ceiling on the last page was not held: holds=%v", f.holdCalls)
	}
}

// TestAnItemHandedBackTwiceIsCountedOnce. Paging is not a snapshot: an item
// created between two pages pushes rows toward later pages and can hand the same
// id back on both. Counting it twice would put a phantom parent edge in the tree.
func TestAnItemHandedBackTwiceIsCountedOnce(t *testing.T) {
	var items []noteboardItem
	for i := 0; i < noteboarditems.PageSize; i++ {
		items = append(items, noteboardItem{ID: fmt.Sprintf("item-%d", i)})
	}
	// The overlap: page two opens with a row page one already handed back.
	items = append(items, items[noteboarditems.PageSize-1], noteboardItem{ID: "tail"})

	f := &fleet{items: items}
	got, truncated, err := noteboarditems.FetchEveryItemIncludingHeld(f.start(t).noteboardURL)
	if err != nil || truncated {
		t.Fatalf("noteboarditems.FetchEveryItemIncludingHeld(err=%v, truncated=%v)", err, truncated)
	}
	if len(got) != noteboarditems.PageSize+1 {
		t.Errorf("read %d items from a list of %d rows holding %d distinct ids",
			len(got), len(items), noteboarditems.PageSize+1)
	}
}

// TestAQuietTickIsNotAShortfall is what keeps the three tests above worth
// anything. Almost every real tick checks a handful of ceilings, finds them all
// comfortably under, and holds nothing — job 34 has done exactly that on 100 of
// its last 100 runs. If that recorded a shortfall the job would be red every
// five minutes and the red would carry no information, which is the same defect
// as the green it replaces, pointing the other way.
//
// Without this case a recorder that records unconditionally satisfies all three.
func TestAQuietTickIsNotAShortfall(t *testing.T) {
	f := &fleet{
		items: []noteboardItem{ceilingOwner("capped-1", 10)},
		links: map[string][]string{"capped-1": {"sess-a"}},
		cost:  map[string]float64{"sess-a": 0.42},
	}
	cfg := f.start(t)
	if got := run(cfg); got != 0 {
		t.Errorf("a tick where every ceiling was under budget exited %d, want 0", got)
	}
	if len(f.holdCalls) != 0 {
		t.Errorf("an under-budget task was held: %v", f.holdCalls)
	}
}

// TestAnAlreadyHeldTaskIsNotAShortfall — a task parked on a previous tick is the
// guard's own success from five minutes ago, not a failure now. It is skipped
// before it is priced, so it must not reach the recorder even when the board
// would refuse to price it.
func TestAnAlreadyHeldTaskIsNotAShortfall(t *testing.T) {
	held := "2026-08-29T19:00:00Z"
	item := ceilingOwner("capped-1", 5)
	item.HeldAt = &held
	f := &fleet{
		items:       []noteboardItem{item},
		linksStatus: http.StatusInternalServerError,
	}
	if got := run(f.start(t)); got != 0 {
		t.Errorf("a tick whose only capped task was already parked exited %d, want 0", got)
	}
}

// TestADryRunReportsTheBreachWithoutHoldingOrDegrading. -dry-run is how a person
// asks the guard what it would do. It must not write, and it must not report a
// shortfall for a hold it deliberately did not attempt.
func TestADryRunReportsTheBreachWithoutHoldingOrDegrading(t *testing.T) {
	f := &fleet{
		items: []noteboardItem{ceilingOwner("capped-1", 5)},
		links: map[string][]string{"capped-1": {"sess-a"}},
		cost:  map[string]float64{"sess-a": 12.50},
	}
	cfg := f.start(t)
	cfg.dryRun = true
	out := captureLog(t, func() {
		if got := run(cfg); got != 0 {
			t.Errorf("a dry run exited %d, want 0 — it declined to hold on purpose", got)
		}
	})
	if len(f.holdCalls) != 0 {
		t.Fatalf("-dry-run wrote to the board: %v", f.holdCalls)
	}
	if !strings.Contains(out, "WOULD HOLD") {
		t.Errorf("a dry run over a breached ceiling reported nothing:\n%s", out)
	}
}
