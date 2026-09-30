package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeSessionActionStores is repo-store's GET /repos/{id} and the scheduler's
// job routes, as session actions use them.
type fakeSessionActionStores struct {
	mu               sync.Mutex
	repoPaths        map[int64]string
	schedulerJobName string
	schedulerRuns    []string
}

func newFakeSessionActionStores(t *testing.T) (*fakeSessionActionStores, *httptest.Server) {
	fake := &fakeSessionActionStores{repoPaths: map[int64]string{}, schedulerJobName: "nightly-digest"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		var id int64
		switch {
		case r.Method == http.MethodGet && sscanPath(r.URL.Path, "/repos/%d", &id):
			path, ok := fake.repoPaths[id]
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "name": filepath.Base(path), "path": path})
		case r.Method == http.MethodGet && sscanPath(r.URL.Path, "/api/jobs/%d", &id) && !strings.HasSuffix(r.URL.Path, "/run"):
			if id != 76 {
				http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "name": fake.schedulerJobName, "type": "shell", "command": "/bin/true"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/jobs/76/run":
			fake.schedulerRuns = append(fake.schedulerRuns, r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": 9001, "status": "running"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func sscanPath(path, format string, id *int64) bool {
	_, err := fmt.Sscanf(path, format, id)
	return err == nil
}

func testServerForSessionActions(t *testing.T) (*Server, *store.Store, *fakeSessionActionStores) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake, storesServer := newFakeSessionActionStores(t)
	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     fakeLogStore(t).URL,
		RepoStoreURL:    storesServer.URL,
		SchedulerURL:    storesServer.URL,
		ListenAddr:      "127.0.0.1:8160",
	}
	testAuthorizationConfig(cfg)
	srv := New(st, nil, nil, nil, nil, testModelStore(t), nil, cfg)
	return srv, st, fake
}

// repoWithDeployScript makes a repo directory whose deploy.sh writes what it
// was run with to a file beside it.
func repoWithDeployScript(t *testing.T, script string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "demo-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "deploy.sh"), []byte("#!/usr/bin/env bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

func postingTokenOf(t *testing.T, srv *Server, session *store.Session) string {
	t.Helper()
	for _, variable := range srv.sessionPostingEnvironment(session) {
		if name, value, _ := strings.Cut(variable, "="); name == sessionPostingTokenEnvironmentVariable {
			return value
		}
	}
	t.Fatalf("no %s in the child environment", sessionPostingTokenEnvironmentVariable)
	return ""
}

func offerRequest(sessionID string, offer map[string]any) *http.Request {
	body, _ := json.Marshal(offer)
	request := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/actions", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func runRequest(sessionID, actionID string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/actions/"+actionID+"/run", nil)
}

func waitForSessionActionToEnd(t *testing.T, st *store.Store, sessionID, actionID string) store.SessionAction {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		action, err := st.GetSessionAction(sessionID, actionID)
		if err != nil {
			t.Fatal(err)
		}
		if action.State != msg.SessionActionRunning {
			return action
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s still running after 20s", actionID)
	return store.SessionAction{}
}

func TestAnAgentOffersADeployAndOnlyThePersonCanRunIt(t *testing.T) {
	srv, st, fake := testServerForSessionActions(t)
	session := newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	newSessionForSignals(t, st, "br_neighbour", msg.SessionTypeInteractive)
	repo := repoWithDeployScript(t, `printf '%s|%s|%s' "$PWD" "$LLM_BRIDGE_SESSION_ID" "$AI_AGENT" > ran.txt; echo deployed`)
	fake.repoPaths[12] = repo
	events := srv.harness.Subscribe("br_buttons")
	token := postingTokenOf(t, srv, session)
	withToken := func(request *http.Request) *httptest.ResponseRecorder {
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		return recorder
	}

	recorder := withToken(offerRequest("br_buttons", map[string]any{"label": "Deploy demo", "type": "deploy", "repo_id": 12}))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
	}
	var offered msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &offered)
	wantCommand := fmt.Sprintf("run `bash -l -c ./deploy.sh` in %s (repo-store repo 12, demo-repo)", repo)
	if offered.ActionID != "session_action_000001" || offered.State != msg.SessionActionOffered || offered.Command != wantCommand || offered.Offer.Label != "Deploy demo" {
		t.Fatalf("offered = %+v", offered)
	}
	select {
	case event := <-events:
		if event.Type != msg.EventSessionAction || event.SessionAction == nil || event.SessionAction.ActionID != offered.ActionID {
			t.Errorf("event = %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session_action event for the offer")
	}

	if code := withToken(offerRequest("br_neighbour", map[string]any{"label": "Deploy demo", "type": "deploy", "repo_id": 12})).Code; code != http.StatusNotFound {
		t.Errorf("agent offering into another session: %d, want 404", code)
	}
	// The agent cannot press its own button.
	if code := withToken(runRequest("br_buttons", offered.ActionID)).Code; code != http.StatusUnauthorized {
		t.Errorf("agent running its own action: %d, want 401", code)
	}
	if _, err := os.Stat(filepath.Join(repo, "ran.txt")); err == nil {
		t.Fatal("deploy.sh ran before anyone confirmed it")
	}

	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", offered.ActionID))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", recorder.Code, recorder.Body)
	}
	finished := waitForSessionActionToEnd(t, st, "br_buttons", offered.ActionID)
	if finished.State != msg.SessionActionSucceeded || strings.TrimSpace(finished.Output) != "deployed" || finished.StartedAt == nil || finished.FinishedAt == nil {
		t.Fatalf("finished = %+v", finished)
	}
	ran, err := os.ReadFile(filepath.Join(repo, "ran.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := repo + "|br_buttons|chat button session_action_000001 confirmed by the internal service"
	if string(ran) != want {
		t.Errorf("deploy.sh ran with %q, want %q", ran, want)
	}

	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", offered.ActionID))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "session_action_already_run") {
		t.Errorf("second run: %d %s, want 409 session_action_already_run", recorder.Code, recorder.Body)
	}

	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/br_buttons/actions", nil))
	var listed []msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].State != msg.SessionActionSucceeded {
		t.Errorf("listed = %+v", listed)
	}
}

func TestAFailingDeployIsRecordedAsFailedWithItsOutput(t *testing.T) {
	srv, st, fake := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	fake.repoPaths[3] = repoWithDeployScript(t, "echo gate refused; exit 3")

	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, offerRequest("br_buttons", map[string]any{"label": "Deploy", "type": "deploy", "repo_id": 3}))
	var offered msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &offered)
	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", offered.ActionID))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", recorder.Code, recorder.Body)
	}
	finished := waitForSessionActionToEnd(t, st, "br_buttons", offered.ActionID)
	if finished.State != msg.SessionActionFailed || !strings.Contains(finished.Output, "gate refused") || !strings.Contains(finished.Error, "exit status 3") {
		t.Errorf("finished = %+v", finished)
	}
}

func TestASchedulerJobActionRunsTheJobAndRefusesOneThatChangedSinceItWasOffered(t *testing.T) {
	srv, st, fake := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	offer := func() msg.SessionAction {
		recorder := httptest.NewRecorder()
		asInternalService(srv).ServeHTTP(recorder, offerRequest("br_buttons", map[string]any{"label": "Run the digest", "type": "run_scheduler_job", "scheduler_job_id": 76}))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
		}
		var offered msg.SessionAction
		json.Unmarshal(recorder.Body.Bytes(), &offered)
		return offered
	}

	first := offer()
	if !strings.Contains(first.Command, "/api/jobs/76/run") || !strings.Contains(first.Command, "nightly-digest") || !strings.Contains(first.Command, "/bin/true") {
		t.Errorf("command = %q", first.Command)
	}
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", first.ActionID))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", recorder.Code, recorder.Body)
	}
	finished := waitForSessionActionToEnd(t, st, "br_buttons", first.ActionID)
	if finished.State != msg.SessionActionSucceeded || !strings.Contains(finished.Output, `"status":"running"`) || len(fake.schedulerRuns) != 1 {
		t.Errorf("finished = %+v, runs %v", finished, fake.schedulerRuns)
	}

	second := offer()
	fake.mu.Lock()
	fake.schedulerJobName = "renamed-digest"
	fake.mu.Unlock()
	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", second.ActionID))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "session_action_changed") {
		t.Errorf("run after the job changed: %d %s, want 409 session_action_changed", recorder.Code, recorder.Body)
	}
	if still, _ := st.GetSessionAction("br_buttons", second.ActionID); still.State != msg.SessionActionOffered {
		t.Errorf("refused action state = %s, want offered", still.State)
	}
}

func TestAnOfferIsRefusedWhenItIsMalformedOrNamesNothing(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	cases := []struct {
		name  string
		offer map[string]any
		want  int
		code  string
	}{
		{"no label", map[string]any{"type": "send_message", "message": "hi"}, 400, "invalid_session_action_offer"},
		{"unknown type", map[string]any{"label": "x", "type": "run_shell", "message": "rm -rf /"}, 400, "invalid_session_action_offer"},
		{"a command field", map[string]any{"label": "x", "type": "deploy", "repo_id": 1, "command": "rm -rf /"}, 400, "invalid_session_action_offer"},
		{"deploy without repo", map[string]any{"label": "x", "type": "deploy"}, 400, "invalid_session_action_offer"},
		{"deploy with a message", map[string]any{"label": "x", "type": "deploy", "repo_id": 1, "message": "hi"}, 400, "invalid_session_action_offer"},
		{"a reply button, which only repeats a question", map[string]any{"label": "Yes", "type": "send_message", "message": "yes"}, 400, "AskUserQuestion"},
		{"fork without message", map[string]any{"label": "x", "type": "fork_and_send"}, 400, "invalid_session_action_offer"},
		{"fork with a job", map[string]any{"label": "x", "type": "fork_and_send", "message": "hi", "scheduler_job_id": 76}, 400, "invalid_session_action_offer"},
		{"repo-store has no such repo", map[string]any{"label": "x", "type": "deploy", "repo_id": 404}, 422, "unknown_record"},
		{"scheduler has no such job", map[string]any{"label": "x", "type": "run_scheduler_job", "scheduler_job_id": 5}, 422, "unknown_record"},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		asInternalService(srv).ServeHTTP(recorder, offerRequest("br_buttons", testCase.offer))
		if recorder.Code != testCase.want || !strings.Contains(recorder.Body.String(), testCase.code) {
			t.Errorf("%s: %d %s, want %d %s", testCase.name, recorder.Code, recorder.Body, testCase.want, testCase.code)
		}
	}
	if listed, _ := st.ListSessionActions("br_buttons"); len(listed) != 0 {
		t.Errorf("refused offers were stored: %+v", listed)
	}
}

func TestADeployRepoWithoutADeployScriptIsRefused(t *testing.T) {
	srv, st, fake := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	fake.repoPaths[7] = t.TempDir()
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, offerRequest("br_buttons", map[string]any{"label": "Deploy", "type": "deploy", "repo_id": 7}))
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "repo_has_no_deploy_script") {
		t.Errorf("offer: %d %s", recorder.Code, recorder.Body)
	}
}

func TestActionsLeftRunningByAStoppedServerAreSettledAsOutcomeUnknown(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	action := store.SessionAction{
		SessionID: "br_buttons",
		Offer:     msg.SessionActionOffer{Label: "Say hi", Type: msg.SessionActionSendMessage, Message: "hi"},
		Command:   "send session br_buttons this message: hi",
		State:     msg.SessionActionOffered,
		OfferedAt: time.Now().UTC(),
	}
	if err := st.InsertSessionAction(&action); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartSessionAction("br_buttons", action.ActionID, "principal_000001", time.Now()); err != nil {
		t.Fatal(err)
	}
	srv.settleSessionActionsLeftRunning()
	settled, err := st.GetSessionAction("br_buttons", action.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != msg.SessionActionOutcomeUnknown || settled.RunByPrincipalID != "principal_000001" || settled.FinishedAt == nil || settled.Error == "" {
		t.Errorf("settled = %+v", settled)
	}
}

func TestTheChildEnvironmentCarriesTheActionsAddressWithoutFileStore(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	session := newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	environment := map[string]string{}
	for _, variable := range srv.sessionPostingEnvironment(session) {
		name, value, _ := strings.Cut(variable, "=")
		environment[name] = value
	}
	if environment[sessionActionsURLEnvironmentVariable] != "http://127.0.0.1:8160/sessions/br_buttons/actions" || environment[sessionPostingTokenEnvironmentVariable] == "" {
		t.Errorf("environment = %v", environment)
	}
	if _, has := environment[sessionFilesURLEnvironmentVariable]; has {
		t.Errorf("a files address with no file-store: %v", environment)
	}
}

// fakeSessionActionModels resolves any named model, answers a review with a
// set verdict and any other call with a set answer, and prices every model at
// $1 in and $10 out per million tokens.
type fakeSessionActionModels struct {
	mu            sync.Mutex
	verdict       string
	reviewFails   bool
	answer        string
	requests      []msg.OneShotRequest
	instanceCalls []string
}

func (fake *fakeSessionActionModels) OneShotRoute(requestedModel string) (oneshotrouting.Route, error) {
	if requestedModel == "" {
		return oneshotrouting.Route{}, &executors.TargetError{Code: "no_model", Message: "no model named"}
	}
	return oneshotrouting.Route{Requested: requestedModel, Role: requestedModel,
		Candidates: []oneshotrouting.Candidate{{ModelID: requestedModel + "-resolved", Provider: "mock"}}}, nil
}

func (fake *fakeSessionActionModels) RunOneShotRoute(ctx context.Context, route oneshotrouting.Route, request msg.OneShotRequest, admit func(oneshotrouting.Candidate) string) (msg.OneShotResponse, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	request.Model = route.Candidates[0].ModelID
	fake.requests = append(fake.requests, request)
	fake.instanceCalls = append(fake.instanceCalls, "inst-fake")
	if len(request.Schema) > 0 {
		if fake.reviewFails {
			return msg.OneShotResponse{}, errors.New("reviewer unreachable")
		}
		return msg.OneShotResponse{Model: request.Model, InstanceID: "inst-fake", Parsed: json.RawMessage(fmt.Sprintf(`{"verdict":%q,"reasons":"it lists two lines"}`, fake.verdict))}, nil
	}
	return msg.OneShotResponse{Model: request.Model, InstanceID: "inst-fake", Text: fake.answer, Usage: msg.TokenUsage{InputTokens: 100, OutputTokens: 200}}, nil
}

func (fake *fakeSessionActionModels) ModelListPrice(model string) (float64, float64, bool) {
	return 1, 10, true
}

func withFakeSessionActionModels(srv *Server, verdict string) *fakeSessionActionModels {
	fake := &fakeSessionActionModels{verdict: verdict, answer: "## Tech events\n- Go meetup, Thursday"}
	srv.sessionActionModels = fake
	return fake
}

func offerAsInternalService(t *testing.T, srv *Server, sessionID string, offer map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, offerRequest(sessionID, offer))
	return recorder
}

func TestARunCommandIsReviewedWhenOfferedAndItsOutputIsItsResult(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	models := withFakeSessionActionModels(srv, "approve")
	directory := t.TempDir()

	recorder := offerAsInternalService(t, srv, "br_buttons", map[string]any{
		"label": "List attendees", "type": "run_command", "shell_command": "printf -- '- Ana\\n- Bo\\n'",
		"working_directory": directory, "result_format": "markdown",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
	}
	var offered msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &offered)
	if offered.Review == nil || offered.Review.Verdict != msg.SessionActionReviewApprove || offered.Review.Model != "balanced-resolved" {
		t.Fatalf("review = %+v", offered.Review)
	}
	if !strings.Contains(models.requests[0].Prompt, "printf -- '- Ana") || !strings.Contains(models.requests[0].Prompt, directory) {
		t.Errorf("the reviewer was not shown the command and its directory: %q", models.requests[0].Prompt)
	}
	if want := "in " + directory + ", run with `bash -l -c`:\nprintf -- '- Ana\\n- Bo\\n'"; offered.Command != want {
		t.Errorf("command = %q, want %q", offered.Command, want)
	}

	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", offered.ActionID))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", recorder.Code, recorder.Body)
	}
	finished := waitForSessionActionToEnd(t, st, "br_buttons", offered.ActionID)
	if finished.State != msg.SessionActionSucceeded || finished.Output != "- Ana\n- Bo\n" || finished.Offer.ResultFormat != msg.SessionActionResultMarkdown || finished.Review == nil {
		t.Errorf("finished = %+v", finished)
	}
}

func TestACommandTheReviewerRejectsCannotBeRun(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	withFakeSessionActionModels(srv, "reject")
	directory := t.TempDir()

	recorder := offerAsInternalService(t, srv, "br_buttons", map[string]any{
		"label": "Tidy up", "type": "run_command", "shell_command": "touch ran.txt", "working_directory": directory,
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
	}
	var offered msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &offered)
	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", offered.ActionID))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "session_action_rejected_by_review") {
		t.Errorf("run: %d %s, want 409 session_action_rejected_by_review", recorder.Code, recorder.Body)
	}
	if _, err := os.Stat(filepath.Join(directory, "ran.txt")); err == nil {
		t.Error("a rejected command ran")
	}
}

func TestACommandIsNotOfferedWhenItCannotBeReviewed(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	withFakeSessionActionModels(srv, "approve").reviewFails = true

	recorder := offerAsInternalService(t, srv, "br_buttons", map[string]any{
		"label": "List", "type": "run_command", "shell_command": "ls", "working_directory": t.TempDir(),
	})
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "review_failed") {
		t.Errorf("offer: %d %s, want 502 review_failed", recorder.Code, recorder.Body)
	}
	recorder = offerAsInternalService(t, srv, "br_buttons", map[string]any{"label": "List", "type": "run_command", "shell_command": "ls"})
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "no_working_directory") {
		t.Errorf("offer with no directory anywhere: %d %s, want 422 no_working_directory", recorder.Code, recorder.Body)
	}
	if listed, _ := st.ListSessionActions("br_buttons"); len(listed) != 0 {
		t.Errorf("refused offers were stored: %+v", listed)
	}
}

func TestAModelCallKeepsToItsCostLimitAndItsAnswerIsItsResult(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	models := withFakeSessionActionModels(srv, "approve")

	// $0.01 at $10 per million output tokens buys 1,000 tokens; the 38-byte prompt,
	// counted as 13.7 tokens at $1 per million, leaves 998.
	recorder := offerAsInternalService(t, srv, "br_buttons", map[string]any{
		"label": "Find tech events", "type": "model_call", "model": "balanced", "maximum_cost_usd": 0.01,
		"message": "List tech events in Seattle this week.",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offer: %d %s", recorder.Code, recorder.Body)
	}
	var offered msg.SessionAction
	json.Unmarshal(recorder.Body.Bytes(), &offered)
	if !strings.Contains(offered.Command, "balanced-resolved") || !strings.Contains(offered.Command, "$0.01") || !strings.Contains(offered.Command, "at most 998 output tokens") {
		t.Errorf("command = %q", offered.Command)
	}

	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, runRequest("br_buttons", offered.ActionID))
	finished := waitForSessionActionToEnd(t, st, "br_buttons", offered.ActionID)
	if finished.State != msg.SessionActionSucceeded || finished.Output != models.answer {
		t.Fatalf("finished = %+v", finished)
	}
	if want := 100*1/1e6 + 200*10/1e6; math.Abs(finished.CostUSD-want) > 1e-12 {
		t.Errorf("cost = %v, want %v", finished.CostUSD, want)
	}
	last := models.requests[len(models.requests)-1]
	if last.MaxTokens != 998 || last.Model != "balanced-resolved" || models.instanceCalls[len(models.instanceCalls)-1] != "inst-fake" {
		t.Errorf("model request = %+v", last)
	}

	recorder = offerAsInternalService(t, srv, "br_buttons", map[string]any{
		"label": "Too cheap", "type": "model_call", "model": "balanced", "maximum_cost_usd": 0.0001, "message": "hi",
	})
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "cost_limit_too_low") {
		t.Errorf("cheap offer: %d %s, want 422 cost_limit_too_low", recorder.Code, recorder.Body)
	}
}

func TestEachNewKindTakesOnlyItsOwnFields(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	newSessionForSignals(t, st, "br_buttons", msg.SessionTypeInteractive)
	withFakeSessionActionModels(srv, "approve")
	cases := []struct {
		name  string
		offer map[string]any
		want  string
	}{
		{"a model call with no cost limit", map[string]any{"label": "x", "type": "model_call", "model": "m", "message": "hi"}, "needs maximum_cost_usd"},
		{"a model call with no model", map[string]any{"label": "x", "type": "model_call", "maximum_cost_usd": 1, "message": "hi"}, "needs model"},
		{"a background agent with no cost limit", map[string]any{"label": "x", "type": "background_agent", "message": "go"}, "needs maximum_cost_usd"},
		{"a command with a message", map[string]any{"label": "x", "type": "run_command", "shell_command": "ls", "message": "hi"}, "message is not used by a run_command"},
		{"a deploy with a command", map[string]any{"label": "x", "type": "deploy", "repo_id": 1, "shell_command": "ls"}, "shell_command is not used by a deploy"},
		{"a relative directory", map[string]any{"label": "x", "type": "run_command", "shell_command": "ls", "working_directory": "repos"}, "absolute path"},
		{"an unknown result format", map[string]any{"label": "x", "type": "run_command", "shell_command": "ls", "result_format": "html"}, "neither text nor markdown"},
	}
	for _, testCase := range cases {
		recorder := offerAsInternalService(t, srv, "br_buttons", testCase.offer)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), testCase.want) {
			t.Errorf("%s: %d %s, want 400 %q", testCase.name, recorder.Code, recorder.Body, testCase.want)
		}
	}
}

func TestABackgroundAgentsFinalReplyIsItsResult(t *testing.T) {
	srv, _, _ := testServerForSessionActions(t)
	events := make(chan harness.StoredEvent, 8)
	events <- harness.StoredEvent{Event: msg.Event{Type: msg.EventStream}}
	events <- harness.StoredEvent{Event: msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "Found 3 events"}}}
	outcome := srv.waitForFinalReply("br_background", events)
	if outcome.err != nil || outcome.output != "Found 3 events" || outcome.resultSessionID != "br_background" {
		t.Errorf("outcome = %+v", outcome)
	}

	events = make(chan harness.StoredEvent, 8)
	events <- harness.StoredEvent{Event: msg.Event{Type: msg.EventTurnComplete}}
	if outcome := srv.waitForFinalReply("br_background", events); outcome.err == nil {
		t.Error("a turn that ended with no reply seen counted as a success")
	}

	config, err := harnessConfigWithModel(json.RawMessage(`{"permission_mode":"bypassPermissions","model":"old","model_selection":{"model":"old"}}`), "efficient")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	json.Unmarshal(config, &fields)
	if fields["model"] != "efficient" || fields["model_selection"] != nil || fields["permission_mode"] != "bypassPermissions" {
		t.Errorf("config = %s", config)
	}
}
