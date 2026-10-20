package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// sessionsForTargetActions makes a manager session that offers buttons and a
// chat session they act on, both on the mock harness so a send completes.
func sessionsForTargetActions(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	srv, st, instanceID := sendableTestServer(t)
	for _, session := range []*store.Session{
		{SessionID: "br_manager", DisplayName: "Claxon", Harness: msg.HarnessMock, InstanceID: instanceID, State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive},
		{SessionID: "br_target", DisplayName: "Fix the flaky test", Harness: msg.HarnessMock, InstanceID: instanceID, State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive},
	} {
		if err := st.CreateSession(session); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	return srv, st
}

func offeredAction(t *testing.T, recorder *httptest.ResponseRecorder) msg.SessionAction {
	t.Helper()
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
	}
	var offered msg.SessionAction
	if err := json.Unmarshal(recorder.Body.Bytes(), &offered); err != nil {
		t.Fatal(err)
	}
	return offered
}

func TestASendToSessionButtonSendsTheTypedReplyToTheOtherSession(t *testing.T) {
	srv, st := sessionsForTargetActions(t)
	offered := offeredAction(t, offerAsInternalService(t, srv, "br_manager", map[string]any{
		"label": "Answer the flaky-test session", "type": "send_to_session", "target_session_id": "br_target",
		"message": "The operator says: $BRIDGE_INPUT_REPLY",
		"inputs":  []map[string]any{{"name": "REPLY", "label": "Your reply"}},
	}))
	wantCommand := "send session br_target (\"Fix the flaky test\") this message, as if the person had typed it there:\n" +
		"The operator says: $BRIDGE_INPUT_REPLY\n(what the person types into \"Your reply\" replaces $BRIDGE_INPUT_REPLY)"
	if offered.Command != wantCommand {
		t.Errorf("command = %q, want %q", offered.Command, wantCommand)
	}

	targetEvents := srv.harness.Subscribe("br_target")
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequestWithInputs("br_manager", offered.ActionID, map[string]string{"REPLY": "skip it for now"}))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", recorder.Code, recorder.Body)
	}
	finished := waitForSessionActionToEnd(t, st, "br_manager", offered.ActionID)
	if finished.State != msg.SessionActionSucceeded || finished.ResultSessionID != "br_target" ||
		!strings.HasPrefix(finished.Output, `Sent the message to session br_target ("Fix the flaky test"); its state is now`) {
		t.Fatalf("finished = %+v", finished)
	}
	if strings.Contains(finished.Output, "skip it for now") {
		t.Errorf("the typed value is in the recorded output: %q", finished.Output)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-targetEvents:
			if event.Type == msg.EventUserMessage {
				if event.Result == nil || event.Result.Text != "The operator says: skip it for now" {
					t.Fatalf("the target was sent %+v", event.Result)
				}
				return
			}
		case <-deadline:
			t.Fatal("the target session got no user_message")
		}
	}
}

func TestAStopSessionButtonStopsTheOtherSessionEvenAfterItWasRenamed(t *testing.T) {
	srv, st := sessionsForTargetActions(t)
	offered := offeredAction(t, offerAsInternalService(t, srv, "br_manager", map[string]any{
		"label": "Stop the flaky-test session", "type": "stop_session", "target_session_id": "br_target",
	}))
	if want := `stop session br_target ("Fix the flaky test", now idle): kill its harness process and mark it aborted`; offered.Command != want {
		t.Errorf("command = %q, want %q", offered.Command, want)
	}
	// The renamer may rename the target between the offer and the press; the
	// name is only for reading, so the button still runs.
	if err := st.UpdateSessionDisplayName("br_target", "Flaky TestSignalTodoLink"); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_manager", offered.ActionID))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", recorder.Code, recorder.Body)
	}
	finished := waitForSessionActionToEnd(t, st, "br_manager", offered.ActionID)
	want := `Stopped session br_target ("Flaky TestSignalTodoLink"); its state is now aborted`
	if finished.State != msg.SessionActionSucceeded || finished.Output != want || finished.ResultSessionID != "br_target" {
		t.Fatalf("finished = %+v, want output %q", finished, want)
	}
	target, err := st.GetSession("br_target")
	if err != nil || target.State != string(msg.SessionAborted) {
		t.Errorf("target = %+v, %v; want aborted", target, err)
	}
}

func TestATargetSessionActionIsRefusedWhenItsTargetIsWrong(t *testing.T) {
	srv, st := sessionsForTargetActions(t)
	for _, session := range []*store.Session{
		{SessionID: "br_someone_elses", Harness: msg.HarnessMock, State: string(msg.SessionIdle), PrincipalID: "principal_000002"},
		{SessionID: "br_subagent", Harness: msg.HarnessMock, State: string(msg.SessionIdle), ControlledBy: msg.ControlledByHarness, ParentID: "br_target"},
	} {
		if err := st.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	reply := []map[string]any{{"name": "REPLY", "label": "Your reply"}}
	cases := []struct {
		name   string
		offer  map[string]any
		status int
		want   string
	}{
		{"no such session", map[string]any{"label": "x", "type": "stop_session", "target_session_id": "br_missing"}, 404, "unknown_target_session"},
		{"another principal's session", map[string]any{"label": "x", "type": "send_to_session", "target_session_id": "br_someone_elses", "message": "hi"}, 404, "unknown_target_session"},
		{"its own session", map[string]any{"label": "x", "type": "stop_session", "target_session_id": "br_manager"}, 400, "target_is_this_session"},
		{"a harness subagent", map[string]any{"label": "x", "type": "send_to_session", "target_session_id": "br_subagent", "message": "hi"}, 409, "target_not_bridge_controlled"},
		{"no target", map[string]any{"label": "x", "type": "send_to_session", "message": "hi"}, 400, "needs target_session_id"},
		{"an empty message", map[string]any{"label": "x", "type": "send_to_session", "target_session_id": "br_target", "message": "  "}, 400, "needs message"},
		{"a stop with a message", map[string]any{"label": "x", "type": "stop_session", "target_session_id": "br_target", "message": "bye"}, 400, "message is not used by a stop_session"},
		{"a stop with inputs", map[string]any{"label": "x", "type": "stop_session", "target_session_id": "br_target", "inputs": reply}, 400, "inputs is not used by a stop_session"},
		{"a target on a fork", map[string]any{"label": "x", "type": "fork_and_send", "target_session_id": "br_target", "message": "hi"}, 400, "target_session_id is not used by a fork_and_send"},
		{"a secret input", map[string]any{"label": "x", "type": "send_to_session", "target_session_id": "br_target", "message": "$BRIDGE_INPUT_REPLY",
			"inputs": []map[string]any{{"name": "REPLY", "label": "p", "secret": true}}}, 400, "cannot be secret"},
		{"an input the message never names", map[string]any{"label": "x", "type": "send_to_session", "target_session_id": "br_target", "message": "hi", "inputs": reply}, 400, "input REPLY is never used"},
		{"a message naming an input the button lacks", map[string]any{"label": "x", "type": "send_to_session", "target_session_id": "br_target", "message": "$BRIDGE_INPUT_OTHER"}, 400, "has no input OTHER"},
	}
	for _, testCase := range cases {
		recorder := offerAsInternalService(t, srv, "br_manager", testCase.offer)
		if recorder.Code != testCase.status || !strings.Contains(recorder.Body.String(), testCase.want) {
			t.Errorf("%s: %d %s, want %d %q", testCase.name, recorder.Code, recorder.Body, testCase.status, testCase.want)
		}
	}
}

func TestATargetThatWentAwayAfterTheOfferIsNotActedOn(t *testing.T) {
	srv, st := sessionsForTargetActions(t)
	offered := offeredAction(t, offerAsInternalService(t, srv, "br_manager", map[string]any{
		"label": "Stop it", "type": "stop_session", "target_session_id": "br_target",
	}))
	if err := st.DeleteSession("br_target"); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_manager", offered.ActionID))
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "unknown_target_session") {
		t.Errorf("run: %d %s, want 404 unknown_target_session", recorder.Code, recorder.Body)
	}
	if still, _ := st.GetSessionAction("br_manager", offered.ActionID); still.State != msg.SessionActionOffered {
		t.Errorf("state = %s, want offered", still.State)
	}
}

func TestTypedValuesTakeThePlacesTheMessageNamesThem(t *testing.T) {
	message := "a=$BRIDGE_INPUT_A b=${BRIDGE_INPUT_A_B}c and again $BRIDGE_INPUT_A."
	inputs := []msg.SessionActionInput{{Name: "A", Label: "a"}, {Name: "A_B", Label: "ab"}}
	if problem := sessionActionMessageInputsProblem(message, inputs); problem != "" {
		t.Fatalf("problem = %q", problem)
	}
	got := fillSessionActionMessageInputs(message, map[string]string{"A": "one", "A_B": "two"})
	if want := "a=one b=twoc and again one."; got != want {
		t.Errorf("filled = %q, want %q", got, want)
	}
}

// An administrator's session reaches any session, as the routes let an
// administrator reach any; a session of anyone else reaches only its own
// principal's. Producer's orchestrator runs as the operator, an
// administrator, and most sessions it manages have no principal at all.
func TestATargetSessionActionFollowsTheRoutesRuleForAdministrators(t *testing.T) {
	srv, st := sessionsForTargetActions(t)
	principalsByID := map[string]principalclient.Principal{
		"principal_000001": {ID: "principal_000001", Kind: "human", ActsAsCaller: true, IsAdministrator: true},
		"principal_000002": {ID: "principal_000002", Kind: "human", ActsAsCaller: true},
	}
	srv.principalLookupCache.readFromUpstream = func(_ context.Context, principalID string) (principalclient.Principal, error) {
		principal, found := principalsByID[principalID]
		if !found {
			return principalclient.Principal{}, principalclient.ErrNotFound
		}
		return principal, nil
	}
	for _, session := range []*store.Session{
		{SessionID: "br_administrators", Harness: msg.HarnessMock, State: string(msg.SessionIdle), PrincipalID: "principal_000001"},
		{SessionID: "br_someone_elses", Harness: msg.HarnessMock, State: string(msg.SessionIdle), PrincipalID: "principal_000002"},
	} {
		if err := st.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	stopTarget := map[string]any{"label": "Stop it", "type": "stop_session", "target_session_id": "br_target"}
	if recorder := offerAsInternalService(t, srv, "br_administrators", stopTarget); recorder.Code != http.StatusCreated {
		t.Errorf("administrator's session offering on an unowned session: %d %s, want 201", recorder.Code, recorder.Body)
	}
	if recorder := offerAsInternalService(t, srv, "br_someone_elses", stopTarget); recorder.Code != http.StatusNotFound {
		t.Errorf("another principal's session offering on an unowned session: %d %s, want 404", recorder.Code, recorder.Body)
	}
}
