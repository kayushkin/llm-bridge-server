package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The create half of the same defect. handleCreate built its request body from
// nine hardcoded keys while the tool's own schema advertised four more and the
// scheduler decodes three of those four, so a job created with a description, a
// timeout or a workspace lost all three between the caller and the row. Nothing
// on screen said so: the old success line named the id, name, schedule, type
// and enabled flag, and no field beyond those was ever mentioned either way.
//
// Every field POST /api/jobs decodes, per scheduler/internal/api/api.go:126-144.
var fieldsTheSchedulerStoresOnCreate = []string{
	"name", "description", "schedule", "command", "type",
	"agent", "prompt", "model_role", "orchestrator", "session_id",
	"workspace_id", "timeout_seconds", "enabled",
}

// A blank row carrying only what the server itself supplies: an id, and the
// enabled flag db.go sets on insert. Anything else appearing on the echo can
// only have come from the request.
func jobAsCreated() Job {
	return Job{ID: 51, Enabled: true}
}

func intPointer(value int) *int { return &value }

// The defect, stated as the three fields the card was filed for.
func TestCreateForwardsTheDescriptionTimeoutAndWorkspaceTheCallerNamed(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	report, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:        stringPointer("nightly summary"),
		Schedule:    stringPointer("0 8 * * *"),
		Type:        stringPointer("agent"),
		Agent:       stringPointer("claude-code"),
		Prompt:      stringPointer("summarize yesterday"),
		Description: stringPointer("what it does"),
		TimeoutSecs: intPointer(1800),
		WorkspaceID: stringPointer("ws-42"),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	sent := lastRequest().Body
	for field, want := range map[string]any{
		"description":     "what it does",
		"timeout_seconds": float64(1800),
		"workspace_id":    "ws-42",
	} {
		got, present := sent[field]
		if !present {
			t.Errorf("the create request never carried %q, so the caller lost it silently", field)
			continue
		}
		if got != want {
			t.Errorf("the create request sent %v for %q, want %v", got, field, want)
		}
	}

	if strings.Contains(report, "NOT stored") {
		t.Errorf("a server that stored every field was reported as dropping one:\n%s", report)
	}
	for _, want := range []string{"description", "timeout_seconds", "workspace_id"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report never mentions %s, so nothing on screen says it landed:\n%s", want, report)
		}
	}
}

// workspace_id is the field with teeth and it gets its own case. An agent job's
// durable memory is bound by it, and a job that lost it reads as a job that was
// never given one — there is no error and no second chance to notice.
func TestCreateNamesTheWorkspaceTheServerDidNotStore(t *testing.T) {
	storesEverythingButTheWorkspace := []string{"name", "schedule", "type", "agent", "prompt"}
	baseURL, _ := fakeScheduler(t, jobAsCreated(), storesEverythingButTheWorkspace)

	report, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:        stringPointer("nightly summary"),
		Schedule:    stringPointer("0 8 * * *"),
		Type:        stringPointer("agent"),
		Agent:       stringPointer("claude-code"),
		Prompt:      stringPointer("summarize yesterday"),
		WorkspaceID: stringPointer("ws-42"),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	storedSection, droppedSection, found := strings.Cut(report, "NOT stored:")
	if !found {
		t.Fatalf("a create that lost the workspace binding did not say so:\n%s", report)
	}
	if !strings.Contains(droppedSection, "workspace_id") {
		t.Errorf("workspace_id was dropped and is not in the NOT stored section:\n%s", report)
	}
	if !strings.Contains(droppedSection, `"ws-42"`) {
		t.Errorf("the report does not say what was asked for, so the caller cannot see what was lost:\n%s", report)
	}
	if strings.Contains(storedSection, "workspace_id") {
		t.Errorf("workspace_id was dropped but is listed as stored:\n%s", report)
	}
	if !strings.Contains(storedSection, "prompt") {
		t.Errorf("prompt was stored and the report denies it:\n%s", report)
	}
}

// A caller who asks for a disabled job gets one. The scheduler has taken
// enabled on create since 271998d; before that this tool dropped the key and
// the caller got a live cron job.
func TestCreateSendsEnabledFalseAndTheJobIsCreatedOff(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	report, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:     stringPointer("nightly summary"),
		Schedule: stringPointer("0 8 * * *"),
		Command:  stringPointer("echo hi"),
		Enabled:  boolPointer(false),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	if got, present := lastRequest().Body["enabled"]; !present || got != false {
		t.Errorf("create did not send enabled:false; body was %v", lastRequest().Body)
	}
	if !strings.Contains(report, "Enabled: false") {
		t.Errorf("the caller asked for a disabled job and the report does not show one:\n%s", report)
	}
	if strings.Contains(report, "NOT stored") {
		t.Errorf("the server stored every field and the report says otherwise:\n%s", report)
	}
}

// A caller who never names enabled leaves the choice to the scheduler, which
// creates the job enabled.
func TestCreateDoesNotSendEnabledWhenTheCallerDidNotNameIt(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	_, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:     stringPointer("nightly summary"),
		Schedule: stringPointer("0 8 * * *"),
		Command:  stringPointer("echo hi"),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	if _, present := lastRequest().Body["enabled"]; present {
		t.Errorf("create sent enabled though the caller never named it: %v", lastRequest().Body)
	}
}

// The verb axis. Every case in the update file drives a PATCH, so a create that
// used the wrong verb, or an update handler reused wholesale, would be
// invisible to all of them.
func TestCreatePOSTsToTheJobsCollection(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	if _, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:     stringPointer("nightly summary"),
		Schedule: stringPointer("0 8 * * *"),
		Command:  stringPointer("echo hi"),
	}); err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	seen := lastRequest()
	if seen.Method != "POST" {
		t.Errorf("create sent %s, want POST", seen.Method)
	}
	if seen.Path != "/api/jobs" {
		t.Errorf("create posted to %q, want /api/jobs", seen.Path)
	}
}

// The status axis. Create is the one write path whose success code is not 200,
// and a handler that accepted any 2xx would take a 200 — which from this server
// means the request was routed as something other than a create — as success.
func TestCreateRefusesA200BecauseCreateAnswers201(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(jobAsCreated()); err != nil {
			t.Errorf("stub could not encode its reply: %s", err)
		}
	}))
	t.Cleanup(server.Close)

	report, err := handleCreate(context.Background(), server.URL, http.DefaultClient, schedulerInput{
		Name:     stringPointer("nightly summary"),
		Schedule: stringPointer("0 8 * * *"),
		Command:  stringPointer("echo hi"),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	if !strings.Contains(report, "API returned 200") {
		t.Errorf("a create answered 200 instead of 201 was not reported as a failure:\n%s", report)
	}
	if strings.Contains(report, "Created job") {
		t.Errorf("a create that did not answer 201 was announced as a created job:\n%s", report)
	}
}

// The type axis, on the create path this time. timeout_seconds is the only
// number among the twelve fields create forwards and the comparison behind the
// report is == on an `any`, which is type-sensitive. If the value asked for
// (an int) ever met a value decoded straight out of JSON (a float64), a stored
// timeout would report itself as dropped forever and no string case could see
// it.
func TestCreateComparesTheTimeoutByValueAndNotByItsJSONType(t *testing.T) {
	baseURL, _ := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	report, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:        stringPointer("nightly summary"),
		Schedule:    stringPointer("0 8 * * *"),
		Command:     stringPointer("echo hi"),
		TimeoutSecs: intPointer(3600),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	if strings.Contains(report, "NOT stored") {
		t.Errorf("the server stored the timeout and the report calls it dropped:\n%s", report)
	}
	if !strings.Contains(report, "timeout_seconds") {
		t.Errorf("the report never names timeout_seconds:\n%s", report)
	}
}

// A caller who names no type still gets one, and the report has to name the
// type the job actually got rather than stay silent about a value nobody chose.
func TestCreateForwardsTheDefaultedTypeAndReportsIt(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	report, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:     stringPointer("nightly summary"),
		Schedule: stringPointer("0 8 * * *"),
		Command:  stringPointer("echo hi"),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	if sent := lastRequest().Body["type"]; sent != "shell" {
		t.Errorf("create sent type %v, want the defaulted \"shell\"", sent)
	}
	if !strings.Contains(report, "Type: shell") {
		t.Errorf("the report does not name the type the job was given:\n%s", report)
	}
}

// scheduler 89b1712 renamed the job field `model` to `model_role`, and its
// POST decoder is strict: measured live 2026-10-02, a create carrying
// "model":"x" answers 400 `json: unknown field "model"`. This tool kept
// sending `model`, so any create that named a model failed outright.
func TestCreateSendsTheModelRoleUnderTheKeyTheSchedulerDecodes(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobAsCreated(), fieldsTheSchedulerStoresOnCreate)

	_, err := handleCreate(context.Background(), baseURL, http.DefaultClient, schedulerInput{
		Name:      stringPointer("nightly summary"),
		Schedule:  stringPointer("0 8 * * *"),
		Type:      stringPointer("agent"),
		Agent:     stringPointer("claude-code"),
		Prompt:    stringPointer("summarize yesterday"),
		ModelRole: stringPointer("balanced"),
	})
	if err != nil {
		t.Fatalf("handleCreate returned an error: %s", err)
	}

	sent := lastRequest().Body
	if got := sent["model_role"]; got != "balanced" {
		t.Errorf("the create request sent model_role=%v, want \"balanced\"", got)
	}
	if _, present := sent["model"]; present {
		t.Errorf("the create request still carries \"model\", which the scheduler answers with a 400")
	}
}
