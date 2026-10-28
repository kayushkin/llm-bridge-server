package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/db"
)

func send(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	return w
}

const triggeredJobBody = `{"name":"discord DMs","type":"agent","agent":"claude-code",
	"prompt":"{{.sender_display_name}} wrote: {{.body}}",
	"event_trigger":{"subject":"messaging.inbound","conditions":[
		{"field":"platform","equals":"discord"},{"field":"is_direct_message","equals":true}]}}`

func TestCreateAndClearAnEventTrigger(t *testing.T) {
	srv := setup(t, "")
	w := send(t, srv, http.MethodPost, "/api/jobs", triggeredJobBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created db.Job
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.EventTrigger == nil || len(created.EventTrigger.Conditions) != 2 || created.Schedule != "" {
		t.Fatalf("created %+v", created)
	}
	path := "/api/jobs/" + jsonNumber(created.ID)

	// Run now has no message to fill the prompt from.
	if w := send(t, srv, http.MethodPost, path+"/run", ""); w.Code != http.StatusConflict {
		t.Fatalf("run now: %d %s, want 409", w.Code, w.Body)
	}

	// Removing the trigger alone leaves a job that runs on nothing.
	if w := send(t, srv, http.MethodPatch, path, `{"event_trigger":null}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(refusal(w), "schedule or event_trigger is required") {
		t.Fatalf("clear alone: %d %s", w.Code, w.Body)
	}
	// Swapping it for a schedule turns it into a cron job.
	w = send(t, srv, http.MethodPatch, path, `{"event_trigger":null,"schedule":"0 9 * * *","prompt":"good morning"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("swap for a schedule: %d %s", w.Code, w.Body)
	}
	var patched db.Job
	json.Unmarshal(w.Body.Bytes(), &patched)
	if patched.EventTrigger != nil || patched.Schedule != "0 9 * * *" {
		t.Fatalf("patched %+v", patched)
	}
}

func TestCreateRefusesATriggerThatCouldNotRun(t *testing.T) {
	srv := setup(t, "")
	cases := map[string]struct{ body, refusal string }{
		"both schedule and trigger": {
			`{"name":"j","type":"agent","agent":"a","prompt":"p","schedule":"* * * * *","event_trigger":{"subject":"messaging.inbound"}}`,
			"not both"},
		"a shell job": {
			`{"name":"j","type":"shell","command":"echo","event_trigger":{"subject":"messaging.inbound"}}`,
			"only for agent jobs"},
		"a misspelled key inside the trigger": {
			`{"name":"j","type":"agent","agent":"a","prompt":"p","event_trigger":{"subject":"messaging.inbound","condition":[]}}`,
			`unknown field "condition"`},
		"a prompt naming a field the message lacks": {
			`{"name":"j","type":"agent","agent":"a","prompt":"{{.Body}}","event_trigger":{"subject":"messaging.inbound"}}`,
			`no entry for key "Body"`},
	}
	for name, c := range cases {
		w := send(t, srv, http.MethodPost, "/api/jobs", c.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(refusal(w), c.refusal) {
			t.Errorf("%s: %d %s, want 400 containing %q", name, w.Code, w.Body, c.refusal)
		}
	}
}

func TestPatchRefusesAMisspelledKeyInsideTheTrigger(t *testing.T) {
	srv := setup(t, "")
	w := send(t, srv, http.MethodPost, "/api/jobs", triggeredJobBody)
	var created db.Job
	json.Unmarshal(w.Body.Bytes(), &created)
	w = send(t, srv, http.MethodPatch, "/api/jobs/"+jsonNumber(created.ID), `{"event_trigger":{"subjekt":"messaging.inbound"}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(refusal(w), `unknown field "subjekt"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestEventTriggerSubjectsListsTheFields(t *testing.T) {
	w := send(t, setup(t, ""), http.MethodGet, "/api/event-trigger-subjects", "")
	var subjects []eventTriggerSubject
	if err := json.Unmarshal(w.Body.Bytes(), &subjects); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	if len(subjects) != 1 || subjects[0].Subject != "messaging.inbound" || subjects[0].IdempotencyField != "event_id" {
		t.Fatalf("subjects = %+v", subjects)
	}
	types := map[string]string{}
	for _, field := range subjects[0].Fields {
		types[field.Name] = field.Type
	}
	if types["body"] != "string" || types["is_direct_message"] != "boolean" || types["timestamp"] != "time" {
		t.Fatalf("fields = %v", types)
	}
}

// refusal is a 400 body's error and details, decoded.
func refusal(w *httptest.ResponseRecorder) string {
	var body APIError
	json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error + " " + body.Details
}

func jsonNumber(id int64) string {
	encoded, _ := json.Marshal(id)
	return string(encoded)
}
