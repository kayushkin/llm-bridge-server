package bus

import (
	"database/sql"
	"encoding/json"
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
	handler, err := NewHandler("", dbStore, scheduler, logger)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	return dbStore, scheduler, logger, handler, nil
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
}
