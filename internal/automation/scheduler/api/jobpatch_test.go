package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
)

// createJob posts a job and returns it, failing the test if creation did not
// return 201.
func createJob(t *testing.T, srv *Server, body map[string]any) db.Job {
	t.Helper()
	encoded, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var job db.Job
	if err := json.NewDecoder(w.Body).Decode(&job); err != nil {
		t.Fatalf("decode created job: %v", err)
	}
	return job
}

func patchJob(t *testing.T, srv *Server, id int64, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, _ := json.Marshal(body)
	req := httptest.NewRequest("PATCH", "/api/jobs/"+strconv.FormatInt(id, 10), bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// TestPatchWritesEveryPatchableField is the regression test for the bug this
// endpoint shipped with: PATCH decoded into a struct carrying only enabled,
// name, description and timeout_seconds, dropped every other field, and still
// answered 200 — so an edited prompt reported success and changed nothing.
func TestPatchWritesEveryPatchableField(t *testing.T) {
	srv := setup(t, "")
	job := createJob(t, srv, map[string]any{
		"name": "agent-job", "schedule": "0 5 * * *",
		"type": "agent", "agent": "claude-code", "prompt": "old prompt",
	})

	w := patchJob(t, srv, job.ID, map[string]any{
		"name":            "renamed",
		"description":     "new description",
		"schedule":        "30 6 * * *",
		"type":            "agent",
		"command":         "echo hi",
		"agent":           "another-agent",
		"prompt":          "new prompt",
		"model_role":      "balanced",
		"orchestrator":    "inber",
		"session_id":      "sess-1",
		"workspace_id":    "ws-1",
		"timeout_seconds": 1800,
		"enabled":         false,
	})
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}

	var updated db.Job
	json.NewDecoder(w.Body).Decode(&updated)
	for _, field := range []struct {
		name string
		got  any
		want any
	}{
		{"name", updated.Name, "renamed"},
		{"description", updated.Description, "new description"},
		{"schedule", updated.Schedule, "30 6 * * *"},
		{"type", updated.Type, "agent"},
		{"command", updated.Command, "echo hi"},
		{"agent", updated.Agent, "another-agent"},
		{"prompt", updated.Prompt, "new prompt"},
		{"model_role", updated.ModelRole, "balanced"},
		{"orchestrator", updated.Orchestrator, "inber"},
		{"session_id", updated.SessionID, "sess-1"},
		{"workspace_id", updated.WorkspaceID, "ws-1"},
		{"timeout_seconds", updated.TimeoutSecs, 1800},
		{"enabled", updated.Enabled, false},
	} {
		if field.got != field.want {
			t.Errorf("%s = %v, want %v", field.name, field.got, field.want)
		}
	}
}

// TestPatchLeavesUnnamedFieldsAlone pins the other half of a partial update: a
// field the body does not mention keeps its value, and a field it sets to the
// zero value really is cleared. Both have to work, or "omitted" and "cleared"
// collapse into one meaning.
func TestPatchLeavesUnnamedFieldsAlone(t *testing.T) {
	srv := setup(t, "")
	job := createJob(t, srv, map[string]any{
		"name": "agent-job", "schedule": "0 5 * * *",
		"type": "agent", "agent": "claude-code", "prompt": "keep me", "model_role": "default",
	})

	w := patchJob(t, srv, job.ID, map[string]any{"model_role": ""})
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var updated db.Job
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.ModelRole != "" {
		t.Errorf("model_role = %q, want cleared", updated.ModelRole)
	}
	if updated.Prompt != "keep me" {
		t.Errorf("prompt = %q, want it untouched", updated.Prompt)
	}
	if !updated.Enabled {
		t.Error("enabled was flipped by a patch that never mentioned it")
	}
}

// TestPatchRejectsUnknownField is the second half of the reported bug: a field
// the API does not understand must 400, not be dropped behind a 200.
func TestPatchRejectsUnknownField(t *testing.T) {
	srv := setup(t, "")
	job := createJob(t, srv, map[string]any{"name": "j", "schedule": "* * * * *", "command": "echo"})

	w := patchJob(t, srv, job.ID, map[string]any{"promt": "typo"})
	if w.Code != 400 {
		t.Fatalf("expected 400 for unknown field, got %d %s", w.Code, w.Body.String())
	}
	var apiErr APIError
	json.NewDecoder(w.Body).Decode(&apiErr)
	if apiErr.Status != 400 {
		t.Fatalf("expected structured 400, got %+v", apiErr)
	}
}

func TestCreateRejectsUnknownField(t *testing.T) {
	srv := setup(t, "")
	body, _ := json.Marshal(map[string]any{
		"name": "j", "schedule": "* * * * *", "command": "echo", "timeout": 60,
	})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400 for unknown field 'timeout', got %d %s", w.Code, w.Body.String())
	}
}

// TestPatchRejectsInvalidSchedule guards the field most able to silently
// disable a job: an unparseable cron expression used to be accepted, then
// skipped with a log line at load time and never run again.
func TestPatchRejectsInvalidSchedule(t *testing.T) {
	srv := setup(t, "")
	job := createJob(t, srv, map[string]any{"name": "j", "schedule": "* * * * *", "command": "echo"})

	w := patchJob(t, srv, job.ID, map[string]any{"schedule": "not a cron"})
	if w.Code != 400 {
		t.Fatalf("expected 400 for invalid schedule, got %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/jobs/"+strconv.FormatInt(job.ID, 10), nil))
	var unchanged db.Job
	json.NewDecoder(w.Body).Decode(&unchanged)
	if unchanged.Schedule != "* * * * *" {
		t.Fatalf("rejected patch still wrote the schedule: %q", unchanged.Schedule)
	}
}

func TestCreateRejectsInvalidSchedule(t *testing.T) {
	srv := setup(t, "")
	body, _ := json.Marshal(map[string]any{"name": "j", "schedule": "every tuesday", "command": "echo"})
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400 for invalid schedule, got %d %s", w.Code, w.Body.String())
	}
}

// TestPatchRejectsJobEditedIntoAnUnrunnableState checks the validator runs on
// the merged result: clearing a shell job's command is only invalid because of
// the type the job already has, which a patch read on its own cannot see.
func TestPatchRejectsJobEditedIntoAnUnrunnableState(t *testing.T) {
	srv := setup(t, "")
	job := createJob(t, srv, map[string]any{"name": "j", "schedule": "* * * * *", "command": "echo"})

	for _, patch := range []map[string]any{
		{"command": ""},         // shell job with nothing to run
		{"type": "agent"},       // switched to agent with no agent/prompt
		{"type": "cron"},        // type outside the closed set
		{"name": ""},            // unnamed
		{"timeout_seconds": -1}, // negative cap
		{"schedule": ""},        // no schedule
	} {
		w := patchJob(t, srv, job.ID, patch)
		if w.Code != 400 {
			t.Errorf("patch %v: expected 400, got %d %s", patch, w.Code, w.Body.String())
		}
	}
}

// TestPatchWithNoFieldsIsANoOp — an empty body asks for nothing and gets the
// job back unchanged. It must not be confused with the unknown-field case,
// which is a typo and 400s.
func TestPatchWithNoFieldsIsANoOp(t *testing.T) {
	srv := setup(t, "")
	job := createJob(t, srv, map[string]any{"name": "j", "schedule": "* * * * *", "command": "echo"})

	w := patchJob(t, srv, job.ID, map[string]any{})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d %s", w.Code, w.Body.String())
	}
	var unchanged db.Job
	json.NewDecoder(w.Body).Decode(&unchanged)
	if unchanged.Name != job.Name || unchanged.UpdatedAt != job.UpdatedAt {
		t.Fatalf("empty patch changed the job: %+v", unchanged)
	}
}

func TestPatchNonexistentJobIs404(t *testing.T) {
	srv := setup(t, "")
	w := patchJob(t, srv, 999, map[string]any{"enabled": false})
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d %s", w.Code, w.Body.String())
	}
}

// TestJobPatchRequestCoversEveryPatchableColumn is the guard against the bug
// coming back one field at a time: add a column to db.JobPatch and forget the
// matching json tag here, and that column becomes unreachable over HTTP again.
func TestJobPatchRequestCoversEveryPatchableColumn(t *testing.T) {
	requestFields := []string{}
	requestType := reflect.TypeOf(jobPatchRequest{})
	for i := 0; i < requestType.NumField(); i++ {
		tag := requestType.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s has no json tag", requestType.Field(i).Name)
		}
		requestFields = append(requestFields, strings.Split(tag, ",")[0])
	}
	patchable := db.PatchableColumns()

	sort.Strings(requestFields)
	sort.Strings(patchable)
	if !reflect.DeepEqual(requestFields, patchable) {
		t.Fatalf("jobPatchRequest fields %v do not match patchable columns %v", requestFields, patchable)
	}
}

// TestCreateRejectsUnknownJobType existed nowhere until db.ValidateJob was
// shared with the bus, and the gap showed up the only way it could: deleting
// the unknown-type refusal reddened the bus suite and left this one green. The
// HTTP edge has always run the check — it simply had no test saying so, which
// makes the refusal one careless edit from vanishing on this side while the bus
// kept it. The rule is one function now, so pin it at both callers.
func TestCreateRejectsUnknownJobType(t *testing.T) {
	srv := setup(t, "")
	body, _ := json.Marshal(map[string]any{
		"name": "j", "schedule": "* * * * *", "type": "banana", "command": "echo",
	})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body)))
	if w.Code != 400 {
		t.Fatalf("expected 400 for an unknown job type, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "type must be one of") {
		t.Errorf("refused for the wrong reason: %s", w.Body.String())
	}
}

// TestModelRoleIsCheckedWithModelStore pins the edge of model_role: a role
// model-store does not know is a 400 on POST and PATCH and is not stored, a
// model-store that cannot be asked is a 502, a role on a shell job is a 400,
// and the old model field is gone rather than silently accepted.
func TestModelRoleIsCheckedWithModelStore(t *testing.T) {
	srv := setup(t, "")
	agentJob := map[string]any{"name": "a", "schedule": "0 5 * * *", "type": "agent", "agent": "claude-code", "prompt": "p"}
	withRole := func(role string) map[string]any {
		body := map[string]any{}
		for key, value := range agentJob {
			body[key] = value
		}
		body["model_role"] = role
		return body
	}
	post := func(body map[string]any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPost, "/api/jobs", bytes.NewReader(encoded))
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		return recorder
	}

	if w := post(withRole("sonnet")); w.Code != http.StatusBadRequest {
		t.Errorf("POST an unknown role: %d %s, want 400", w.Code, w.Body.String())
	}
	if w := post(withRole("model-store-down")); w.Code != http.StatusBadGateway {
		t.Errorf("POST with model-store down: %d %s, want 502", w.Code, w.Body.String())
	}
	legacy := withRole("default")
	delete(legacy, "model_role")
	legacy["model"] = "claude-opus"
	if w := post(legacy); w.Code != http.StatusBadRequest {
		t.Errorf("POST the old model field: %d %s, want 400", w.Code, w.Body.String())
	}
	if w := post(map[string]any{"name": "s", "schedule": "0 5 * * *", "type": "shell", "command": "true", "model_role": "default"}); w.Code != http.StatusBadRequest {
		t.Errorf("POST a role on a shell job: %d %s, want 400", w.Code, w.Body.String())
	}

	job := createJob(t, srv, withRole("default"))
	if job.ModelRole != "default" {
		t.Fatalf("created model_role = %q, want default", job.ModelRole)
	}
	if w := patchJob(t, srv, job.ID, map[string]any{"model_role": "sonnet"}); w.Code != http.StatusBadRequest {
		t.Errorf("PATCH an unknown role: %d %s, want 400", w.Code, w.Body.String())
	}
	if w := patchJob(t, srv, job.ID, map[string]any{"model_role": "model-store-down"}); w.Code != http.StatusBadGateway {
		t.Errorf("PATCH with model-store down: %d %s, want 502", w.Code, w.Body.String())
	}
	stored, err := srv.store.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ModelRole != "default" {
		t.Errorf("a refused PATCH changed model_role to %q", stored.ModelRole)
	}
	if w := patchJob(t, srv, job.ID, map[string]any{"model_role": "balanced"}); w.Code != http.StatusOK {
		t.Errorf("PATCH a known role: %d %s, want 200", w.Code, w.Body.String())
	}
}
