package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// Ported 2026-09-25 from 348e0589 (test/classifier-register-hole). When it was
// written, the `-register` entry point and requireColumns sat at 0.0% coverage:
// nobody had ever wired a test to a flag path.
//
// `-register` is run by hand on every deploy and its exit status is all the
// operator sees. What it writes is a cron job that fires the binary that pays
// for a model call per session, so a wrong cadence or a second copy of the job
// is billed in real API spend on a schedule.

// schedulerFixture serves s as the bridge's /scheduler and returns the
// configuration -register runs with, built by parseFlags from an environment
// naming the stub as the bridge, so the job is posted to the stub and never to
// the scheduler serving this box. registerSchedulerJob reads the address from
// cfg alone; there is no compiled-in route around the fixture.
func schedulerFixture(t *testing.T, s *schedulerStub) config {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/scheduler/", http.StripPrefix("/scheduler", s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg, err := parseFlags([]string{"-register"}, environmentWith(map[string]string{"LLM_BRIDGE_URL": srv.URL}), "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.BridgeURL != srv.URL {
		t.Fatalf("BridgeURL = %q, want the stub %q — the fixture would post to a real scheduler", cfg.BridgeURL, srv.URL)
	}
	return cfg
}

// schedulerStub is the scheduler reduced to the two routes -register uses.
type schedulerStub struct {
	existing []map[string]any
	// listStatus and listBody override a healthy job list. listBody is served
	// verbatim, so a test can hand back a well-formed body with a failing
	// status — the only shape that isolates a status guard from the JSON
	// decode standing behind it.
	listStatus int
	listBody   string
	postStatus int

	mu     sync.Mutex
	posted []map[string]any
}

func (s *schedulerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/jobs" {
		http.Error(w, "scheduler stub: unexpected path "+r.URL.Path, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if s.listStatus != 0 {
			w.WriteHeader(s.listStatus)
			w.Write([]byte(s.listBody))
			return
		}
		existing := s.existing
		if existing == nil {
			existing = []map[string]any{}
		}
		json.NewEncoder(w).Encode(existing)
	case http.MethodPost:
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		s.mu.Lock()
		s.posted = append(s.posted, payload)
		s.mu.Unlock()
		status := s.postStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"id":"job-99"}`))
	default:
		http.Error(w, "scheduler stub: unexpected method", http.StatusMethodNotAllowed)
	}
}

func (s *schedulerStub) postedJobs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.posted...)
}

// ============================ what it registers ============================

// Literals, not reads of the constants the code builds from. A test that asks
// the code what it sent agrees with the code through its own drift.
func TestRegisterWritesTheClassifierTickJobTheBoxExpects(t *testing.T) {
	s := &schedulerStub{}
	cfg := schedulerFixture(t, s)

	if err := registerSchedulerJob(cfg); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	posted := s.postedJobs()
	if len(posted) != 1 {
		t.Fatalf("want exactly one job registered, got %d: %v", len(posted), posted)
	}
	job := posted[0]

	if got := job["name"]; got != "kanban-classifier-tick" {
		t.Errorf("job name = %v, want kanban-classifier-tick — the name is what the "+
			"idempotence check reads back, so a drift here registers a second job on "+
			"every deploy", got)
	}
	// The cadence and the per-tick cap set the spend together: ten
	// classifications per tick, ninety-six ticks a day. A cadence drift
	// silently rescales the binary's whole budget.
	if got := job["schedule"]; got != "*/15 * * * *" {
		t.Errorf("job schedule = %v, want */15 * * * *", got)
	}
	if got := job["type"]; got != "shell" {
		t.Errorf("job type = %v, want shell", got)
	}
	if got := job["command"]; got != "/home/agent/bin/kanban-classifier" {
		t.Errorf("job command = %v, want /home/agent/bin/kanban-classifier", got)
	}
	if desc, _ := job["description"].(string); desc == "" {
		t.Error("job carries no description; the scheduler list is read by a human")
	}
}

func TestRegisterIsIdempotentSoADeployDoesNotStackTicks(t *testing.T) {
	s := &schedulerStub{existing: []map[string]any{{"name": "kanban-classifier-tick"}}}
	cfg := schedulerFixture(t, s)

	if err := registerSchedulerJob(cfg); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}
	if posted := s.postedJobs(); len(posted) != 0 {
		t.Errorf("the tick is already registered and -register posted anyway: %v", posted)
	}
}

// The loose-match direction, and it is the one that fails quietly. A comparison
// accepting any job whose name merely CONTAINS "kanban-classifier" would read a
// neighbor as proof the tick is registered, and the tick would then never be
// registered — an operator seeing "already registered" and a board that never
// fills.
func TestANeighboringJobIsNotMistakenForTheTick(t *testing.T) {
	s := &schedulerStub{existing: []map[string]any{
		{"name": "kanban-classifier-tick-hourly"},
		{"name": "kanban-dispatcher-tick"},
		{"name": "kanban-curator-tick"},
	}}
	cfg := schedulerFixture(t, s)

	if err := registerSchedulerJob(cfg); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}
	if posted := s.postedJobs(); len(posted) != 1 {
		t.Fatalf("no job named kanban-classifier-tick exists, so -register must register "+
			"one; posted %d: %v", len(posted), posted)
	}
}

// ============================ what it refuses ============================

func TestASchedulerRefusalIsReturnedRatherThanReportedAsRegistered(t *testing.T) {
	s := &schedulerStub{postStatus: http.StatusBadRequest}
	cfg := schedulerFixture(t, s)

	err := registerSchedulerJob(cfg)
	if err == nil {
		t.Fatal("the scheduler refused the job and -register exited 0, so the operator " +
			"was told the tick is registered when it is not")
	}
	// Assert the STATUS reaches the message, not merely that an error came
	// back. A client that has stopped reading status codes still errors here —
	// one step later and blaming something else — so `err != nil` alone stays
	// green on exactly the regression this pins.
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error does not carry the status the scheduler answered: %v", err)
	}
}

// The finding this file was written to close. The list is read to decide
// whether the job is ALREADY there, and both ways of failing to read it
// produced an empty list, which is indistinguishable from "not registered
// yet". The next statement then registers it.
func TestAnUnreadableJobListRefusesRatherThanRegisteringASecondTick(t *testing.T) {
	// A well-formed body with a failing status is the fixture that isolates
	// the status guard. A 500 carrying an error PAGE is caught by the JSON
	// decode whether the guard is there or not.
	s := &schedulerStub{listStatus: http.StatusInternalServerError, listBody: `[]`}
	cfg := schedulerFixture(t, s)

	err := registerSchedulerJob(cfg)
	if err == nil {
		t.Fatal("the job list came back 500 and -register registered a job anyway")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not carry the status the scheduler answered: %v", err)
	}
	if posted := s.postedJobs(); len(posted) != 0 {
		t.Errorf("posted a job on an unreadable list: %v", posted)
	}
}

func TestAJobListThatIsNotAnArrayRefusesRatherThanRegisteringASecondTick(t *testing.T) {
	s := &schedulerStub{listStatus: http.StatusOK, listBody: `{"error":"boom"}`}
	cfg := schedulerFixture(t, s)

	err := registerSchedulerJob(cfg)
	if err == nil {
		t.Fatal("the job list was not a JSON array and -register registered a job anyway")
	}
	if posted := s.postedJobs(); len(posted) != 0 {
		t.Errorf("posted a job on an undecodable list: %v", posted)
	}
}

// ============================ the lifecycle columns ============================

// requireColumns decides whether this binary runs at all. It used to end the
// process, which is why nothing could reach it; it now reports, and main still
// exits. The four names are the columns the model is allowed to place a card
// in, so a board missing one is a board where some classification has nowhere
// to go — and the failure has to be at startup rather than per card.
func TestABoardMissingALifecycleColumnIsNamedRatherThanDiscoveredPerCard(t *testing.T) {
	kc := issuesclient.New("http://unused")
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{
		"In Progress": "col-1",
		"Blocked":     "col-2",
		"Done":        "col-3",
		// Failed is absent.
	}

	err := requireColumns(kc, []string{"In Progress", "Blocked", "Done", "Failed"})
	if err == nil {
		t.Fatal("a board with no Failed column was accepted, so an errored session has " +
			"nowhere to be filed")
	}
	if !strings.Contains(err.Error(), "Failed") {
		t.Errorf("error does not name the missing column: %v", err)
	}
	if !strings.Contains(err.Error(), "board-1") {
		t.Errorf("error does not name the board it looked at: %v", err)
	}

	kc.ColumnsByName["Failed"] = "col-4"
	if err := requireColumns(kc, []string{"In Progress", "Blocked", "Done", "Failed"}); err != nil {
		t.Errorf("a complete board was rejected: %v", err)
	}
}

// A column present in the map but mapped to an empty id is the shape a
// half-built board has, and it is not the same as an absent key — Go answers
// "" to both, which is exactly why this is worth pinning rather than assuming.
func TestAColumnWithNoIDIsAsMissingAsAnAbsentOne(t *testing.T) {
	kc := issuesclient.New("http://unused")
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{"In Progress": ""}

	if err := requireColumns(kc, []string{"In Progress"}); err == nil {
		t.Fatal("a column mapped to an empty id was accepted; nothing can be moved to it")
	}
}
