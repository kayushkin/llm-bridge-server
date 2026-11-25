package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJobReferenceAnswersTheJobAndItsNewestRun(t *testing.T) {
	srv := setup(t, "")
	body, _ := json.Marshal(map[string]string{"name": "j1", "schedule": "* * * * *", "command": "echo hi"})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	read := func(reference string) (int, map[string]any) {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/job-references/"+reference, nil))
		var answer map[string]any
		json.Unmarshal(w.Body.Bytes(), &answer)
		return w.Code, answer
	}

	code, answer := read("scheduler_job_000001")
	if code != 200 || answer["name"] != "j1" || answer["reference_id"] != "scheduler_job_000001" || answer["last_run"] != nil {
		t.Fatalf("before a run: %d %v", code, answer)
	}

	run, err := srv.store.CreateRun(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishRun(run.ID, "error", "a long output"); err != nil {
		t.Fatal(err)
	}
	code, answer = read("scheduler_job_000001")
	lastRun, _ := answer["last_run"].(map[string]any)
	if code != 200 || lastRun["status"] != "error" || lastRun["output"] != nil {
		t.Fatalf("after a run: %d %v", code, answer)
	}

	if code, _ := read("scheduler_job_000999"); code != 404 {
		t.Fatalf("unknown job: %d", code)
	}
	for _, malformed := range []string{"1", "scheduler_job_1", "job_000001"} {
		if code, _ := read(malformed); code != 400 {
			t.Fatalf("%s: %d", malformed, code)
		}
	}
}
