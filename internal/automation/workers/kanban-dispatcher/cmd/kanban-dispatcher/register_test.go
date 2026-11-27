package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Ported from 18ecd596 / f119a711 (test/the-mint-site-truncates-a-long-parent-id)
// onto main's registerSchedulerJob, which takes the scheduler URL and the board
// id as arguments; main passes the bridge's /scheduler.
//
// -register is the path an operator runs by hand on a deploy, and what it
// writes is a cron job that fires the binary that spends. A wrong cadence, a
// wrong command or a second copy of the job are each billed in agent runs.

// schedulerStub is the scheduler reduced to the two routes -register uses,
// plus a record of what was posted to it.
type schedulerStub struct {
	// existing is the job list GET /api/jobs answers with.
	existing []map[string]any
	// listStatus and listBody replace a healthy job list. listBody is served
	// verbatim, so a test can pair a well-formed body with a failing status —
	// the only shape that separates the status check from the decode.
	listStatus int
	listBody   string
	// postStatus is what POST /api/jobs answers, 201 when unset.
	postStatus int

	mu     sync.Mutex
	paths  []string
	posted []map[string]any
}

func (s *schedulerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
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
		w.Write([]byte(`{"id":99}`))
	default:
		http.Error(w, "scheduler stub: unexpected method", http.StatusMethodNotAllowed)
	}
}

func (s *schedulerStub) postedJobs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.posted...)
}

func registerAgainst(t *testing.T, s *schedulerStub) error {
	t.Helper()
	server := httptest.NewServer(s)
	defer server.Close()
	return registerSchedulerJob(server.URL, testBoardID)
}

// The fields are written as literals rather than read back off the payload the
// code built: a test that asks the code what it sent agrees with the code
// through its own drift.
func TestRegisterWritesTheDispatcherTickJobTheBoxExpects(t *testing.T) {
	s := &schedulerStub{}
	if err := registerAgainst(t, s); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}
	posted := s.postedJobs()
	if len(posted) != 1 {
		t.Fatalf("want exactly one job registered, got %d: %v", len(posted), posted)
	}
	job := posted[0]
	if got := job["name"]; got != "kanban-dispatcher-tick" {
		t.Errorf("job name = %v, want kanban-dispatcher-tick — the name is how the next -register finds it", got)
	}
	if got := job["schedule"]; got != "*/5 * * * *" {
		t.Errorf("schedule = %v, want */5 * * * * — every per-tick gate in the binary is sized to it", got)
	}
	if got := job["type"]; got != "shell" {
		t.Errorf("type = %v, want shell — the job runs a binary, not a prompt", got)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "bin", "kanban-dispatcher") + " -board-id " + testBoardID
	if got := job["command"]; got != want {
		t.Errorf("command = %v, want %q — this binary, with the board it dispatches on", got, want)
	}
}

func TestRegisterLeavesAnExistingTickAlone(t *testing.T) {
	s := &schedulerStub{existing: []map[string]any{{"name": "kanban-dispatcher-tick"}}}
	if err := registerAgainst(t, s); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}
	if posted := s.postedJobs(); len(posted) != 0 {
		t.Errorf("a tick already registered must not be registered again, posted %v", posted)
	}
}

// A job whose name only starts with the tick's name is a different job, and
// its presence proves nothing about the tick.
func TestRegisterIsNotSatisfiedByANeighboringJobName(t *testing.T) {
	s := &schedulerStub{existing: []map[string]any{{"name": "kanban-dispatcher-tick-hourly"}, {"name": "kanban-dispatcher"}}}
	if err := registerAgainst(t, s); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}
	if posted := s.postedJobs(); len(posted) != 1 {
		t.Errorf("a neighboring job name must not count as the tick, posted %v", posted)
	}
}

func TestRegisterReportsAJobTheSchedulerRefused(t *testing.T) {
	s := &schedulerStub{postStatus: http.StatusBadRequest}
	err := registerAgainst(t, s)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a refused registration must be an error naming the status, got %v", err)
	}
}

// An unreadable job list is not an empty one: reading it as empty registers a
// second tick, which dispatches every card twice.
func TestRegisterRefusesAJobListItCouldNotRead(t *testing.T) {
	cases := map[string]*schedulerStub{
		// A well-formed empty list under a failing status: only the status
		// check can refuse it.
		"answered 500":     {listStatus: http.StatusInternalServerError, listBody: `[]`},
		"not a JSON array": {listStatus: http.StatusOK, listBody: `<html>maintenance</html>`},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if err := registerAgainst(t, s); err == nil || !strings.Contains(err.Error(), "list jobs") {
				t.Errorf("an unreadable job list must stop -register with a list jobs error, got %v", err)
			}
			if posted := s.postedJobs(); len(posted) != 0 {
				t.Errorf("nothing may be registered after an unreadable job list, posted %v", posted)
			}
		})
	}
}

func TestRegisterReadsAndWritesTheSchedulersJobsRoute(t *testing.T) {
	s := &schedulerStub{}
	if err := registerAgainst(t, s); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}
	want := []string{"GET /api/jobs", "POST /api/jobs"}
	if strings.Join(s.paths, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", s.paths, want)
	}
}
