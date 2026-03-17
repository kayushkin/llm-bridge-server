package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestCalendarStub(t *testing.T) {
	srv := setup(t, "")
	req := httptest.NewRequest("GET", "/api/calendar/events", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}
