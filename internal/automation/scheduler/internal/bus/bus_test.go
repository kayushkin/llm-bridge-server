package bus

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	cronpkg "github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
	"github.com/slava/scheduler/internal/logging"

	_ "modernc.org/sqlite"
)

func setupTest() (*db.Store, *cronpkg.Scheduler, *logging.Logger, *Handler, error) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, nil, nil, err
	}

	dbStore, err := db.NewWithDB(sqlDB)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	mockExec := func(command string, timeout time.Duration) executor.Result {
		return executor.Result{Status: "success", Output: "mock output"}
	}

	scheduler := cronpkg.NewWithExecutor(dbStore, mockExec)
	logger := logging.New(nil) // nil NATS client = no-op

	// Create handler without NATS (no-op)
	handler, err := NewHandler("", dbStore, scheduler, logger, checkModelRoleForTests)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	return dbStore, scheduler, logger, handler, nil
}

// checkModelRoleForTests stands in for model-store: it knows two roles.
func checkModelRoleForTests(role string) error {
	if role == "default" || role == "balanced" {
		return nil
	}
	return fmt.Errorf("model-store has no role %q", role)
}

func TestCreateJobRefusesAModelRoleModelStoreDoesNotKnow(t *testing.T) {
	store, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	_, err = handler.handleRequest([]byte(`{"action":"create_job","name":"j","schedule":"0 3 * * *","type":"agent","agent":"claude-code","prompt":"hi","model_role":"sonnet"}`))
	if err == nil || !strings.Contains(err.Error(), "no role") {
		t.Fatalf("err = %v, want model-store's refusal", err)
	}
	jobs, _ := store.ListJobs()
	if len(jobs) != 0 {
		t.Errorf("a refused job was stored: %+v", jobs)
	}
	if _, err := NewHandler("", store, nil, nil, nil); err == nil {
		t.Error("a handler with no model-role checker was built")
	}
}

func TestHandleListJobs(t *testing.T) {
	dbStore, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	job, err := dbStore.CreateJob("test-job", "0 0 * * *", "echo hello")
	if err != nil {
		t.Fatalf("failed to create job: %v", err)
	}

	result, err := handler.handleListJobs()
	if err != nil {
		t.Fatalf("handleListJobs failed: %v", err)
	}

	resp := result.(map[string]any)
	if resp["success"] != true {
		t.Fatalf("expected success")
	}

	jobs := resp["data"].([]db.Job)
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestHandleCreateJob(t *testing.T) {
	dbStore, scheduler, logger, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	scheduler.SetLogger(logger)

	cmd := Command{
		Action:   "create_job",
		Name:     "new-job",
		Schedule: "0 0 * * *",
		Command:  "echo test",
	}

	result, err := handler.handleCreateJob(cmd)
	if err != nil {
		t.Fatalf("handleCreateJob failed: %v", err)
	}

	resp := result.(map[string]any)
	if resp["success"] != true {
		t.Fatalf("expected success")
	}

	jobs, _ := dbStore.ListJobs()
	if len(jobs) != 1 || jobs[0].Name != "new-job" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestHandleDeleteJob(t *testing.T) {
	dbStore, scheduler, logger, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	scheduler.SetLogger(logger)

	job, _ := dbStore.CreateJob("to-delete", "0 0 * * *", "echo delete")

	_, err = handler.handleDeleteJob(Command{Action: "delete_job", ID: job.ID})
	if err != nil {
		t.Fatalf("handleDeleteJob failed: %v", err)
	}

	jobs, _ := dbStore.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestHandleToggleJob(t *testing.T) {
	dbStore, scheduler, logger, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	scheduler.SetLogger(logger)

	job, _ := dbStore.CreateJob("to-toggle", "0 0 * * *", "echo toggle")

	enabled := false
	_, err = handler.handleToggleJob(Command{Action: "toggle_job", ID: job.ID, Enabled: &enabled})
	if err != nil {
		t.Fatalf("handleToggleJob failed: %v", err)
	}

	updatedJob, _ := dbStore.GetJob(job.ID)
	if updatedJob.Enabled != false {
		t.Fatalf("expected job to be disabled")
	}
}

func TestHandleRequest_InvalidJSON(t *testing.T) {
	_, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	_, err = handler.handleRequest([]byte("invalid json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	// handleRequest returns an error from eight places, so "it returned an error"
	// says nothing about which one. Delete the unmarshal check and this input still
	// errors -- an empty Action falls through to the default branch. Name the reason.
	if !strings.Contains(err.Error(), "invalid command format") {
		t.Fatalf("error should name the malformed command, got: %v", err)
	}
}

// TestAMalformedCommandIsNeverDispatched pins the property the error message only
// describes: a command that fails to unmarshal must not reach the switch. A type
// error on a later field is the case that can tell the two apart -- encoding/json
// fills the fields it did decode and reports the failure afterwards, so Action and
// ID arrive populated and the dispatch would go through if nothing stopped it.
func TestAMalformedCommandIsNeverDispatched(t *testing.T) {
	dbStore, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	job, err := dbStore.CreateJob("must-survive", "0 0 * * *", "echo hi")
	if err != nil {
		t.Fatalf("create job failed: %v", err)
	}

	// Valid action, valid id, and an "enabled" that is a string where a bool belongs.
	raw := []byte(fmt.Sprintf(`{"action":"delete_job","id":%d,"enabled":"yes"}`, job.ID))
	if _, err := handler.handleRequest(raw); err == nil {
		t.Fatal("expected error for a command that does not unmarshal")
	}

	if _, err := dbStore.GetJob(job.ID); err != nil {
		t.Fatalf("job %d was deleted by a command that never parsed: %v", job.ID, err)
	}
}

func TestHandleRequest_UnknownAction(t *testing.T) {
	_, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	cmd, _ := json.Marshal(Command{Action: "unknown_action"})
	_, err = handler.handleRequest(cmd)
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
	// Same reason as above: pin which of the eight producers fired, so that a future
	// failure earlier in handleRequest cannot keep this green with the default branch
	// broken.
	if !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("error should name the unknown action, got: %v", err)
	}
}

// TestHandleCreateJobHonorsEnabled — Command.Enabled already existed for
// toggle_job, and create_job read it and threw it away, so a caller could ask
// for a disabled job over the bus, be told "success", and get an armed one.
func TestHandleCreateJobHonorsEnabled(t *testing.T) {
	disabled := false
	enabled := true
	for _, tc := range []struct {
		name      string
		requested *bool
		want      bool
	}{
		{"absent means armed", nil, true},
		{"false means off", &disabled, false},
		{"true means armed", &enabled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbStore, scheduler, logger, handler, err := setupTest()
			if err != nil {
				t.Fatalf("setup failed: %v", err)
			}
			scheduler.SetLogger(logger)

			if _, err := handler.handleCreateJob(Command{
				Action: "create_job", Name: "j", Schedule: "0 0 * * *",
				Command: "echo test", Enabled: tc.requested,
			}); err != nil {
				t.Fatalf("handleCreateJob failed: %v", err)
			}

			jobs, _ := dbStore.ListJobs()
			if len(jobs) != 1 {
				t.Fatalf("expected 1 job, got %d", len(jobs))
			}
			if jobs[0].Enabled != tc.want {
				t.Fatalf("enabled = %t, want %t", jobs[0].Enabled, tc.want)
			}
		})
	}
}
