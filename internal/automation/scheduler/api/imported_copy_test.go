package api

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/cron"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

var (
	standaloneSchedulerCopy = flag.String("standalone-scheduler-copy", "", "a copy of a standalone scheduler's SQLite file to import and read back")
	schedulerAnswersOutput  = flag.String("scheduler-answers-output", "", "where to write what the imported copy answers each GET, as JSON")
	calendarWindowFrom      = flag.String("calendar-window-from", "2026-12-01T00:00:00Z", "the start of the calendar window read")
	calendarWindowTo        = flag.String("calendar-window-to", "2026-12-03T00:00:00Z", "the end of the calendar window read")
)

// recordedAnswer is one GET's status and body.
type recordedAnswer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// TestImportedStandaloneSchedulerAnswers imports a copy of a standalone
// scheduler and writes what every read route answers from it, for comparing
// with what the standalone service answers the same paths. It runs only when
// given a copy:
//
//	go test ./internal/automation/scheduler/api -run TestImportedStandaloneSchedulerAnswers \
//	  -args -standalone-scheduler-copy <copy.db> -scheduler-answers-output <answers.json>
func TestImportedStandaloneSchedulerAnswers(t *testing.T) {
	if *standaloneSchedulerCopy == "" || *schedulerAnswersOutput == "" {
		t.Skip("give -standalone-scheduler-copy and -scheduler-answers-output to read an imported copy back")
	}
	module, err := db.Module()
	if err != nil {
		t.Fatal(err)
	}
	pool := postgrestest.NewDatabase(t, module).Open(module)
	counts, err := db.ImportStandaloneDatabase(context.Background(), pool, *standaloneSchedulerCopy, db.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("imported %+v", counts)
	store := db.New(pool)
	settings, err := config.NewStoredSettingsRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.AttachStoredValues(store.ServiceSettingValues(), nil); err != nil {
		t.Fatal(err)
	}
	scheduler := cron.New(store)
	scheduler.SwitchRunsOff("reading an imported copy")
	routes := New(store, scheduler, "", settings, func(string) error { return nil }).Routes()

	jobs, err := store.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/api/jobs", "/api/runs/latest", "/api/event-trigger-subjects", "/settings", "/api/jobs/999999", "/api/job-references/scheduler_job_999999",
		fmt.Sprintf("/api/calendar/events?from=%s&to=%s", *calendarWindowFrom, *calendarWindowTo)}
	for _, job := range jobs {
		paths = append(paths, fmt.Sprintf("/api/jobs/%d", job.ID), fmt.Sprintf("/api/jobs/%d/runs", job.ID),
			fmt.Sprintf("/api/job-references/%s", fmt.Sprintf("%s_%06d", db.JobReferencePrefix, job.ID)))
	}
	answers := map[string]recordedAnswer{}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		answers[path] = recordedAnswer{Status: recorder.Code, Body: json.RawMessage(recorder.Body.Bytes())}
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*schedulerAnswersOutput, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d answers to %s", len(answers), *schedulerAnswersOutput)
}
