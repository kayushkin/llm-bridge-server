package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
	"github.com/slava/scheduler/internal/noteboarditems"
)

// Ported from e0f686a5 and 4e4a0b93 (test/first-tests-for-autoworker). Main has
// no internal/kanbanvocab, so its names are written as the literals they stand
// for, and selectQueuedCard takes its URLs from a config.
//
// selectQueuedCard is the whole dispatch decision for a queued card: which card,
// and how much it may cost. Both halves reach the outside world over HTTP, so the
// fakes below stand in for kanban-store, noteboard and llm-bridge.
//
// Nothing in these fixtures is seeded from a constant in the code under test. The
// board and column names, the hold key, the ceiling field and the link type are
// spelled out as the LITERALS the stores put on the wire. A fixture that read them from
// the same variable production reads would move with the code and pin nothing.

// ---------------------------------------------------------------- fake stores

// fakeKanban serves the two reads and two writes selectQueuedCard makes, and
// records the writes so a test can assert a card was actually parked.
type fakeKanban struct {
	mu sync.Mutex

	// queued is the Queued column, in placement order.
	queued []kanbanclient.CardView
	// inProgress is a second column, so a test can tell "read the right column"
	// from "read any column".
	inProgress []kanbanclient.CardView
	// links maps card id → its entity links.
	links map[string][]kanbanclient.CardLink

	// linksFail makes ListCardLinks return 500, which is how a spend that cannot
	// be read presents itself.
	linksFail bool

	held []string // card ids parked via HoldCard, in call order
}

func (f *fakeKanban) heldCards() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.held...)
}

func (f *fakeKanban) start(t *testing.T) *kanbanclient.Client {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/boards/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/cards") {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		view := kanbanclient.BoardView{
			Board: kanbanclient.Board{ID: "board-1", Name: "Autoworker queue"},
			Columns: []kanbanclient.ColumnView{
				{Column: kanbanclient.Column{ID: "col-queued", Name: "Queued"}, Cards: f.queued},
				{Column: kanbanclient.Column{ID: "col-wip", Name: "In Progress"}, Cards: f.inProgress},
			},
		}
		writeFakeJSON(w, view)
	})

	mux.HandleFunc("/api/cards/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/cards/")
		cardID, action, _ := strings.Cut(rest, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch action {
		case "links":
			if f.linksFail {
				http.Error(w, "links are unreadable", http.StatusInternalServerError)
				return
			}
			links := f.links[cardID]
			if links == nil {
				links = []kanbanclient.CardLink{}
			}
			writeFakeJSON(w, links)
		case "hold":
			f.held = append(f.held, cardID)
			writeFakeJSON(w, map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	qc := kanbanclient.New(srv.URL, "")
	qc.BoardID = "board-1"
	qc.ColumnsByName = map[string]string{
		"Queued":      "col-queued",
		"In Progress": "col-wip",
	}
	return qc
}

// serveNoteboardItems stands in for noteboard's /api/items and records the last
// query it was asked. The query matters: the parent chain has to be complete,
// which means held ancestors and every page of the list. It honours limit and
// offset as noteboard does, so a list longer than one page takes more than one
// request to read.
//
// It answers on the real route and 404s everything else. A catch-all handler
// answers whatever the client asks for, so it agrees with a client that has the
// path wrong — measured, and it is why a mutation of the aggregates route went
// unnoticed until these two fixtures were narrowed.
func serveNoteboardItems(t *testing.T, items []map[string]any) (url string, lastQuery *string) {
	t.Helper()
	var mu sync.Mutex
	seen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/items" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		seen = r.URL.RawQuery
		mu.Unlock()
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page := []map[string]any{}
		for i := offset; i < offset+limit && i < len(items); i++ {
			page = append(page, items[i])
		}
		writeFakeJSON(w, page)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

// serveEndlessNoteboardItems is a board that never runs out: every page comes
// back exactly full, so the read can only stop at its own page bound. The first
// page holds the given items; the rest is filler.
func serveEndlessNoteboardItems(t *testing.T, items []map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/items" {
			http.NotFound(w, r)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page := make([]map[string]any, 0, limit)
		for i := offset; i < offset+limit; i++ {
			if i < len(items) {
				page = append(page, items[i])
				continue
			}
			page = append(page, map[string]any{"id": fmt.Sprintf("filler-%d", i)})
		}
		writeFakeJSON(w, page)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func serveBridgeAggregates(t *testing.T, costs map[string]float64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/aggregates" {
			http.NotFound(w, r)
			return
		}
		aggs := make([]sessionAggregate, 0, len(costs))
		for id, cost := range costs {
			aggs = append(aggs, sessionAggregate{SessionID: id, CostUSD: cost})
		}
		writeFakeJSON(w, aggs)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func serveBroken(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// card builds a CardView. The item is a raw map because that is what the board
// view actually hands us — an opaque noteboard item.
func card(id string, item map[string]any) kanbanclient.CardView {
	var cv kanbanclient.CardView
	cv.Placement.CardID = id
	cv.Placement.BoardID = "board-1"
	cv.Placement.ColumnID = "col-queued"
	cv.Item = item
	return cv
}

// pointAtFakes builds the config selectQueuedCard reads the two spend URLs from.
// On the branch these tests came from, selectQueuedCard read them from the
// environment; on main they are config fields.
func pointAtFakes(t *testing.T, bridgeURL, noteboardURL string) config {
	t.Helper()
	return config{llmBridgeURL: bridgeURL, noteboardURL: noteboardURL}
}

// ------------------------------------------------------------- the hold gate

// The hold gate, and the reason it has to exist twice.
//
// Noteboard withholds held items from its own discovery path, so the
// noteboard-reading worker cannot walk through the gate by accident. The kanban
// board view does the OPPOSITE on purpose — it hydrates held cards, because a
// card the user cannot see is a card they cannot press play on. So the queue path
// has to apply the gate again, by hand, and this is the test that says so.
func TestAHeldCardIsNeverDispatched(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("held-card", map[string]any{
			"title":       "Something the user parked",
			"held_at":     "2026-08-08T00:00:00Z",
			"hold_reason": "waiting on me",
		}),
	}}
	qc := f.start(t)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), func() string { u, _ := serveNoteboardItems(t, nil); return u }())

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got != nil {
		t.Fatalf("dispatched %q, a card the user had HELD", got.cardID)
	}
}

// The gate skips the held card and keeps looking. Stopping at the first held card
// would let one parked card block the whole queue behind it.
func TestAHeldCardIsSkippedAndTheNextOneIsTaken(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("held-card", map[string]any{"title": "parked", "held_at": "2026-08-08T00:00:00Z"}),
		card("live-card", map[string]any{"title": "ready to go"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, nil)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("a held card at the head of the queue blocked the live card behind it")
	}
	if got.cardID != "live-card" {
		t.Errorf("dispatched %q, want live-card", got.cardID)
	}
}

// An unheld card must still be dispatchable. Without this, a gate that rejected
// everything would pass the two assertions above and dispatch nothing, forever.
func TestAnUnheldCardIsDispatched(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("live-card", map[string]any{"title": "ready to go", "body": "do the thing"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, nil)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("an unheld card with a title was not dispatched")
	}
	if got.title != "ready to go" || got.body != "do the thing" {
		t.Errorf("card fields did not survive selection: %+v", got)
	}
}

// ------------------------------------------------------ the rest of selection

func TestSelectionReadsTheQueuedColumn(t *testing.T) {
	f := &fakeKanban{
		queued:     []kanbanclient.CardView{card("queued-card", map[string]any{"title": "waiting"})},
		inProgress: []kanbanclient.CardView{card("wip-card", map[string]any{"title": "already running"})},
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, nil)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil || got.cardID != "queued-card" {
		t.Fatalf("selection took %v; a card already In Progress has a session on it "+
			"and re-dispatching it doubles the work and the bill", got)
	}
}

func TestSelectionTakesTheHeadOfTheColumn(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("first", map[string]any{"title": "first"}),
		card("second", map[string]any{"title": "second"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, nil)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, _ := selectQueuedCard(qc, cfg, nil)
	if got == nil || got.cardID != "first" {
		t.Fatalf("selection did not take the head of the column in placement order: %v", got)
	}
}

func TestACardWithNoNoteboardItemIsSkipped(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("orphan", nil),
		card("real", map[string]any{"title": "real work"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, nil)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, _ := selectQueuedCard(qc, cfg, nil)
	if got == nil || got.cardID != "real" {
		t.Fatalf("a card with no noteboard item has no task in it; got %v", got)
	}
}

func TestACardWithABlankTitleIsSkipped(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("blank", map[string]any{"title": "   "}),
		card("real", map[string]any{"title": "real work"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, nil)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, _ := selectQueuedCard(qc, cfg, nil)
	if got == nil || got.cardID != "real" {
		t.Fatalf("a card with a whitespace title was dispatched: %v", got)
	}
}

func TestAnEmptyQueueDispatchesNothing(t *testing.T) {
	qc := (&fakeKanban{}).start(t)
	got, err := selectQueuedCard(qc, config{}, nil)
	if err != nil {
		t.Fatalf("an empty queue is not an error: %v", err)
	}
	if got != nil {
		t.Fatalf("an empty queue produced a card: %v", got)
	}
}

// ------------------------------------------------------- the spend ceiling gate

// A card that has already spent its ceiling is PARKED, not merely skipped.
// Skipping alone re-examines it on every tick forever, and the user never learns
// their ceiling stopped the work.
func TestASpentOutCardIsHeldNotJustSkipped(t *testing.T) {
	f := &fakeKanban{
		queued: []kanbanclient.CardView{
			card("spent", map[string]any{"title": "expensive", "auto_hold_at_usd": 5.0}),
		},
		links: map[string][]kanbanclient.CardLink{
			"spent": {{CardID: "spent", EntityType: "session", EntityRef: "sess-1"}},
		},
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{
		{"id": "spent", "auto_hold_at_usd": 5.0},
	})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, map[string]float64{"sess-1": 6.00}), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got != nil {
		t.Fatalf("dispatched %q, which has spent $6.00 of a $5.00 ceiling", got.cardID)
	}
	if held := f.heldCards(); len(held) != 1 || held[0] != "spent" {
		t.Errorf("the spent-out card was skipped but not parked (held=%v); it will be "+
			"re-examined on every tick and the user is never told", held)
	}
}

// The dispatched cap is (ceiling - what the whole tree has already cost). Handing
// the harness the full ceiling instead lets one task spend it twice.
func TestTheDispatchedCapIsTheCeilingMinusWhatWasSpent(t *testing.T) {
	f := &fakeKanban{
		queued: []kanbanclient.CardView{
			card("task", map[string]any{"title": "work", "auto_hold_at_usd": 10.0}),
		},
		links: map[string][]kanbanclient.CardLink{
			"task": {{CardID: "task", EntityType: "session", EntityRef: "sess-1"}},
		},
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{{"id": "task", "auto_hold_at_usd": 10.0}})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, map[string]float64{"sess-1": 4.00}), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("a card with $6.00 left of its ceiling was not dispatched")
	}
	if got.maxBudgetUSD != 6.00 {
		t.Errorf("dispatched cap $%.2f, want $6.00 ($10.00 ceiling less $4.00 already spent)",
			got.maxBudgetUSD)
	}
}

// The pot belongs to the ancestor that set the ceiling. Measuring spend over the
// CHILD's subtree gives every sibling the full pot, so "$10 for this task"
// quietly becomes $10 per branch.
func TestSpendIsCountedOverTheCeilingOwnersWholeTree(t *testing.T) {
	parentID := "parent"
	f := &fakeKanban{
		queued: []kanbanclient.CardView{
			card("child-b", map[string]any{"title": "second child"}),
		},
		links: map[string][]kanbanclient.CardLink{
			// The spend sits on the SIBLING, not on the card being dispatched.
			"child-a": {{CardID: "child-a", EntityType: "session", EntityRef: "sess-a"}},
		},
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{
		{"id": parentID, "auto_hold_at_usd": 10.0},
		{"id": "child-a", "parent_id": parentID},
		{"id": "child-b", "parent_id": parentID},
	})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, map[string]float64{"sess-a": 7.00}), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("child-b has $3.00 of the family pot left and should have dispatched")
	}
	if got.maxBudgetUSD != 3.00 {
		t.Errorf("dispatched cap $%.2f, want $3.00 — the $7.00 its SIBLING spent comes "+
			"out of the same $10.00 pot", got.maxBudgetUSD)
	}
}

// The ceiling is inherited. A child that sets none of its own is still covered by
// the nearest ancestor that does — otherwise splitting a task walks around the
// cap the user put on it, which is precisely the split this binary's own prompt
// asks a worker to perform.
func TestAChildInheritsItsAncestorsCeiling(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("child", map[string]any{"title": "split out of the parent"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{
		{"id": "grandparent", "auto_hold_at_usd": 12.0},
		{"id": "parent", "parent_id": "grandparent"},
		{"id": "child", "parent_id": "parent"},
	})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("the child was not dispatched at all")
	}
	if got.maxBudgetUSD != 12.00 {
		t.Errorf("dispatched cap $%.2f, want $12.00 inherited from the grandparent — a "+
			"child that escapes its ancestor's ceiling is how a split walks around a cap",
			got.maxBudgetUSD)
	}
}

// A held ancestor still owns its subtree's pot, so the item fetch has to ask for
// held items. Noteboard hides them by default, and a parent missing from the tree
// takes its ceiling with it.
func TestTheItemFetchAsksForHeldAncestors(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("child", map[string]any{"title": "child of a parked parent"}),
	}}
	qc := f.start(t)
	nb, query := serveNoteboardItems(t, []map[string]any{
		{"id": "parent", "auto_hold_at_usd": 9.0, "held_at": "2026-08-08T00:00:00Z"},
		{"id": "child", "parent_id": "parent"},
	})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil || got.maxBudgetUSD != 9.00 {
		t.Errorf("the held parent's $9.00 ceiling did not reach its child: %v", got)
	}
	if !strings.Contains(*query, "include_held=true") {
		t.Errorf("item query %q does not ask for held items; a held ancestor still owns "+
			"its subtree's ceiling, and noteboard hides held items by default", *query)
	}
}

// A ceiling owner older than the first page is still the child's ceiling owner.
// The fetch used to be one request for limit=10000; measured 2026-09-27 the
// board held 13,042 items, so the oldest ~3,000 never reached the tree, and
// EffectiveCeiling reads a missing ancestor as one that sets no ceiling. The
// child then went out with no --max-budget-usd at all.
func TestACeilingOwnerPastTheFirstPageStillCapsItsChild(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("child", map[string]any{"title": "child of an old parent"}),
	}}
	qc := f.start(t)
	items := []map[string]any{{"id": "child", "parent_id": "old-parent"}}
	for i := 0; i < noteboarditems.PageSize; i++ {
		items = append(items, map[string]any{"id": fmt.Sprintf("newer-%d", i)})
	}
	items = append(items, map[string]any{"id": "old-parent", "auto_hold_at_usd": 7.0})
	nb, _ := serveNoteboardItems(t, items)
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil || got.maxBudgetUSD != 7.00 {
		t.Errorf("the $7.00 ceiling on the second page did not reach its child: %v", got)
	}
}

// When the read stops at its page bound, the autoworker must NOT dispatch — not
// even a card with no ceiling of its own, because its owner may be past the
// bound. Asserting only that the read is complete would leave this unpinned: the
// guard reports a truncated read, the autoworker has to refuse to act on one.
func TestATruncatedItemReadDispatchesNothing(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("child", map[string]any{"title": "no ceiling of its own"}),
	}}
	qc := f.start(t)
	nb := serveEndlessNoteboardItems(t, []map[string]any{{"id": "child", "parent_id": "unseen-parent"}})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got != nil {
		t.Fatalf("dispatched %q with cap $%.2f from an item read that stopped at its page bound; "+
			"its ceiling owner could be past the bound, so this may be uncapped work", got.cardID, got.maxBudgetUSD)
	}
}

// The spend tree has TWO upstreams and the test below only ever fails one of
// them. loadSpendTree reads noteboard first and returns on its error, so
// pointing both at a broken server exercises the noteboard leg and leaves the
// bridge leg unreached — measured, and it is why the status guard inside
// fetchSessionAggregates could be deleted with the whole suite still green.
//
// The bridge is the half that carries the money. Noteboard supplies the
// ceilings; the bridge supplies what has already been spent against them. A
// bridge failure read as a successful empty reading says every card has spent
// $0.00, which uncaps every capped card at once and dispatches each one with its
// full ceiling — the one direction here that spends real money.
//
// The fixture answers a NON-200 carrying a well-formed empty array on purpose. A
// 500 with an error page is caught by the decode either way, so it cannot tell
// the guard from its absence; a service that answers a valid body with a failing
// status is the case where the status check is the only thing standing between
// "I could not read the spend" and "nothing has been spent".
func TestABridgeThatCannotReportSpendHoldsCappedWork(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("[]"))
	}))
	t.Cleanup(bridge.Close)

	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("capped", map[string]any{"title": "capped work", "auto_hold_at_usd": 5.0}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{{"id": "capped", "auto_hold_at_usd": 5.0}})
	cfg := pointAtFakes(t, bridge.URL, nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got != nil {
		t.Fatalf("dispatched capped card %q against a bridge that could not say what had "+
			"been spent; a %d carrying a valid empty body was read as $0.00 spent, so the "+
			"card went out with its whole $5.00 ceiling", got.cardID, http.StatusServiceUnavailable)
	}
}

// Fail closed, but only for work that has a ceiling to honour. An unreadable
// spend tree must not park work that never had one — that would turn a
// noteboard outage into a frozen queue.
func TestAnUnreadableTreeHoldsCappedWorkAndPassesUncappedWorkThrough(t *testing.T) {
	broken := serveBroken(t)

	t.Run("card with its own ceiling is held", func(t *testing.T) {
		f := &fakeKanban{queued: []kanbanclient.CardView{
			card("capped", map[string]any{"title": "capped work", "auto_hold_at_usd": 5.0}),
		}}
		qc := f.start(t)
		cfg := pointAtFakes(t, broken, broken)

		got, err := selectQueuedCard(qc, cfg, nil)
		if err != nil {
			t.Fatalf("selectQueuedCard: %v", err)
		}
		if got != nil {
			t.Fatalf("dispatched capped card %q uncapped while the spend tree was "+
				"unreadable — the one direction here that spends real money", got.cardID)
		}
	})

	t.Run("card with no ceiling is dispatched", func(t *testing.T) {
		f := &fakeKanban{queued: []kanbanclient.CardView{
			card("uncapped", map[string]any{"title": "work that never had a ceiling"}),
		}}
		qc := f.start(t)
		cfg := pointAtFakes(t, broken, broken)

		got, err := selectQueuedCard(qc, cfg, nil)
		if err != nil {
			t.Fatalf("selectQueuedCard: %v", err)
		}
		if got == nil {
			t.Fatal("a card that never had a ceiling was parked by an unreadable spend " +
				"tree; a store outage must not freeze work that has no cap to honour")
		}
	})
}

// The tree can be readable while the SPEND is not — the links live in a different
// store. A capped card whose spend cannot be read must be held, not dispatched
// against a ceiling nobody could measure.
func TestACappedCardWhoseSpendIsUnreadableIsHeld(t *testing.T) {
	f := &fakeKanban{
		queued: []kanbanclient.CardView{
			card("capped", map[string]any{"title": "capped work", "auto_hold_at_usd": 5.0}),
		},
		linksFail: true,
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{{"id": "capped", "auto_hold_at_usd": 5.0}})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got != nil {
		t.Fatalf("dispatched %q with a $5.00 ceiling whose spend could not be read",
			got.cardID)
	}
}

// Only session links are money. A card carries links of several types — the
// dispatch path alone attaches a session link and an autoworker_provider one —
// and a spend that counted all of them would bill a card for things that are not
// bills at all.
//
// The non-session link below points at a reference that HAS a cost, which no
// production link type does today. That is deliberate: the property under test is
// "the link type decides, not the reference", and a fixture whose two links share
// a reference proves nothing, because the sum deduplicates by reference and comes
// out the same either way. Measured — that was this test's first shape, and the
// mutation walked straight through it.
func TestOnlySessionLinksCountAsSpend(t *testing.T) {
	f := &fakeKanban{
		queued: []kanbanclient.CardView{
			card("task", map[string]any{"title": "work", "auto_hold_at_usd": 10.0}),
		},
		links: map[string][]kanbanclient.CardLink{
			"task": {
				{CardID: "task", EntityType: "session", EntityRef: "sess-1"},
				{CardID: "task", EntityType: "autoworker_provider", EntityRef: "not-a-session"},
			},
		},
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{{"id": "task", "auto_hold_at_usd": 10.0}})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, map[string]float64{
		"sess-1":        4.00,
		"not-a-session": 3.00,
	}), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("the card was not dispatched at all")
	}
	if got.maxBudgetUSD != 6.00 {
		t.Errorf("cap was $%.2f, want $6.00 ($10.00 less the $4.00 session). Only the "+
			"session link is a bill; the autoworker_provider link is not", got.maxBudgetUSD)
	}
}

// A card nothing has a ceiling for is dispatched with no cap of its own, leaving
// the fleet default (resolveSessionBudget) as the only thing covering it.
func TestACardWithNoCeilingAnywhereCarriesNoCapOfItsOwn(t *testing.T) {
	f := &fakeKanban{queued: []kanbanclient.CardView{
		card("free", map[string]any{"title": "no ceiling anywhere"}),
	}}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{{"id": "free"}})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, nil), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got == nil {
		t.Fatal("a card with no ceiling was not dispatched")
	}
	if got.maxBudgetUSD != 0 {
		t.Errorf("invented a cap of $%.2f for a card no ceiling covers", got.maxBudgetUSD)
	}
}

// ------------------------------------------------------------ cardSpendCeiling

// A missing key and a null both mean "no ceiling of its own", which says nothing
// about what the card may inherit. Reading either as a ceiling of zero would park
// every uncapped card on the board.
func TestCardSpendCeilingDistinguishesAbsentFromZero(t *testing.T) {
	for name, tc := range map[string]struct {
		item    map[string]any
		want    float64
		wantSet bool
	}{
		"absent":       {map[string]any{}, 0, false},
		"null":         {map[string]any{"auto_hold_at_usd": nil}, 0, false},
		"not a number": {map[string]any{"auto_hold_at_usd": "5.00"}, 0, false},
		"a real zero":  {map[string]any{"auto_hold_at_usd": 0.0}, 0, true},
		"a number":     {map[string]any{"auto_hold_at_usd": 5.0}, 5, true},
	} {
		got, ok := cardSpendCeiling(tc.item)
		if ok != tc.wantSet || got != tc.want {
			t.Errorf("%s: cardSpendCeiling = (%v, %v), want (%v, %v)",
				name, got, ok, tc.want, tc.wantSet)
		}
	}
}

// ------------------------------------------------------------ the worker prompt

// The card id is what the dispatched worker PATCHes to close the task. A prompt
// that loses it produces a session that does the work and cannot report it, and
// the card sits In Progress forever.
func TestTheQueuePromptCarriesTheCardIdAndTheTask(t *testing.T) {
	prompt := buildQueuePrompt(&queueCandidate{
		cardID: "card-abc-123",
		title:  "Rename the thing",
		body:   "It is called the wrong thing in four places.",
	})

	for _, want := range []string{
		"card-abc-123",
		"Rename the thing",
		"It is called the wrong thing in four places.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the dispatched prompt does not carry %q", want)
		}
	}
	if strings.Count(prompt, "card-abc-123") < 2 {
		t.Error("the card id appears once; the prompt needs it both as the task's " +
			"identity and in the URL the worker closes it with")
	}
}

func TestAnEmptyCardBodySaysSoRatherThanRenderingNothing(t *testing.T) {
	prompt := buildQueuePrompt(&queueCandidate{cardID: "c1", title: "Only a title", body: "   "})
	if !strings.Contains(prompt, "Only a title") {
		t.Fatal("the title did not reach the prompt")
	}
	if strings.Contains(prompt, "\n\n\n\n") {
		t.Error("an empty body left a hole in the prompt instead of saying there is none")
	}
}

func TestQueueCardLogIDNamesTheAbsenceOfACard(t *testing.T) {
	if got := queueCardLogID(nil); got == "" {
		t.Error("a fire with no queue card logged an empty id, which reads as a card " +
			"whose id failed to render")
	}
	if got := queueCardLogID(&queueCandidate{cardID: "c1"}); got != "c1" {
		t.Errorf("queueCardLogID = %q, want c1", got)
	}
}
