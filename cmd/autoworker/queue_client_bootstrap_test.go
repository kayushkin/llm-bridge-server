package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
	"github.com/slava/scheduler/internal/kanbanvocab"
)

// initQueueClient was executed by no test at all — a panic() written at its
// first line left the whole suite green (measured by the 202nd nightly pass,
// card e9b0b89c). It is the function that decides whether an unattended tick
// may WRITE to kanban-store, so the value never exercised was the one that
// suppresses writes.
//
// A -dry-run fails two ways that look alike from outside:
//
//   - it writes anyway, which is the loud failure nobody sees at 3am;
//   - it writes nothing AND resolves nothing, which prints exactly what a
//     board with no work to do prints.
//
// So "no POST happened" is only half an assertion. Every dry-run case below
// pins the writes that must not happen AND the ids that must come back, and
// the three separate reasons this function answers nil are told apart by the
// line it logs — which, on a nil return, is the only thing an operator has.

// ============================ the fake store ============================

// fakeKanbanStore answers the four calls initQueueClient can reach and records
// every request, so a test can assert on what was NOT sent as well as what was.
type fakeKanbanStore struct {
	mu       sync.Mutex
	requests []string // "METHOD /path", in order

	// boards is the store's contents at the start of the test. A nil slice is
	// a fresh install: the queue board does not exist yet.
	boards []map[string]any
	// columnsByBoardID is what ListColumns answers for a given board.
	columnsByBoardID map[string][]map[string]any

	// failListBoards and failListColumns make those two calls answer 500, which
	// is how a kanban-store outage reaches this function.
	failListBoards   bool
	failListColumns  bool
	nextGeneratedID  int
	createdColumnsIn map[string][]map[string]any // board id -> column payloads received
}

func newFakeKanbanStore() *fakeKanbanStore {
	return &fakeKanbanStore{
		columnsByBoardID: map[string][]map[string]any{},
		createdColumnsIn: map[string][]map[string]any{},
	}
}

// withBoard seeds an existing board carrying the named columns.
func (f *fakeKanbanStore) withBoard(id, name string, columnNames ...string) *fakeKanbanStore {
	f.boards = append(f.boards, map[string]any{"id": id, "name": name})
	cols := []map[string]any{}
	for i, cn := range columnNames {
		cols = append(cols, map[string]any{
			"id": fmt.Sprintf("%s-col-%d", id, i), "board_id": id, "name": cn, "position": float64(i),
		})
	}
	f.columnsByBoardID[id] = cols
	return f
}

func (f *fakeKanbanStore) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
}

// writes returns only the requests that change the store. This is the set that
// must be empty on every dry-run path.
func (f *fakeKanbanStore) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, req := range f.requests {
		if !strings.HasPrefix(req, "GET ") {
			out = append(out, req)
		}
	}
	return out
}

func (f *fakeKanbanStore) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && path == "/api/boards":
			if f.failListBoards {
				http.Error(w, "kanban-store is having a night", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, f.boards)

		case r.Method == http.MethodPost && path == "/api/boards":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.mu.Lock()
			f.nextGeneratedID++
			id := fmt.Sprintf("created-board-%d", f.nextGeneratedID)
			name, _ := payload["name"].(string)
			f.boards = append(f.boards, map[string]any{"id": id, "name": name})
			f.columnsByBoardID[id] = []map[string]any{}
			f.mu.Unlock()
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name})

		case r.Method == http.MethodGet && strings.HasSuffix(path, "/columns"):
			if f.failListColumns {
				http.Error(w, "kanban-store is having a night", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, f.columnsByBoardID[boardIDFromColumnsPath(path)])

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/columns"):
			boardID := boardIDFromColumnsPath(path)
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.mu.Lock()
			f.nextGeneratedID++
			id := fmt.Sprintf("created-col-%d", f.nextGeneratedID)
			name, _ := payload["name"].(string)
			col := map[string]any{"id": id, "board_id": boardID, "name": name}
			f.columnsByBoardID[boardID] = append(f.columnsByBoardID[boardID], col)
			f.createdColumnsIn[boardID] = append(f.createdColumnsIn[boardID], payload)
			f.mu.Unlock()
			writeJSON(w, http.StatusCreated, col)

		default:
			// Reaching here means initQueueClient grew a call this fixture does
			// not model. Fail loudly rather than answering an empty 200, which
			// would look like a store with nothing in it.
			http.Error(w, "fake kanban-store has no route for "+r.Method+" "+path, http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// boardIDFromColumnsPath pulls the id out of /api/boards/{id}/columns.
func boardIDFromColumnsPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// ============================ dry-run: the writes that must not happen ============================

func TestADryRunNeverCreatesTheQueueBoard(t *testing.T) {
	store := newFakeKanbanStore() // fresh install: no boards at all
	url := store.start(t)

	var client *kanbanclient.Client
	logged := captureLog(t, func() { client, _ = initQueueClient(url, "", true) })

	if client != nil {
		t.Fatalf("dry-run against a store with no queue board returned a client; want nil")
	}
	if writes := store.writes(); len(writes) != 0 {
		t.Fatalf("dry-run wrote to kanban-store: %v", writes)
	}
	// The other half of the assertion: nil for this reason has to be readable.
	if !strings.Contains(logged, "does not exist yet") {
		t.Fatalf("a dry run against a fresh install must say the board is missing, logged: %q", logged)
	}
}

func TestADryRunResolvesTheBoardItFindsInsteadOfReportingNothing(t *testing.T) {
	store := newFakeKanbanStore().withBoard("queue-1", autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()...)
	url := store.start(t)

	client, _ := initQueueClient(url, "", true)

	if client == nil {
		t.Fatal("dry-run against an existing, fully-columned board returned nil")
	}
	if client.BoardID != "queue-1" {
		t.Fatalf("BoardID = %q, want %q", client.BoardID, "queue-1")
	}
	// "Wrote nothing and counted nothing" prints the same as a quiet board, so
	// the columns actually have to arrive.
	for _, want := range kanbanvocab.AutoworkerQueueColumns() {
		if _, ok := client.ColumnsByName[want]; !ok {
			t.Errorf("column %q missing from the resolved client; got %v", want, client.ColumnsByName)
		}
	}
	if writes := store.writes(); len(writes) != 0 {
		t.Fatalf("dry-run wrote to kanban-store: %v", writes)
	}
}

func TestADryRunDoesNotBackfillAMissingColumn(t *testing.T) {
	// A board that exists but predates a column the binary now expects. The
	// real run repairs it; the dry run must report it as it is.
	store := newFakeKanbanStore().withBoard("queue-1", autoworkerQueueBoard, "Queued")
	url := store.start(t)

	client, _ := initQueueClient(url, "", true)

	if client == nil {
		t.Fatal("dry-run against a partially-columned board returned nil")
	}
	if _, ok := client.ColumnsByName[autoworkerQueueColumnInProgress]; ok {
		t.Errorf("dry-run invented the %q column", autoworkerQueueColumnInProgress)
	}
	if writes := store.writes(); len(writes) != 0 {
		t.Fatalf("dry-run created the missing column: %v", writes)
	}
}

// ============================ the parameter's straddle pair ============================

// The one case that makes the dryRun parameter observable at all: the same
// store, the same absent board, the two values — one writes, one does not.
func TestTheSameEmptyStoreIsBootstrappedOnlyWhenNotDryRunning(t *testing.T) {
	dryStore := newFakeKanbanStore()
	dryClient, _ := initQueueClient(dryStore.start(t), "", true)

	realStore := newFakeKanbanStore()
	realClient, _ := initQueueClient(realStore.start(t), "", false)

	if dryClient != nil {
		t.Error("dry-run bootstrapped the board")
	}
	if realClient == nil {
		t.Fatal("a real run left the queue board uncreated")
	}
	if len(dryStore.writes()) != 0 {
		t.Errorf("dry-run wrote %v", dryStore.writes())
	}
	if len(realStore.writes()) == 0 {
		t.Error("a real run against a fresh install wrote nothing at all")
	}
}

// ============================ the real run ============================

func TestARealRunCreatesTheBoardAndEveryColumnItNeeds(t *testing.T) {
	store := newFakeKanbanStore()
	url := store.start(t)

	client, _ := initQueueClient(url, "", false)

	if client == nil {
		t.Fatal("bootstrapping a fresh install returned nil")
	}
	if client.BoardID == "" {
		t.Error("client came back with no BoardID")
	}
	// Spelled out rather than ranged over AutoworkerQueueColumns. A test that
	// reads the same constant it is meant to pin moves with it: dropping a
	// column from that slice deleted the expectation along with the behavior,
	// and this assertion passed while a fresh board came up missing the column
	// every dispatched card is moved to. Measured, not imagined — the row is in
	// scripts/sabotage-autoworker.py.
	for _, want := range []string{"Queued", "In Progress", "Blocked", "Done", "Failed"} {
		if _, ok := client.ColumnsByName[want]; !ok {
			t.Errorf("column %q was not created; got %v", want, client.ColumnsByName)
		}
	}
	// The two columns that carry an auto_status are the ones that close a card
	// when it lands. A column created without it is a card that never closes.
	autoStatusByColumn := map[string]string{}
	for _, payload := range store.createdColumnsIn[client.BoardID] {
		name, _ := payload["name"].(string)
		status, _ := payload["auto_status"].(string)
		autoStatusByColumn[name] = status
	}
	if got := autoStatusByColumn["Done"]; got != "done" {
		t.Errorf(`Done column created with auto_status %q, want "done"`, got)
	}
	if got := autoStatusByColumn["Failed"]; got != "archived" {
		t.Errorf(`Failed column created with auto_status %q, want "archived"`, got)
	}
}

// The two columns this binary later addresses by name are read from constants
// that have nothing to do with the set it creates, so the two can drift apart
// silently: a board bootstrapped without "In Progress" takes the dispatch, and
// markQueueCardDispatched then reports the column missing from a board it
// created itself. This ties the halves together at the one place both are
// visible.
func TestEveryColumnTheBinaryAddressesIsOneItCreates(t *testing.T) {
	store := newFakeKanbanStore()
	url := store.start(t)

	client, _ := initQueueClient(url, "", false)
	if client == nil {
		t.Fatal("bootstrapping a fresh install returned nil")
	}

	for _, addressed := range []string{autoworkerQueueColumnFrom, autoworkerQueueColumnInProgress} {
		if _, ok := client.ColumnsByName[addressed]; !ok {
			t.Errorf("the binary reads column %q but never creates it; created %v",
				addressed, client.ColumnsByName)
		}
	}
}

func TestARealRunAgainstAReadyBoardCreatesNothing(t *testing.T) {
	store := newFakeKanbanStore().withBoard("queue-1", autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()...)
	url := store.start(t)

	client, _ := initQueueClient(url, "", false)

	if client == nil {
		t.Fatal("a ready board returned nil")
	}
	if client.BoardID != "queue-1" {
		t.Errorf("BoardID = %q, want the board that already existed", client.BoardID)
	}
	if writes := store.writes(); len(writes) != 0 {
		t.Fatalf("a ready board was written to anyway: %v", writes)
	}
}

func TestARealRunBackfillsOnlyTheColumnsThatAreMissing(t *testing.T) {
	store := newFakeKanbanStore().withBoard("queue-1", autoworkerQueueBoard, "Queued", "In Progress")
	url := store.start(t)

	client, _ := initQueueClient(url, "", false)

	if client == nil {
		t.Fatal("a partially-columned board returned nil")
	}
	created := map[string]bool{}
	for _, payload := range store.createdColumnsIn["queue-1"] {
		name, _ := payload["name"].(string)
		created[name] = true
	}
	for _, want := range []string{"Blocked", "Done", "Failed"} {
		if !created[want] {
			t.Errorf("missing column %q was not backfilled", want)
		}
	}
	for _, alreadyThere := range []string{"Queued", "In Progress"} {
		if created[alreadyThere] {
			t.Errorf("column %q existed and was created a second time", alreadyThere)
		}
	}
}

// ============================ the outage paths ============================

// Three different failures all answer nil. The caller falls back to the default
// prompt in every one of them, so what separates them is the log line — and a
// wrong line here sends whoever reads it at 3am to the wrong service.

func TestAnUnreadableStoreIsReportedAsAnErrorAndNotAsAnAbsentBoard(t *testing.T) {
	store := newFakeKanbanStore()
	store.failListBoards = true
	url := store.start(t)

	for _, dryRun := range []bool{true, false} {
		var client *kanbanclient.Client
		logged := captureLog(t, func() { client, _ = initQueueClient(url, "", dryRun) })

		if client != nil {
			t.Errorf("dryRun=%v: a store that cannot list boards produced a usable client", dryRun)
		}
		if strings.Contains(logged, "does not exist yet") {
			t.Errorf("dryRun=%v: an outage was reported as a missing board: %q", dryRun, logged)
		}
		if !strings.Contains(logged, "500") {
			t.Errorf("dryRun=%v: the store's failure did not reach the log: %q", dryRun, logged)
		}
	}
}

func TestADryRunThatCannotListColumnsReturnsNothingRatherThanAHalfConfiguredClient(t *testing.T) {
	// The board resolves, the columns do not. Returning the client here would
	// hand the caller a BoardID with an empty column map, and the first
	// ColumnsByName lookup downstream reports the column as missing from the
	// board — blaming the board for an outage.
	store := newFakeKanbanStore().withBoard("queue-1", autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()...)
	store.failListColumns = true
	url := store.start(t)

	var client *kanbanclient.Client
	logged := captureLog(t, func() { client, _ = initQueueClient(url, "", true) })

	if client != nil {
		t.Fatalf("got a client with columns=%v; want nil", client.ColumnsByName)
	}
	if !strings.Contains(logged, "columns") {
		t.Errorf("the failing call was not named in the log: %q", logged)
	}
}

func TestAnUnreachableStoreIsSurvivedRatherThanFatal(t *testing.T) {
	// Nothing is listening on this port. kanban-store being down must not take
	// the tick with it: the fire path falls back to the default prompt.
	const nowhere = "http://127.0.0.1:1"

	for _, dryRun := range []bool{true, false} {
		if client, _ := initQueueClient(nowhere, "", dryRun); client != nil {
			t.Errorf("dryRun=%v: an unreachable store produced a client", dryRun)
		}
	}
}

// ============================ the board this binary asks for ============================

func TestTheQueueBoardIsResolvedByItsOwnNameAndNotTheTelemetryBoard(t *testing.T) {
	// Both boards exist and only one of them is the dispatch source. Resolving
	// the wrong one would dispatch cards from the "Agent runs" telemetry board,
	// which autoworker itself writes to — a feedback loop.
	store := newFakeKanbanStore().
		withBoard("telemetry", kanbanBoardName, kanbanvocab.AgentRunsColumns()...).
		withBoard("queue-1", autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()...)
	url := store.start(t)

	for _, dryRun := range []bool{true, false} {
		client, _ := initQueueClient(url, "", dryRun)
		if client == nil {
			t.Fatalf("dryRun=%v: returned nil with the queue board present", dryRun)
		}
		if client.BoardID != "queue-1" {
			t.Errorf("dryRun=%v: resolved board %q, want the queue board", dryRun, client.BoardID)
		}
	}
}
