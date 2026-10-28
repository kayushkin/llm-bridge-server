package bus

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/eventtriggers"

	_ "modernc.org/sqlite"
)

// A job can be created from two places, and the file next door pins them to the
// same RULES. This one pins them to the same FIELDS, which is a separate way for
// the pair to drift and was broken while the rules were being fixed.
//
// POST /api/jobs accepted twelve; Command declared nine. description,
// workspace_id and timeout_seconds existed on the HTTP edge only. Measured
// against the old handler through handleRequest, the same json.Unmarshal the
// NATS reply path uses:
//
//	{"action":"create_job","name":"j","schedule":"0 3 * * *","type":"agent",
//	 "agent":"claude-code","prompt":"hi","workspace_id":"ws-123",
//	 "timeout_seconds":3600,"description":"a description"}
//
//	-> err=<nil>, success:true, and the stored row read
//	   workspace_id="" timeout_seconds=0 description=""
//
// Accepted, reported success, dropped all three. The HTTP edge could not do
// this: it decodes with decodeStrict, so an unknown field is a 400. handleRequest
// uses a plain json.Unmarshal, which discards silently.
//
// Two of the three had teeth. workspace_id is an agent job's working memory —
// executor.agent prepends the working-memory preamble only when it is non-empty,
// so a bus-created agent job ran with no memory and nothing said so.
// timeout_seconds is the per-job wall-clock cap, so a bus-created job could not
// be given one at creation and was killed at the five-minute default.
//
// Every case here goes through handleRequest with raw JSON rather than calling
// handleCreateJob with a filled-in Command. That is deliberate: this defect was
// half a missing struct field and half a missing assignment, and a test that
// hands handleCreateJob a Command literal only ever sees the second half. A
// struct field with a misspelled json tag would pass it and drop the value in
// production.

// creationRequest is one create_job request paired with what every field in it
// should read back as. Keyed by db.Job FIELD name rather than by json tag,
// because the accounting test below compares these keys against db.Job's own
// fields — a new column has a field name whatever its tag says, and can have no
// tag at all.
//
// It takes two requests to reach every creatable field: an agent job cannot
// carry a command and a shell job cannot carry an agent or a prompt, so a single
// request can only ever cover one of those branches.
type creationRequest struct {
	name string
	body string
	want map[string]any
}

var creationRequests = []creationRequest{
	{
		name: "an agent job carrying every field an agent job can carry",
		body: `{
			"action":"create_job","name":"j","description":"a description",
			"schedule":"0 3 * * *","type":"agent","agent":"claude-code","prompt":"hi",
			"model_role":"default","orchestrator":"claude-code","session_id":"sess-1",
			"workspace_id":"ws-123","timeout_seconds":3600
		}`,
		want: map[string]any{
			"Name": "j", "Description": "a description", "Schedule": "0 3 * * *",
			"Type": "agent", "Agent": "claude-code", "Prompt": "hi", "ModelRole": "default",
			"Orchestrator": "claude-code", "SessionID": "sess-1",
			"WorkspaceID": "ws-123", "TimeoutSecs": 3600,
		},
	},
	{
		name: "an agent job that runs on an event trigger instead of a schedule",
		body: `{
			"action":"create_job","name":"t","type":"agent","agent":"claude-code",
			"prompt":"{{.body}}",
			"event_trigger":{"subject":"messaging.inbound","conditions":[{"field":"is_direct_message","equals":true}]}
		}`,
		want: map[string]any{
			"Name": "t", "Type": "agent", "Agent": "claude-code", "Prompt": "{{.body}}",
			"EventTrigger": &eventtriggers.Trigger{Subject: "messaging.inbound", Conditions: []eventtriggers.Condition{
				{Field: "is_direct_message", Equals: json.RawMessage(`true`)},
			}},
		},
	},
	{
		name: "a shell job, which is the only kind that can carry a command",
		body: `{
			"action":"create_job","name":"s","description":"shell one",
			"schedule":"0 4 * * *","type":"shell","command":"echo hello",
			"timeout_seconds":1800
		}`,
		want: map[string]any{
			"Name": "s", "Description": "shell one", "Schedule": "0 4 * * *",
			"Type": "shell", "Command": "echo hello", "TimeoutSecs": 1800,
		},
	},
}

// TestCreateJobOverTheBusStoresEveryFieldItWasSent is the direct pin: the
// measurement in this file's header, inverted.
func TestCreateJobOverTheBusStoresEveryFieldItWasSent(t *testing.T) {
	for _, request := range creationRequests {
		t.Run(request.name, func(t *testing.T) {
			store, _, _, handler, err := setupTest()
			if err != nil {
				t.Fatalf("setup failed: %v", err)
			}
			if _, err := handler.handleRequest([]byte(request.body)); err != nil {
				t.Fatalf("refused a runnable job: %v", err)
			}
			jobs, err := store.ListJobs()
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 {
				t.Fatalf("expected 1 stored job, got %d", len(jobs))
			}
			stored := reflect.ValueOf(jobs[0])
			for field, want := range request.want {
				got := stored.FieldByName(field).Interface()
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: sent %v, stored %v", field, want, got)
				}
			}
		})
	}
}

// TestCreateJobOverTheBusLeavesAnOmittedFieldEmpty is the other half. A handler
// that defaulted the three new fields to something non-empty would pass the test
// above and quietly give every bus-created job a timeout nobody asked for.
func TestCreateJobOverTheBusLeavesAnOmittedFieldEmpty(t *testing.T) {
	store, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if _, err := handler.handleRequest([]byte(
		`{"action":"create_job","name":"j","schedule":"0 3 * * *","command":"echo"}`,
	)); err != nil {
		t.Fatalf("refused a runnable job: %v", err)
	}

	jobs, err := store.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 stored job, got %d", len(jobs))
	}
	stored := jobs[0]
	if stored.Description != "" || stored.WorkspaceID != "" || stored.TimeoutSecs != 0 {
		t.Errorf("omitted fields did not stay empty: description=%q workspace_id=%q timeout_seconds=%d",
			stored.Description, stored.WorkspaceID, stored.TimeoutSecs)
	}
}

// TestCreateJobOverTheBusRefusesANegativeTimeout pins a refusal that only became
// reachable when timeout_seconds started arriving. db.ValidateJob has always
// rejected a negative cap, but the bus dropped the field before it got there, so
// the check was dead on this edge and `-1` was stored as the 0 that means
// "default". Wiring a field through can arm a rule that was previously
// unreachable, and that is worth a case of its own rather than an assumption.
func TestCreateJobOverTheBusRefusesANegativeTimeout(t *testing.T) {
	store, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if _, err := handler.handleRequest([]byte(
		`{"action":"create_job","name":"j","schedule":"0 3 * * *","command":"echo","timeout_seconds":-1}`,
	)); err == nil {
		t.Error("a negative timeout_seconds was accepted")
	}
	assertNoJobsStored(t, store)
}

// jobFieldsNotSettableAtCreation names every db.Job field a creation request has
// no business carrying, with the reason. It is an exclusion list, so it is
// written to be read: the test below reports any field that is neither covered
// by a creation request nor named here, which means a new column on Job fails
// until somebody makes that choice explicitly.
//
// ⚠️ This list fails SILENTLY in one direction. A field that stops being
// excluded — one that becomes settable at creation — does not redden anything,
// because the test only requires that excluded fields go unasserted. So a stale
// entry here reads exactly like a correct one. Enabled is a live example: on
// branch fix/create-a-job-in-the-off-position, dd0b274 drops the literal 1 from
// CreateJobFull's INSERT and makes enabled settable at both edges. That commit
// is NOT an ancestor of this branch — checked, not assumed — so the reason below
// is true here and will be false the moment the two are merged. Whoever merges
// them: move Enabled out of this list and give it a creation request.
var jobFieldsNotSettableAtCreation = map[string]string{
	"ID":        "assigned by the store on insert",
	"Enabled":   "CreateJobFull binds a literal 1 in its INSERT — see the warning above",
	"HoldUntil": "set by the hold endpoints, not at creation",
	"CreatedAt": "stamped by the store",
	"UpdatedAt": "stamped by the store",
}

// TestEveryCreatableJobFieldIsCoveredByACreationRequest is the guard the
// original defect went past. Both edges build a db.Job, and the only thing that
// had ever kept their field lists in step was somebody remembering to edit two
// files. Nothing failed when that stopped happening — the bus answered success
// and dropped three fields for as long as it took a side-by-side read to notice.
//
// It accounts for every field of db.Job against the requests above: each is
// either asserted by one of them or named in jobFieldsNotSettableAtCreation with
// a reason. A thirteenth column added to Job reddens this test until somebody
// makes that choice explicitly, and the choice is then written down either way.
//
// It measures against db.Job rather than the HTTP request struct because that
// struct is declared inline inside the POST handler and cannot be named from
// here. Job is the better reference anyway: it is the thing both edges are
// trying to fill in.
//
// ⚠️ It deliberately does NOT reflect over Command. Command is shared by all six
// bus actions, so ID is on it for delete_job and Enabled for toggle_job, and
// asking "does Command have this field" calls both of those settable at
// creation. My first draft did exactly that and reported two false failures.
// Presence on a multi-action struct is not reachability; only a request that
// comes back off the store proves the field arrived.
func TestEveryCreatableJobFieldIsCoveredByACreationRequest(t *testing.T) {
	asserted := map[string]bool{}
	for _, request := range creationRequests {
		for field := range request.want {
			asserted[field] = true
		}
	}

	jobType := reflect.TypeOf(db.Job{})
	for i := 0; i < jobType.NumField(); i++ {
		name := jobType.Field(i).Name
		_, excluded := jobFieldsNotSettableAtCreation[name]
		switch {
		case excluded && asserted[name]:
			t.Errorf("db.Job.%s is listed as not settable at creation, but a creation "+
				"request asserts it — one of the two is wrong", name)
		case !excluded && !asserted[name]:
			t.Errorf("db.Job.%s is not covered by any creation request and is not listed "+
				"in jobFieldsNotSettableAtCreation — wire it through Command and "+
				"handleCreateJob and assert it above, or add it to that list with the reason", name)
		}
	}
}
