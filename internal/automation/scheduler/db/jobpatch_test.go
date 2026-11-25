package db

import (
	"database/sql"
	"encoding/json"
	"reflect"

	"errors"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
	"testing"
)

func patchTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewWithDB(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestUpdateJobWritesOnlyTheFieldsThePatchSets(t *testing.T) {
	store := patchTestStore(t)
	created, err := store.CreateJobFull(Job{
		Name: "agent-job", Schedule: "0 5 * * *", Type: "agent",
		Agent: "claude-code", Prompt: "old", ModelRole: "default", TimeoutSecs: 60,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	prompt := "new"
	if err := store.UpdateJob(created.ID, JobPatch{Prompt: &prompt}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetJob(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "new" {
		t.Errorf("prompt = %q, want %q", got.Prompt, "new")
	}
	// Everything the patch did not name must be exactly as it was.
	if got.ModelRole != "default" || got.Agent != "claude-code" || got.TimeoutSecs != 60 ||
		got.Name != "agent-job" || got.Schedule != "0 5 * * *" || !got.Enabled {
		t.Errorf("a one-field patch disturbed another column: %+v", got)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at did not move: %v then %v", created.UpdatedAt, got.UpdatedAt)
	}
}

// TestUpdateJobClearsAFieldSetToItsZeroValue — "omitted" and "set to empty"
// are different requests, and the pointer fields are what keeps them apart.
func TestUpdateJobClearsAFieldSetToItsZeroValue(t *testing.T) {
	store := patchTestStore(t)
	created, _ := store.CreateJobFull(Job{
		Name: "j", Schedule: "* * * * *", Type: "agent",
		Agent: "claude-code", Prompt: "p", SessionID: "sess-1",
	})

	empty := ""
	if err := store.UpdateJob(created.ID, JobPatch{SessionID: &empty}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetJob(created.ID)
	if got.SessionID != "" {
		t.Errorf("session_id = %q, want cleared", got.SessionID)
	}
	if got.Prompt != "p" {
		t.Errorf("prompt = %q, want it untouched", got.Prompt)
	}
}

func TestUpdateJobWritesEveryPatchableColumn(t *testing.T) {
	store := patchTestStore(t)
	// Enabled: true so that patching it to false below is an observable
	// change. Created disabled, the enabled assertion would hold whether or
	// not UpdateJob wrote that column.
	created, _ := store.CreateJobFull(Job{Name: "j", Schedule: "* * * * *", Command: "echo", Enabled: true})

	name, description, schedule, command := "renamed", "desc", "0 1 * * *", "echo two"
	jobType, agent, prompt, modelRole := "agent", "claude-code", "p", "balanced"
	orchestrator, sessionID, workspaceID := "inber", "sess", "ws"
	timeout, enabled := 1800, false
	eventTrigger := `{"subject":"messaging.inbound","conditions":[{"field":"platform","equals":"discord"}]}`
	patch := JobPatch{
		Name: &name, Description: &description, Schedule: &schedule, Command: &command,
		Type: &jobType, Agent: &agent, Prompt: &prompt, ModelRole: &modelRole,
		Orchestrator: &orchestrator, SessionID: &sessionID, WorkspaceID: &workspaceID,
		EventTrigger: &eventTrigger, TimeoutSecs: &timeout, Enabled: &enabled,
	}
	if got := len(patch.Changes()); got != len(PatchableColumns()) {
		t.Fatalf("patch reports %d changes, want all %d patchable columns", got, len(PatchableColumns()))
	}
	if err := store.UpdateJob(created.ID, patch); err != nil {
		t.Fatal(err)
	}

	got, _ := store.GetJob(created.ID)
	want := Job{
		ID: created.ID, Name: name, Description: description, Schedule: schedule,
		Command: command, Type: jobType, Agent: agent, Prompt: prompt, ModelRole: modelRole,
		Orchestrator: orchestrator, SessionID: sessionID, WorkspaceID: workspaceID,
		EventTrigger: &eventtriggers.Trigger{Subject: "messaging.inbound", Conditions: []eventtriggers.Condition{
			{Field: "platform", Equals: json.RawMessage(`"discord"`)},
		}},
		TimeoutSecs: timeout, Enabled: enabled,
	}
	want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("after full patch:\n got %+v\nwant %+v", *got, want)
	}
}

func TestUpdateJobRejectsAnEmptyPatch(t *testing.T) {
	store := patchTestStore(t)
	created, _ := store.CreateJobFull(Job{Name: "j", Schedule: "* * * * *", Command: "echo"})

	err := store.UpdateJob(created.ID, JobPatch{})
	if !errors.Is(err, ErrEmptyJobPatch) {
		t.Fatalf("empty patch returned %v, want ErrEmptyJobPatch", err)
	}
}

func TestUpdateJobOnMissingRowIsErrNoRows(t *testing.T) {
	store := patchTestStore(t)
	enabled := false
	if err := store.UpdateJob(999, JobPatch{Enabled: &enabled}); err != sql.ErrNoRows {
		t.Fatalf("got %v, want sql.ErrNoRows", err)
	}
}

// TestEnabledRoundTripsThroughSQLite — jobs.enabled is INTEGER, so a Go bool
// has to be converted on the way in. Both directions have to survive it.
func TestEnabledRoundTripsThroughSQLite(t *testing.T) {
	store := patchTestStore(t)
	// Enabled: true so the first step of the loop is a real transition.
	created, _ := store.CreateJobFull(Job{Name: "j", Schedule: "* * * * *", Command: "echo", Enabled: true})

	for _, want := range []bool{false, true, false} {
		if err := store.UpdateJob(created.ID, JobPatch{Enabled: &want}); err != nil {
			t.Fatal(err)
		}
		got, _ := store.GetJob(created.ID)
		if got.Enabled != want {
			t.Fatalf("enabled = %t, want %t", got.Enabled, want)
		}
	}
}
