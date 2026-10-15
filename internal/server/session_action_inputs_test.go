package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func runRequestWithInputs(sessionID, actionID string, inputs map[string]string) *http.Request {
	body, _ := json.Marshal(msg.SessionActionRunRequest{Inputs: inputs})
	request := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/actions/"+actionID+"/run", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestTypedInputsReachTheCommandOnlyAndASecretIsKeptNowhere(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_inputs", msg.SessionTypeInteractive)
	models := withFakeSessionActionModels(srv, "caution")
	const secret = "correct horse battery staple"

	recorder := offerAsInternalService(t, srv, "br_inputs", map[string]any{
		"label": "Save the Mac password", "type": "run_command", "working_directory": t.TempDir(),
		"shell_command": `printf 'stored %s for %s\n' "$BRIDGE_INPUT_PASSWORD" "$BRIDGE_INPUT_HOST"`,
		"inputs": []map[string]any{
			{"name": "PASSWORD", "label": "Mac login password", "secret": true},
			{"name": "HOST", "label": "Which Mac"},
		},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
	}
	var offered msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &offered)
	for _, want := range []string{`$BRIDGE_INPUT_PASSWORD (secret, "Mac login password")`, `$BRIDGE_INPUT_HOST (text, "Which Mac")`} {
		if !strings.Contains(offered.Command, want) {
			t.Errorf("command %q does not name %s", offered.Command, want)
		}
		if !strings.Contains(models.requests[0].Prompt, want) {
			t.Errorf("the reviewer was not told about %s: %q", want, models.requests[0].Prompt)
		}
	}

	for _, bad := range []map[string]string{nil, {"PASSWORD": secret}, {"PASSWORD": secret, "HOST": "MacBook", "EXTRA": "x"}} {
		recorder = httptest.NewRecorder()
		asInternalService(srv).ServeHTTP(recorder, runRequestWithInputs("br_inputs", offered.ActionID, bad))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_session_action_inputs") {
			t.Fatalf("confirm with %v: %d %s; want 400", bad, recorder.Code, recorder.Body)
		}
	}
	if still, _ := st.GetSessionAction("br_inputs", offered.ActionID); still.State != msg.SessionActionOffered {
		t.Fatalf("a refused confirm moved the action to %s", still.State)
	}

	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequestWithInputs("br_inputs", offered.ActionID, map[string]string{"PASSWORD": secret, "HOST": "MacBook"}))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("confirm: %d %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Error("the confirm's answer carries the secret")
	}
	finished := waitForSessionActionToEnd(t, st, "br_inputs", offered.ActionID)
	if finished.State != msg.SessionActionSucceeded || finished.Output != "stored [hidden input] for MacBook\n" {
		t.Fatalf("finished = %s %q %q", finished.State, finished.Output, finished.Error)
	}
	record, _ := json.Marshal(finished)
	if strings.Contains(string(record), secret) {
		t.Error("the stored action holds the secret")
	}
	events, err := st.ListEventsSinceID("br_inputs", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if encoded, _ := json.Marshal(event); strings.Contains(string(encoded), secret) {
			t.Errorf("an event holds the secret: %s", encoded)
		}
	}
}

func TestInputsAreRefusedWhenMalformedOrOnAnotherKind(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_inputs_bad", msg.SessionTypeInteractive)
	withFakeSessionActionModels(srv, "approve")
	command := map[string]any{"label": "x", "type": "run_command", "shell_command": "true"}
	withInputs := func(inputs ...map[string]any) map[string]any {
		offer := map[string]any{}
		for k, v := range command {
			offer[k] = v
		}
		offer["inputs"] = inputs
		return offer
	}
	for _, offer := range []map[string]any{
		withInputs(map[string]any{"name": "password", "label": "lower case"}),
		withInputs(map[string]any{"name": "1ST", "label": "starts with a digit"}),
		withInputs(map[string]any{"name": "A", "label": "one"}, map[string]any{"name": "A", "label": "twice"}),
		withInputs(map[string]any{"name": "NO_LABEL", "label": " "}),
		withInputs(map[string]any{"name": "A", "label": "a"}, map[string]any{"name": "B", "label": "b"}, map[string]any{"name": "C", "label": "c"},
			map[string]any{"name": "D", "label": "d"}, map[string]any{"name": "E", "label": "e"}, map[string]any{"name": "F", "label": "f"}),
		{"label": "Ask", "type": "model_call", "message": "hi", "model": "balanced", "maximum_cost_usd": 0.1,
			"inputs": []map[string]any{{"name": "A", "label": "a"}}},
	} {
		if recorder := offerAsInternalService(t, srv, "br_inputs_bad", offer); recorder.Code != http.StatusBadRequest {
			t.Errorf("offer %v: %d %s; want 400", offer["inputs"], recorder.Code, recorder.Body)
		}
	}
}
