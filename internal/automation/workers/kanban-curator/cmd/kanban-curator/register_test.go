package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
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
	// postStatus is the status an accepted POST /api/jobs answers with; zero
	// means 201. registerSchedulerJob reads anything >= 300 as a refusal, and a
	// stub that only ever answers 201 and 500 cannot tell that threshold from
	// any other value between them.
	postStatus int
	failList   int    // answer GET /api/jobs with this status instead of the job list
	listBody   string // answer GET /api/jobs 200 with this body instead of the job list
	posted     []map[string]any
	url        string
}

func newStubScheduler(t *testing.T, s *stubScheduler) *stubScheduler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/jobs" && r.Method == http.MethodGet:
			if s.failList != 0 {
				http.Error(w, "the scheduler could not list its jobs", s.failList)
				return
			}
			if s.listBody != "" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(s.listBody))
				return
			}
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
			status := s.postStatus
			if status == 0 {
				status = http.StatusCreated
			}
			writeStubJSON(w, status, map[string]any{"id": 99})
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

// The duplicate check reads the job list, so a list it could not read must stop
// -register. Read as "no jobs", a 500 lets a re-run register a second curator
// beside the first, and the board is then curated twice every tick.
func TestAJobListTheSchedulerCouldNotServeIsNotReadAsNoJobs(t *testing.T) {
	s := newStubScheduler(t, &stubScheduler{failList: http.StatusInternalServerError})

	board := &kanbanclient.Board{ID: "board-7", Name: "Agent runs"}
	err := registerSchedulerJob(s.url, board, "")
	if err == nil {
		t.Fatal("no error from a scheduler that could not list its jobs; -register went on as if none existed")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error does not carry the status the scheduler answered: %v", err)
	}
	if len(s.posted) != 0 {
		t.Errorf("registered a job without knowing whether one already exists: %v", s.posted)
	}
}

// A 200 whose body is not a job list is the same unknown as a 500: whatever
// answered, it did not say which jobs exist.
func TestAJobListThatIsNotAJSONArrayIsNotReadAsNoJobs(t *testing.T) {
	s := newStubScheduler(t, &stubScheduler{listBody: `{"error":"not a list"}`})

	board := &kanbanclient.Board{ID: "board-7", Name: "Agent runs"}
	err := registerSchedulerJob(s.url, board, "")
	if err == nil {
		t.Fatal("no error from a job list that did not decode; -register went on as if no jobs existed")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("the error does not say the job list failed to decode: %v", err)
	}
	if len(s.posted) != 0 {
		t.Errorf("registered a job without knowing whether one already exists: %v", s.posted)
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

// 300 is the first status registerSchedulerJob reads as a refusal, and the only
// one its threshold can be tested at: a check that slipped to 301 still rejects
// every 4xx and 5xx the other tests serve. Ported from 63a7f704.
func TestThreeHundredIsAlreadyARefusal(t *testing.T) {
	board := &kanbanclient.Board{ID: "board-7", Name: "Agent runs"}

	t.Run("300 is refused", func(t *testing.T) {
		s := newStubScheduler(t, &stubScheduler{postStatus: 300})
		if err := registerSchedulerJob(s.url, board, ""); err == nil {
			t.Error("no error from a scheduler answering 300; -register would exit 0 having registered nothing")
		}
	})

	t.Run("299 is accepted", func(t *testing.T) {
		s := newStubScheduler(t, &stubScheduler{postStatus: 299})
		if err := registerSchedulerJob(s.url, board, ""); err != nil {
			t.Errorf("a scheduler answering 299 was read as a refusal: %v", err)
		}
		s.only(t)
	})
}

// TestABoardNameBecomesAJobNameSlug proves both character classes survive, but
// "Sprint 42" never reaches '9' and no name in it has a 'z'. An end of either
// class that moves in by one drops a character from two boards' job names and
// can collapse them onto one job. Each row sits on one side of one end of a
// class. Ported from 63a7f704.
func TestTheSlugCharacterClassesRunToTheirLastCharacter(t *testing.T) {
	for _, check := range []struct{ board, want, why string }{
		{"Zone 9", "zone-9", "'z' and '9' are the last characters of the two classes"},
		{"Aim 0", "aim-0", "'a' and '0' are the first characters of the two classes"},
		{"x`y", "x-y", "'`' is the character before 'a' and must not be kept"},
		{"a{b", "a-b", "'{' is the character after 'z' and must not be kept"},
		{"1:2", "1-2", "':' is the character after '9' and must not be kept"},
		{"1/2", "1-2", "'/' is the character before '0' and must not be kept"},
	} {
		if got := slugify(check.board); got != check.want {
			t.Errorf("slugify(%q) = %q, want %q: %s", check.board, got, check.want, check.why)
		}
	}
}
