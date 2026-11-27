package schedulerjobs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeScheduler answers GET /api/jobs with listStatus and listBody, answers
// POST /api/jobs with createStatus, and records every request and posted body.
// Every test here talks to one of these, so nothing can reach the live
// scheduler, where a POST creates a real cron job.
type fakeScheduler struct {
	listStatus   int
	listBody     string
	createStatus int

	requests    []string
	contentType string
	posted      []map[string]any
	url         string
}

func newFakeScheduler(t *testing.T, listStatus int, listBody string, createStatus int) *fakeScheduler {
	t.Helper()
	fake := &fakeScheduler{listStatus: listStatus, listBody: listBody, createStatus: createStatus}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests = append(fake.requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
			w.WriteHeader(fake.listStatus)
			io.WriteString(w, fake.listBody)
		case r.Method == http.MethodPost && r.URL.Path == "/api/jobs":
			fake.contentType = r.Header.Get("Content-Type")
			var job map[string]any
			if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
				t.Errorf("POST body is not a JSON object: %v", err)
			}
			fake.posted = append(fake.posted, job)
			w.WriteHeader(fake.createStatus)
			io.WriteString(w, `{"id":7}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	fake.url = server.URL
	return fake
}

var testJob = Job{
	Name:        "test-tick",
	Schedule:    "*/5 * * * *",
	Description: "a job for tests",
	Command:     "/home/agent/bin/test -board-id board-1",
}

func TestAnUnregisteredJobIsPostedWithEveryField(t *testing.T) {
	job := testJob
	job.TimeoutSeconds = 900
	// test-tick-hourly starts with the name being registered, so a loose match
	// would read it as proof that test-tick is already there.
	fake := newFakeScheduler(t, http.StatusOK, `[{"name":"some-other-tick"},{"name":"test-tick-hourly"}]`, http.StatusCreated)

	if err := RegisterIfAbsent(fake.url, job); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}

	if got, want := strings.Join(fake.requests, ","), "GET /api/jobs,POST /api/jobs"; got != want {
		t.Errorf("requests = %s, want %s", got, want)
	}
	if fake.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", fake.contentType)
	}
	if len(fake.posted) != 1 {
		t.Fatalf("posted %d jobs, want 1", len(fake.posted))
	}
	for key, want := range map[string]any{
		"name":        "test-tick",
		"schedule":    "*/5 * * * *",
		"description": "a job for tests",
		"type":        "shell",
		"command":     "/home/agent/bin/test -board-id board-1",
		// JSON numbers decode as float64.
		"timeout_seconds": float64(900),
	} {
		if got := fake.posted[0][key]; got != want {
			t.Errorf("posted %s = %v, want %v", key, got, want)
		}
	}
}

// Zero means "the scheduler's default". Sending 0 explicitly would be the same
// to today's scheduler, but the binaries that never set a timeout never sent the
// key, and this keeps their payload byte-for-byte what it was.
func TestAZeroTimeoutIsLeftOutOfThePayload(t *testing.T) {
	if _, present := testJob.Payload()["timeout_seconds"]; present {
		t.Error("a job with no timeout sends timeout_seconds anyway")
	}
}

func TestAJobAlreadyRegisteredIsNotPostedAgain(t *testing.T) {
	fake := newFakeScheduler(t, http.StatusOK, `[{"name":"other"},{"name":"test-tick"}]`, http.StatusCreated)

	if err := RegisterIfAbsent(fake.url, testJob); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	if len(fake.posted) != 0 {
		t.Errorf("a job the scheduler already holds was posted again: %v", fake.posted)
	}
}

// The three ways the list read fails. Each used to be read, in four of the seven
// copies, as "no jobs registered", and the POST that followed added a second
// copy of a live cron job.
func TestAJobListThatCannotBeReadRegistersNothing(t *testing.T) {
	for _, check := range []struct {
		name       string
		listStatus int
		listBody   string
		wantInErr  string
	}{
		{"the scheduler answers 500", http.StatusInternalServerError, `{"error":"database is locked"}`, "500"},
		{"the scheduler answers 201 to a GET", http.StatusCreated, `[]`, "201"},
		{"the body is an object, not a list", http.StatusOK, `{"error":"x"}`, "decode"},
	} {
		t.Run(check.name, func(t *testing.T) {
			fake := newFakeScheduler(t, check.listStatus, check.listBody, http.StatusCreated)

			err := RegisterIfAbsent(fake.url, testJob)

			if err == nil {
				t.Fatal("want an error, got a registration")
			}
			if !strings.Contains(err.Error(), check.wantInErr) || !strings.Contains(err.Error(), "list jobs") {
				t.Errorf("error = %q, want it to say list jobs and %q", err, check.wantInErr)
			}
			if len(fake.posted) != 0 {
				t.Errorf("posted %v after a failed list read", fake.posted)
			}
		})
	}
}

// A list body cut off in transit is not a short list. The server here promises
// more bytes than it sends, so the read fails partway through a valid array.
func TestAJobListCutOffInTransitRegistersNothing(t *testing.T) {
	posted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted++
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `[]`)
	}))
	defer server.Close()

	err := RegisterIfAbsent(server.URL, testJob)

	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Errorf("error = %v, want a read body error", err)
	}
	if posted != 0 {
		t.Errorf("posted %d jobs after a list read that failed", posted)
	}
}

func TestAnUnreachableSchedulerIsAnError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	if err := RegisterIfAbsent(url, testJob); err == nil || !strings.Contains(err.Error(), "list jobs") {
		t.Errorf("error = %v, want a list jobs error", err)
	}
}

// 300 is the first status read as a refusal; 299 is the last read as success.
func TestAnyStatusFrom300UpIsARefusal(t *testing.T) {
	for _, check := range []struct {
		status    int
		wantError bool
	}{
		{http.StatusCreated, false},
		{299, false},
		{300, true},
		{http.StatusBadRequest, true},
		{http.StatusInternalServerError, true},
	} {
		fake := newFakeScheduler(t, http.StatusOK, `[]`, check.status)

		err := RegisterIfAbsent(fake.url, testJob)

		if gotError := err != nil; gotError != check.wantError {
			t.Errorf("create answered %d: error = %v, want an error: %v", check.status, err, check.wantError)
		}
		if err != nil && !strings.Contains(err.Error(), "scheduler returned") {
			t.Errorf("create answered %d: error = %q, want it to say scheduler returned", check.status, err)
		}
	}
}
