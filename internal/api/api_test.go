package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
)

func setup(t *testing.T, token string) *Server {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.NewWithDB(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sched := cron.New(store)
	return New(store, sched, token)
}

func TestAuthRequired(t *testing.T) {
	srv := setup(t, "secret")
	req := httptest.NewRequest("GET", "/api/jobs", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	// Verify structured error
	var apiErr APIError
	json.NewDecoder(w.Body).Decode(&apiErr)
	if apiErr.Error == "" {
		t.Fatal("expected structured error response")
	}
}

func TestCreateAndListJobs(t *testing.T) {
	srv := setup(t, "tok")
	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo hi"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	var jobs []db.Job
	json.NewDecoder(w.Body).Decode(&jobs)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
}

func TestCreateJobValidation(t *testing.T) {
	srv := setup(t, "")
	// Missing required fields
	body, _ := json.Marshal(map[string]string{"name": "j1"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetJob(t *testing.T) {
	srv := setup(t, "")
	// Create a job
	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo hi"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	// GET single job
	req = httptest.NewRequest("GET", "/api/jobs/1", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var job db.Job
	json.NewDecoder(w.Body).Decode(&job)
	if job.Name != "j1" {
		t.Fatalf("expected j1, got %s", job.Name)
	}

	// GET nonexistent
	req = httptest.NewRequest("GET", "/api/jobs/999", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPatchJobEnabled(t *testing.T) {
	srv := setup(t, "")
	// Create
	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo hi"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Disable
	body, _ = json.Marshal(map[string]interface{}{"enabled": false})
	req = httptest.NewRequest("PATCH", "/api/jobs/1", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var job db.Job
	json.NewDecoder(w.Body).Decode(&job)
	if job.Enabled {
		t.Fatal("expected disabled")
	}

	// Enable
	body, _ = json.Marshal(map[string]interface{}{"enabled": true})
	req = httptest.NewRequest("PATCH", "/api/jobs/1", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	json.NewDecoder(w.Body).Decode(&job)
	if !job.Enabled {
		t.Fatal("expected enabled")
	}
}

// TestCalendarEvents pins the real /api/calendar/events behaviour. This
// endpoint used to be a 501 stub (see the old TestCalendarStub); commit
// be7ad73 implemented it, expanding each enabled job's cron expression into
// occurrences within [from, to].
func TestCalendarEvents(t *testing.T) {
	srv := setup(t, "")

	// A job that fires every minute produces occurrences in any window.
	body, _ := json.Marshal(map[string]string{"name": "cal-job", "schedule": "* * * * *", "command": "echo hi"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create job: %d %s", w.Code, w.Body.String())
	}

	from := time.Now().UTC()
	to := from.Add(time.Hour)
	url := "/api/calendar/events?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	req = httptest.NewRequest("GET", url, nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var events []struct {
		ID       string `json:"id"`
		JobID    int64  `json:"job_id"`
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(w.Body).Decode(&events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A per-minute job over a one-hour window yields roughly 60 occurrences.
	if len(events) == 0 {
		t.Fatal("expected calendar occurrences for the enabled job, got none")
	}
	if events[0].JobID != 1 || events[0].Name != "cal-job" {
		t.Fatalf("occurrence does not reference the created job: %+v", events[0])
	}
}

// TestCalendarEventsBadRange pins the structured 400 when 'to' is not after
// 'from' — the endpoint validates its window rather than returning a stub.
func TestCalendarEventsBadRange(t *testing.T) {
	srv := setup(t, "")
	from := time.Now().UTC()
	url := "/api/calendar/events?from=" + from.Format(time.RFC3339) + "&to=" + from.Add(-time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var apiErr APIError
	json.NewDecoder(w.Body).Decode(&apiErr)
	if apiErr.Status != 400 {
		t.Fatalf("expected structured 400 error, got %+v", apiErr)
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := setup(t, "secret")
	// Health should work without auth
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected ok status, got %v", resp["status"])
	}
}

func TestDeleteJob(t *testing.T) {
	srv := setup(t, "")
	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	req = httptest.NewRequest("DELETE", "/api/jobs/1", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	// Delete again → 404
	req = httptest.NewRequest("DELETE", "/api/jobs/1", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestStructuredErrors(t *testing.T) {
	srv := setup(t, "")
	// Invalid ID
	req := httptest.NewRequest("GET", "/api/jobs/abc", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var apiErr APIError
	json.NewDecoder(w.Body).Decode(&apiErr)
	if apiErr.Error == "" || apiErr.Status != 400 {
		t.Fatalf("expected structured error, got %+v", apiErr)
	}
}

func TestHoldAndRelease(t *testing.T) {
	srv := setup(t, "")

	// Create job
	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	// Hold by days
	body, _ = json.Marshal(map[string]interface{}{"days": 3})
	req = httptest.NewRequest("POST", "/api/jobs/1/hold", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("hold: %d %s", w.Code, w.Body.String())
	}
	var job db.Job
	json.NewDecoder(w.Body).Decode(&job)
	if job.HoldUntil == nil {
		t.Fatal("expected hold_until to be set")
	}

	// Verify in list
	req = httptest.NewRequest("GET", "/api/jobs", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var jobs []db.Job
	json.NewDecoder(w.Body).Decode(&jobs)
	if len(jobs) != 1 || jobs[0].HoldUntil == nil {
		t.Fatal("expected hold_until in list response")
	}

	// Release hold
	req = httptest.NewRequest("DELETE", "/api/jobs/1/hold", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	var releasedJob db.Job
	json.NewDecoder(w.Body).Decode(&releasedJob)
	if releasedJob.HoldUntil != nil {
		t.Fatalf("expected hold_until to be nil after release, got %v", releasedJob.HoldUntil)
	}
}

func TestHoldValidation(t *testing.T) {
	srv := setup(t, "")

	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// No days or until
	body, _ = json.Marshal(map[string]interface{}{})
	req = httptest.NewRequest("POST", "/api/jobs/1/hold", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	// Nonexistent job
	body, _ = json.Marshal(map[string]interface{}{"days": 1})
	req = httptest.NewRequest("POST", "/api/jobs/999/hold", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
