package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
)

// postJob sends a create request and returns the job the server reports back.
func postJob(t *testing.T, srv *Server, body map[string]any) db.Job {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var job db.Job
	if err := json.NewDecoder(w.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

// readJob fetches a job back through the API, so the assertion covers what a
// caller would actually see rather than the handler's own return value.
func readJob(t *testing.T, srv *Server, id int64) db.Job {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/jobs", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var jobs []db.Job
	if err := json.NewDecoder(w.Body).Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %d not in the list", id)
	return db.Job{}
}

// TestCreateAJobInTheOffPosition is the defect this whole change exists for.
// There used to be no way to express it: the request struct had no enabled
// field, so create-then-PATCH was the only path and the job was armed for the
// window between the two calls.
func TestCreateAJobInTheOffPosition(t *testing.T) {
	srv := setup(t, "")
	created := postJob(t, srv, map[string]any{
		"name": "off", "schedule": "* * * * *", "command": "echo", "enabled": false,
	})
	if created.Enabled {
		t.Error("the response says the job is enabled")
	}
	if readJob(t, srv, created.ID).Enabled {
		t.Error("the stored job is enabled")
	}
}

// TestCreateWithoutEnabledStillArmsTheJob — absent must keep meaning true, or
// adding the field would silently disable every caller that predates it.
func TestCreateWithoutEnabledStillArmsTheJob(t *testing.T) {
	srv := setup(t, "")
	created := postJob(t, srv, map[string]any{
		"name": "default", "schedule": "* * * * *", "command": "echo",
	})
	if !created.Enabled {
		t.Error("the response says the job is disabled")
	}
	if !readJob(t, srv, created.ID).Enabled {
		t.Error("the stored job is disabled")
	}
}

// TestCreateWithEnabledTrueArmsTheJob — the explicit true has to be honored
// too, not merely happen to match the default.
func TestCreateWithEnabledTrueArmsTheJob(t *testing.T) {
	srv := setup(t, "")
	created := postJob(t, srv, map[string]any{
		"name": "on", "schedule": "* * * * *", "command": "echo", "enabled": true,
	})
	if !created.Enabled {
		t.Error("the response says the job is disabled")
	}
	if !readJob(t, srv, created.ID).Enabled {
		t.Error("the stored job is disabled")
	}
}
