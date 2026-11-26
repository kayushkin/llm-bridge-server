package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/evalstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usagestore"
	usagestorehttpapi "github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/httpapi"
	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore/operationstoretest"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

type limitsWatchedNowhere struct{}

func (limitsWatchedNowhere) MarkWatched() {}

// enableUsageStoreOnTestDatabase opens usage-store on a Postgres database of
// the test's own and serves it from server, with no provider to read.
func enableUsageStoreOnTestDatabase(t *testing.T, server *Server) *usagestore.Store {
	t.Helper()
	module, err := usagestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	store := usagestore.New(postgrestest.NewDatabase(t, module).Open(module))
	server.EnableUsageStore(usagestorehttpapi.New(store, nil, nil, nil, limitsWatchedNowhere{}, 45*time.Minute, usagestorehttpapi.SpendCollectors{}))
	return store
}

func openTestEvalStore(t *testing.T) *evalstore.Store {
	t.Helper()
	module, err := evalstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	return evalstore.New(postgrestest.NewDatabase(t, module).Open(module))
}

func saveUsageSnapshot(t *testing.T, store *usagestore.Store, provider, window string, usedPercent float64) {
	t.Helper()
	if err := store.SaveLimits(usagestore.ProviderLimits{Provider: provider, SnapshotAt: time.Now().Unix(), Source: "api",
		Windows: map[string]*usagestore.LimitWindow{window: {UsedPercent: usedPercent}}}, nil); err != nil {
		t.Fatal(err)
	}
}

// Every route usage-store's handler serves is mounted under /usage-store/
// with a rule, so none of them is left unreachable or unclassified.
func TestEveryUsageStoreRouteIsMountedUnderThePrefix(t *testing.T) {
	for _, pattern := range usagestorehttpapi.RoutePatterns {
		method, path, _ := strings.Cut(pattern, " ")
		mounted := method + " " + usageStoreRoutePrefix + path
		if _, classified := usageAndEvalStoreRouteAccessRules[mounted]; !classified {
			t.Errorf("usage-store serves %s, and %s has no rule", pattern, mounted)
		}
	}
	if len(usagestorehttpapi.RoutePatterns) != strings.Count(strings.Join(mapKeys(usageAndEvalStoreRouteAccessRules), "\n"), " /usage-store/") {
		t.Errorf("the mounted usage-store routes and the store's own differ in number")
	}
}

func mapKeys(rules map[string]routeAccessRule) []string {
	keys := make([]string, 0, len(rules))
	for key := range rules {
		keys = append(keys, key)
	}
	return keys
}

// usage-store answers under /usage-store/ with its own paths, to the operator
// only, and 503 until its module is open.
func TestUsageStoreRoutesAnswerUnderThePrefixToTheOperatorOnly(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	if off := gated.requestAsService(t, "GET", "/usage-store/api/usage/limits"); off.Code != http.StatusServiceUnavailable || !strings.Contains(off.Body.String(), "usage_store_off") {
		t.Fatalf("before the module opens = %d %s, want 503 usage_store_off", off.Code, off.Body)
	}
	store := enableUsageStoreOnTestDatabase(t, gated.server)
	saveUsageSnapshot(t, store, "anthropic", "seven_day", 42)

	limits := gated.requestAsService(t, "GET", "/usage-store/api/usage/limits")
	var providers map[string]*usagestore.ProviderLimits
	if err := json.Unmarshal(limits.Body.Bytes(), &providers); limits.Code != http.StatusOK || err != nil {
		t.Fatalf("GET /usage-store/api/usage/limits = %d %s", limits.Code, limits.Body)
	}
	if providers["anthropic"] == nil || providers["anthropic"].Windows["seven_day"].UsedPercent != 42 || providers["anthropic"].StaleAfter == nil || providers["codex"] != nil {
		t.Fatalf("limits = %s", limits.Body)
	}
	added := gated.requestAsServiceWithJSON(t, "POST", "/usage-store/api/usage/spend/topups", map[string]any{"provider": "anthropic", "amount_usd": 20, "occurred_at": 100})
	if added.Code != http.StatusOK || !strings.Contains(added.Body.String(), `"id":1`) {
		t.Fatalf("POST a top-up = %d %s", added.Code, added.Body)
	}
	if deleted := gated.requestAsService(t, "DELETE", "/usage-store/api/usage/spend/topups/1"); deleted.Code != http.StatusNoContent {
		t.Errorf("DELETE the top-up = %d %s", deleted.Code, deleted.Body)
	}
	if health := gated.requestAsService(t, "GET", "/usage-store/health"); health.Code != http.StatusOK {
		t.Errorf("GET /usage-store/health = %d %s", health.Code, health.Body)
	}
	if refused := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/usage-store/api/usage/limits", nil); refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "operator_route") {
		t.Errorf("a principal who is not an administrator = %d %s, want 403 operator_route", refused.Code, refused.Body)
	}
	if administrator := gated.requestAsAdministrator(t, "GET", "/usage-store/api/usage/limits", nil); administrator.Code != http.StatusOK {
		t.Errorf("an administrator = %d %s, want 200", administrator.Code, administrator.Body)
	}
}

// The one-shot router reads usage-store in this process: a full, fresh window
// is reported for the subscription it bills, and a server whose module is not
// open is an error the router logs and calls through, never a skip.
func TestTheOneShotRouterReadsUsageStoreLimitsInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	if _, err := gated.server.oneShotRouter.Limits.ExhaustedWindow(context.Background(), oneshotrouting.UsageProviderByHarness[msg.HarnessClaudeCode]); err == nil {
		t.Fatal("a usage-store that is not open read as no limit instead of an error")
	}
	store := enableUsageStoreOnTestDatabase(t, gated.server)
	saveUsageSnapshot(t, store, "anthropic", "seven_day", 100)
	// The router keeps one answer for 30 seconds, and the failed read above
	// kept none, so this read reaches the store.
	exhausted, err := gated.server.oneShotRouter.Limits.ExhaustedWindow(context.Background(), "anthropic")
	if err != nil || exhausted != "seven_day limit at 100%" {
		t.Fatalf("ExhaustedWindow = %q, %v", exhausted, err)
	}
	if codex, err := gated.server.oneShotRouter.Limits.ExhaustedWindow(context.Background(), "codex"); err != nil || codex != "" {
		t.Errorf("a subscription usage-store has no snapshot of = %q, %v, want nothing exhausted", codex, err)
	}
}

// answerEveryCallWith answers every one-shot call with the same parsed JSON.
type answerEveryCallWith struct{ parsed json.RawMessage }

func (answer answerEveryCallWith) OneShotRoute(requestedModel string) (oneshotrouting.Route, error) {
	return oneshotrouting.Route{Requested: requestedModel, Candidates: []oneshotrouting.Candidate{{ModelID: requestedModel, Provider: "mock"}}}, nil
}

func (answer answerEveryCallWith) RunOneShotRoute(_ context.Context, route oneshotrouting.Route, _ msg.OneShotRequest, _ func(oneshotrouting.Candidate) string) (msg.OneShotResponse, error) {
	return msg.OneShotResponse{Model: route.Candidates[0].ModelID, InstanceID: "inst_test", Parsed: answer.parsed, StopReason: "end_turn",
		Usage: msg.TokenUsage{InputTokens: 10, OutputTokens: 2}}, nil
}

// eval-store answers under /eval-store/ to the operator only, reads
// model-store's prices in process, and refuses to start a run while runs are
// off.
func TestEvalStoreRoutesAnswerAndRunsStayOffUntilTheSettingIsOn(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	if off := gated.requestAsService(t, "GET", "/eval-store/suites"); off.Code != http.StatusServiceUnavailable || !strings.Contains(off.Body.String(), "eval_store_off") {
		t.Fatalf("before the module opens = %d %s, want 503 eval_store_off", off.Code, off.Body)
	}
	if _, err := gated.server.EnableEvalStore(openTestEvalStore(t), EvalStoreOptions{Defaults: evalstore.RunDefaults{Concurrency: 1, BudgetCheckEveryCalls: 1}}); err != nil {
		t.Fatal(err)
	}
	runID := createSyntheticEvalRun(t, gated)
	refused := gated.requestAsService(t, "POST", "/eval-store/runs/"+runID+"/start")
	if refused.Code != http.StatusServiceUnavailable || !strings.Contains(refused.Body.String(), "eval_store.runs.enabled") {
		t.Fatalf("start while runs are off = %d %s, want 503 naming the setting", refused.Code, refused.Body)
	}
	if principal := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/eval-store/suites", nil); principal.Code != http.StatusForbidden || !strings.Contains(principal.Body.String(), "operator_route") {
		t.Errorf("a principal who is not an administrator = %d %s, want 403 operator_route", principal.Code, principal.Body)
	}
}

// createSyntheticEvalRun makes a made-up suite with one made-up case and a
// run of the two test models over it, and answers the run's id. The run's
// prices are read from model-store through this server, in process.
func createSyntheticEvalRun(t *testing.T, gated *gatedTestServer) string {
	t.Helper()
	suite := gated.requestAsServiceWithJSON(t, "POST", "/eval-store/suites", map[string]any{"name": "synthetic", "task_kind": "synthetic",
		"judge_instructions": "Judge it.", "judge_failure_kinds": []string{"none", "no_answer"}, "output_schema": map[string]any{"type": "object"},
		"max_tokens": 100, "judge_max_tokens": 100})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(suite.Body.Bytes(), &created); suite.Code != http.StatusCreated || err != nil {
		t.Fatalf("POST /eval-store/suites = %d %s", suite.Code, suite.Body)
	}
	if c := gated.requestAsServiceWithJSON(t, "POST", "/eval-store/suites/"+created.ID+"/cases", map[string]any{"source_kind": "llm-bridge-operation",
		"source_ref": "operation_synthetic", "system_prompt": "Be brief.", "user_prompt": "Say something made up."}); c.Code != http.StatusCreated {
		t.Fatalf("POST a case = %d %s", c.Code, c.Body)
	}
	run := gated.requestAsServiceWithJSON(t, "POST", "/eval-store/runs", map[string]any{"suite_id": created.ID,
		"candidate_model_ids": []string{"mock-model", "mock-model-alt"}, "judge_model_ids": []string{"mock-model"},
		"case_selection": map[string]any{"mode": "all"}, "organization_id": groupTestPrincipalID})
	var createdRun struct {
		ID          string                          `json:"id"`
		ModelPrices map[string]evalstore.ModelPrice `json:"model_prices"`
	}
	if err := json.Unmarshal(run.Body.Bytes(), &createdRun); run.Code != http.StatusCreated || err != nil {
		t.Fatalf("POST /eval-store/runs = %d %s", run.Code, run.Body)
	}
	if createdRun.ModelPrices["mock-model-alt"].Provider != "mock" {
		t.Fatalf("the run's prices were not read from model-store: %s", run.Body)
	}
	return createdRun.ID
}

// With runs on, a run's every call is an llm.completion operation on this
// server and its budget reading comes from usage-store, both in process: the
// answers and verdicts carry the operations' ids.
func TestAnEvalStoreRunMakesItsCallsAsOperationsInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	operationStore := operationstoretest.NewStore(t)
	coordinator, err := operations.NewCoordinator(operationStore, operations.Config{WorkerCount: 2, LeaseDuration: 3 * time.Second, LeaseOwner: "test"},
		executors.LLMCompletion{Caller: answerEveryCallWith{parsed: json.RawMessage(`{"made_up":true}`)}})
	if err != nil {
		t.Fatal(err)
	}
	gated.server.EnableOperations(coordinator)
	working, stopWorking := context.WithCancel(context.Background())
	t.Cleanup(stopWorking)
	go coordinator.Run(working)

	usage := enableUsageStoreOnTestDatabase(t, gated.server)
	saveUsageSnapshot(t, usage, "anthropic", "seven_day", 10)
	runner, err := gated.server.EnableEvalStore(openTestEvalStore(t), EvalStoreOptions{RunsEnabled: true, OperationPollingInterval: 10 * time.Millisecond,
		Defaults: evalstore.RunDefaults{Concurrency: 2, BudgetCheckEveryCalls: 2, AnswerTimeout: 30 * time.Second, JudgeTimeout: 30 * time.Second,
			BudgetLimits: []evalstore.BudgetLimit{{Subscription: "anthropic", Window: "seven_day", MaxUsedPercent: 40}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	runID := createSyntheticEvalRun(t, gated)
	if started := gated.requestAsService(t, "POST", "/eval-store/runs/"+runID+"/start"); started.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", started.Code, started.Body)
	}
	runner.Wait()

	var run evalstore.Run
	read := gated.requestAsService(t, "GET", "/eval-store/runs/"+runID)
	if err := json.Unmarshal(read.Body.Bytes(), &run); err != nil || run.Status != evalstore.RunStatusCompleted {
		t.Fatalf("the run ended %s: %s", run.Status, read.Body)
	}
	var listedAnswers struct {
		Answers []evalstore.Answer `json:"answers"`
	}
	listed := gated.requestAsService(t, "GET", "/eval-store/runs/"+runID+"/answers?include_bodies=true")
	if err := json.Unmarshal(listed.Body.Bytes(), &listedAnswers); err != nil || len(listedAnswers.Answers) != 2 {
		t.Fatalf("want an answer from each candidate, got %d %s", listed.Code, listed.Body)
	}
	for _, answer := range listedAnswers.Answers {
		if !strings.HasPrefix(answer.OperationID, "operation_") || answer.Error != "" || string(answer.Parsed) != `{"made_up":true}` || answer.InputTokens != 10 {
			t.Errorf("answer %+v was not made by an operation on this server", answer)
		}
	}
	var listedVerdicts struct {
		Verdicts []evalstore.Verdict `json:"verdicts"`
	}
	verdicts := gated.requestAsService(t, "GET", "/eval-store/runs/"+runID+"/verdicts")
	if err := json.Unmarshal(verdicts.Body.Bytes(), &listedVerdicts); err != nil || len(listedVerdicts.Verdicts) != 1 ||
		!strings.HasPrefix(listedVerdicts.Verdicts[0].OperationID, "operation_") {
		t.Errorf("want the judge's verdict made by an operation, got %d %s", verdicts.Code, verdicts.Body)
	}
}
