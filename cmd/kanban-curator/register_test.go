package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// -register is how this binary gets onto the cron schedule at all, and it is run
// by hand. Ported 2026-09-25 from test/first-tests-for-curator-and-dispatcher
// (6767efd1, 86b6dc8f) onto main, where the board is pinned by id and the
// scheduler URL is a parameter rather than an environment lookup.

// stubScheduler is the scheduler reduced to the two routes -register touches,
// plus a record of what it was asked to create.
type stubScheduler struct {
	existing []string // names of jobs already registered
	refuse   bool     // answer POST /api/jobs with 500
	posted   []map[string]any
	url      string
}

func newStubScheduler(t *testing.T, s *stubScheduler) *stubScheduler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/jobs" && r.Method == http.MethodGet:
			rows := make([]map[string]any, 0, len(s.existing))
			for _, name := range s.existing {
				rows = append(rows, map[string]any{"name": name})
			}
			writeStubJSON(w, 200, rows)
		case r.URL.Path == "/api/jobs" && r.Method == http.MethodPost:
			if s.refuse {
				http.Error(w, "the scheduler refused it", http.StatusInternalServerError)
				return
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("POST /api/jobs body is not JSON: %v", err)
			}
			s.posted = append(s.posted, payload)
			writeStubJSON(w, 201, map[string]any{"id": 99})
		default:
			t.Errorf("stub scheduler: unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *stubScheduler) only(t *testing.T) map[string]any {
	t.Helper()
	if len(s.posted) != 1 {
		t.Fatalf("registered %d jobs, want exactly 1: %v", len(s.posted), s.posted)
	}
	return s.posted[0]
}

// The command must carry -board-id: the scheduler runs it with `sh -c` and its
// own thin environment, and main refuses to start without the flag. So a job
// registered without it runs on schedule and fails every tick.
func TestRegisteringPostsAFiveMinuteShellJobThatNamesTheBoardByID(t *testing.T) {
	s := newStubScheduler(t, &stubScheduler{})

	board := &kanbanclient.Board{ID: "board-7", Name: "Task completion loop"}
	if err := registerSchedulerJob(s.url, board, ""); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	job := s.only(t)
	if job["name"] != "kanban-curator-task-completion-loop-tick" {
		t.Errorf("name = %q, want kanban-curator-task-completion-loop-tick", job["name"])
	}
	if job["schedule"] != "*/5 * * * *" {
		t.Errorf("schedule = %q; the curator's whole design assumes a five-minute tick", job["schedule"])
	}
	if job["type"] != "shell" {
		t.Errorf("type = %q, want shell", job["type"])
	}
	command, _ := job["command"].(string)
	if !strings.HasSuffix(command, "/bin/kanban-curator -board-id board-7") {
		t.Errorf("command = %q, want the curator binary pinned to board-7", command)
	}
}

// -job-name exists for job 17, which was named by hand and cannot be derived. A
// flag that is read but not used registers it under the derived name instead.
func TestAnExplicitJobNameOverridesTheDerivedOne(t *testing.T) {
	s := newStubScheduler(t, &stubScheduler{})

	board := &kanbanclient.Board{ID: "board-7", Name: "Agent runs"}
	if err := registerSchedulerJob(s.url, board, "kanban-curator-tcl-tick"); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	if job := s.only(t); job["name"] != "kanban-curator-tcl-tick" {
		t.Errorf("name = %q, want the name that was passed in", job["name"])
	}
}

// The scheduler does not reject a duplicate name, so this check is the only
// thing between a re-run of -register and one board curated twice per tick.
func TestAnAlreadyRegisteredJobIsNotRegisteredTwice(t *testing.T) {
	s := newStubScheduler(t, &stubScheduler{existing: []string{"kanban-curator-tick"}})

	board := &kanbanclient.Board{ID: "board-7", Name: "Agent runs"}
	if err := registerSchedulerJob(s.url, board, ""); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	if len(s.posted) != 0 {
		t.Errorf("registered a second job beside the one already there: %v", s.posted)
	}
}

// -register's exit status is all the operator sees. A refusal reported as
// success leaves a curator that was never scheduled.
func TestASchedulerThatRefusesTheJobIsReportedAsAnError(t *testing.T) {
	s := newStubScheduler(t, &stubScheduler{refuse: true})

	board := &kanbanclient.Board{ID: "board-7", Name: "Agent runs"}
	err := registerSchedulerJob(s.url, board, "")
	if err == nil {
		t.Fatal("no error from a scheduler that refused the job; -register would exit 0 having registered nothing")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error does not carry what the scheduler said: %v", err)
	}
}

// A slug is a cron job's identity: two boards that collapse to one slug share
// one job, and the second -register reports "already registered" for a board
// that has no curator at all.
func TestABoardNameBecomesAJobNameSlug(t *testing.T) {
	for _, check := range []struct{ board, want string }{
		{"Sprint 42", "sprint-42"},
		{"  --Weird__Name--  ", "weird-name"},
		{"a/b/c", "a-b-c"},
		{"!!!", ""},
	} {
		if got := slugify(check.board); got != check.want {
			t.Errorf("slugify(%q) = %q, want %q", check.board, got, check.want)
		}
	}
}
