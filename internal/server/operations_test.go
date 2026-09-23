package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/executors/executorstest"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

// newOperationsTestServer is the gated test server with operations enabled.
// principal_000001 belongs to the group principal_000006; principal_000002
// belongs to nothing. Nothing runs until the test calls RunNext or Run.
func newOperationsTestServer(t *testing.T, classifier executors.ModelClassifier) (*gatedTestServer, *operations.Coordinator) {
	t.Helper()
	return newOperationsTestServerWithGrants(t, classifier, nil)
}

// testModelClassifier classifies with the fake harness: an item gets the
// values whose names appear in its text. delayPerCall slows each batch.
func testModelClassifier(delayPerCall time.Duration) executors.ModelClassifier {
	return executors.ModelClassifier{
		Caller:     &executorstest.FakeOneShot{InstanceID: "inst-test", DefaultModel: "test-model", DelayPerCall: delayPerCall},
		Taxonomies: executorstest.FakeTaxonomies{},
	}
}

// newOperationsTestServerWithGrants is newOperationsTestServer with grant-store
// answering from grantsByPrincipal, keyed "relation/resource_type".
func newOperationsTestServerWithGrants(t *testing.T, classifier executors.ModelClassifier, grantsByPrincipal map[string]map[string][]string) (*gatedTestServer, *operations.Coordinator) {
	t.Helper()
	gated := newGatedTestServer(t, grantsByPrincipal)
	gated.principals.mutex.Lock()
	gated.principals.groupIDsByMember = map[string][]string{firstTestPrincipalID: {groupTestPrincipalID}}
	gated.principals.mutex.Unlock()
	operationStore, err := operationstore.Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { operationStore.Close() })
	coordinator, err := operations.NewCoordinator(operationStore,
		operations.Config{WorkerCount: 1, LeaseDuration: 3 * time.Second, LeaseOwner: "test"}, classifier)
	if err != nil {
		t.Fatal(err)
	}
	gated.server.EnableOperations(coordinator)
	return gated, coordinator
}

func classificationIntentFor(organizationID, key string) map[string]any {
	return map[string]any{
		"type":            "classification.run",
		"organization_id": organizationID,
		"idempotency_key": key,
		"input": map[string]any{
			"taxonomy": map[string]any{"name": "support", "axes": []map[string]any{
				{"name": "category", "values": []map[string]any{{"name": "billing"}, {"name": "outage"}}}}},
			"items": []map[string]any{{"id": "mail_1", "text": "billing question"}, {"id": "mail_2", "text": "hi"}},
		},
	}
}

func decodeReceipt(t *testing.T, response *httptest.ResponseRecorder) msg.OperationReceipt {
	t.Helper()
	var receipt msg.OperationReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt %s: %v", response.Body.String(), err)
	}
	return receipt
}

func errorCodeOf(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(response.Body.Bytes(), &envelope)
	return envelope.Error.Code
}

func TestAMemberStartsAClassificationAndARepeatReturnsTheSameReceipt(t *testing.T) {
	gated, coordinator := newOperationsTestServer(t, testModelClassifier(0))
	cookie := gated.loginAs(t, firstTestPrincipalID)
	intent := classificationIntentFor(groupTestPrincipalID, "email-review-1")
	// A body naming someone else is ignored: the operation is the caller's.
	intent["principal_id"] = secondTestPrincipalID

	created := gated.requestAs(t, cookie, "POST", "/operations", intent)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	receipt := decodeReceipt(t, created)
	if receipt.PrincipalID != firstTestPrincipalID || receipt.State != msg.OperationStateQueued || created.Header().Get("Location") != "/operations/"+receipt.ID {
		t.Fatalf("receipt %+v location %q", receipt, created.Header().Get("Location"))
	}

	repeated := gated.requestAs(t, cookie, "POST", "/operations", intent)
	if repeated.Code != http.StatusOK || decodeReceipt(t, repeated).ID != receipt.ID {
		t.Fatalf("repeat = %d %s", repeated.Code, repeated.Body.String())
	}
	changed := classificationIntentFor(groupTestPrincipalID, "email-review-1")
	changed["input"].(map[string]any)["items"] = []map[string]any{{"id": "mail_9", "text": "other"}}
	if response := gated.requestAs(t, cookie, "POST", "/operations", changed); response.Code != http.StatusConflict || errorCodeOf(t, response) != "idempotency_key_reused" {
		t.Fatalf("reused key = %d %s", response.Code, response.Body.String())
	}

	if _, err := coordinator.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished := decodeReceipt(t, gated.requestAs(t, cookie, "GET", "/operations/"+receipt.ID, nil))
	if finished.State != msg.OperationStateSucceeded || finished.Progress.Completed != 2 {
		t.Fatalf("finished %+v", finished)
	}
	// Refreshing the page after the work finished still finds it by key.
	again := gated.requestAs(t, cookie, "POST", "/operations", intent)
	if again.Code != http.StatusOK || decodeReceipt(t, again).State != msg.OperationStateSucceeded {
		t.Fatalf("repeat after finish = %d %s", again.Code, again.Body.String())
	}
}

func TestTheOrganizationIsCheckedWithPrincipalStore(t *testing.T) {
	gated, _ := newOperationsTestServer(t, testModelClassifier(0))
	member := gated.loginAs(t, firstTestPrincipalID)
	outsider := gated.loginAs(t, secondTestPrincipalID)
	for _, check := range []struct {
		name           string
		cookie         *http.Cookie
		organizationID string
		status         int
		code           string
	}{
		{"not a member", outsider, groupTestPrincipalID, http.StatusForbidden, "not_a_member_of_organization"},
		{"a person is not an organization", member, secondTestPrincipalID, http.StatusBadRequest, "organization_not_group"},
		{"unknown", member, unknownTestPrincipalID, http.StatusBadRequest, "unknown_organization"},
		{"missing", member, "", http.StatusBadRequest, "organization_required"},
	} {
		response := gated.requestAs(t, check.cookie, "POST", "/operations", classificationIntentFor(check.organizationID, "k-"+check.name))
		if response.Code != check.status || errorCodeOf(t, response) != check.code {
			t.Errorf("%s: %d %s, want %d %s", check.name, response.Code, response.Body.String(), check.status, check.code)
		}
	}
	// An administrator need not be a member.
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	if response := gated.requestAs(t, administrator, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, "k-admin")); response.Code != http.StatusAccepted {
		t.Errorf("administrator: %d %s", response.Code, response.Body.String())
	}
	// Nor need the internal service, which starts operations as nobody.
	request := httptest.NewRequest("POST", "/operations", strings.NewReader(`{"type":"classification.run","organization_id":"principal_000006","idempotency_key":"k-service","input":{"taxonomy":{"name":"t","axes":[{"name":"a","values":[{"name":"b"}]}]},"items":[{"id":"1","text":"t"}]}}`))
	request.Header.Set(serviceTokenHeader, testServiceToken)
	response := serve(gated.server, request)
	if response.Code != http.StatusAccepted || decodeReceipt(t, response).PrincipalID != "" {
		t.Errorf("service: %d %s", response.Code, response.Body.String())
	}
}

func TestAnOperationIsReachableOnlyByItsPrincipal(t *testing.T) {
	gated, _ := newOperationsTestServer(t, testModelClassifier(0))
	owner := gated.loginAs(t, firstTestPrincipalID)
	other := gated.loginAs(t, secondTestPrincipalID)
	receipt := decodeReceipt(t, gated.requestAs(t, owner, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, "k1")))

	for _, route := range []struct{ method, path string }{
		{"GET", "/operations/" + receipt.ID},
		{"GET", "/operations/" + receipt.ID + "/children"},
		{"GET", "/operations/" + receipt.ID + "/events"},
		{"POST", "/operations/" + receipt.ID + "/cancel"},
	} {
		if response := gated.requestAs(t, other, route.method, route.path, nil); response.Code != http.StatusNotFound {
			t.Errorf("%s %s by another principal = %d, want 404", route.method, route.path, response.Code)
		}
	}
	var listed []msg.OperationReceipt
	json.Unmarshal(gated.requestAs(t, other, "GET", "/operations?principal_id="+firstTestPrincipalID, nil).Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Errorf("another principal listed %d operations", len(listed))
	}
	json.Unmarshal(gated.requestAs(t, owner, "GET", "/operations?state=queued", nil).Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].ID != receipt.ID {
		t.Errorf("owner listed %+v", listed)
	}
	if response := gated.requestAs(t, nil, "GET", "/operations/"+receipt.ID, nil); response.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", response.Code)
	}
	if response := gated.requestAs(t, owner, "GET", "/operations?state=partial", nil); response.Code != http.StatusBadRequest {
		t.Errorf("unknown state filter = %d, want 400", response.Code)
	}
}

func TestCancelAQueuedOperationThenCancelAgain(t *testing.T) {
	gated, _ := newOperationsTestServer(t, testModelClassifier(0))
	cookie := gated.loginAs(t, firstTestPrincipalID)
	receipt := decodeReceipt(t, gated.requestAs(t, cookie, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, "k1")))
	cancelled := gated.requestAs(t, cookie, "POST", "/operations/"+receipt.ID+"/cancel", nil)
	if cancelled.Code != http.StatusOK || decodeReceipt(t, cancelled).State != msg.OperationStateCancelled {
		t.Fatalf("cancel = %d %s", cancelled.Code, cancelled.Body.String())
	}
	again := gated.requestAs(t, cookie, "POST", "/operations/"+receipt.ID+"/cancel", nil)
	if again.Code != http.StatusConflict || errorCodeOf(t, again) != "operation_finished" {
		t.Fatalf("cancel again = %d %s", again.Code, again.Body.String())
	}
}

func TestOperationTypesListsTheClassifier(t *testing.T) {
	gated, _ := newOperationsTestServer(t, testModelClassifier(0))
	var types []msg.OperationTypeDescription
	json.Unmarshal(gated.requestAs(t, gated.loginAs(t, secondTestPrincipalID), "GET", "/operation-types", nil).Body.Bytes(), &types)
	if len(types) != 1 || types[0].Type != msg.OperationTypeClassificationRun {
		t.Fatalf("types %+v", types)
	}
}

// readOperationStream reads SSE frames until the server closes the stream.
func readOperationStream(t *testing.T, url string, cookie *http.Cookie, lastEventID string) []msg.OperationEvent {
	t.Helper()
	request, _ := http.NewRequest("GET", url, nil)
	request.AddCookie(cookie)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	var events []msg.OperationEvent
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var event msg.OperationEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatalf("frame %s: %v", data, err)
			}
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream ended with %v", err)
	}
	return events
}

// The page's view of one classification: submit, watch progress arrive, and
// see the stream end on the terminal receipt. A second stream resumed from
// the middle gets only what came after.
func TestTheEventStreamFollowsAnOperationToItsEnd(t *testing.T) {
	gated, coordinator := newOperationsTestServer(t, testModelClassifier(50 * time.Millisecond))
	listener := httptest.NewServer(gated.server)
	t.Cleanup(listener.Close)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go coordinator.Run(ctx)

	cookie := gated.loginAs(t, firstTestPrincipalID)
	receipt := decodeReceipt(t, gated.requestAs(t, cookie, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, "k1")))
	events := readOperationStream(t, listener.URL+"/operations/"+receipt.ID+"/events", cookie, "")

	var kinds []string
	for index, event := range events {
		kinds = append(kinds, string(event.Kind))
		if event.Sequence != int64(index+1) {
			t.Fatalf("event %d has sequence %d", index, event.Sequence)
		}
	}
	want := "accepted started progress progress progress succeeded"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("events %s, want %s", strings.Join(kinds, " "), want)
	}
	last := events[len(events)-1].Receipt
	if last.State != msg.OperationStateSucceeded || len(last.Result) == 0 {
		t.Fatalf("last receipt %+v", last)
	}

	resumed := readOperationStream(t, listener.URL+"/operations/"+receipt.ID+"/events", cookie, "4")
	if len(resumed) != 2 || resumed[0].Sequence != 5 || resumed[1].Kind != msg.OperationEventSucceeded {
		t.Fatalf("resumed %+v", resumed)
	}
	// A stream opened after the end sends nothing more and closes.
	if after := readOperationStream(t, listener.URL+"/operations/"+receipt.ID+"/events", cookie, "6"); len(after) != 0 {
		t.Fatalf("after the end: %+v", after)
	}
}

func (gated *gatedTestServer) requestAsServiceWithBody(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set(serviceTokenHeader, testServiceToken)
	return serve(gated.server, request)
}

func TestOperationGrantsAreLenientByDefaultAndStrictOnRequest(t *testing.T) {
	const runOperation = "can_run_operation/operation_type"
	gated, _ := newOperationsTestServerWithGrants(t, testModelClassifier(0), map[string]map[string][]string{
		firstTestPrincipalID:  {},
		secondTestPrincipalID: {runOperation: {"llm.completion"}},
	})
	gated.principals.mutex.Lock()
	gated.principals.groupIDsByMember[secondTestPrincipalID] = []string{groupTestPrincipalID}
	gated.principals.mutex.Unlock()
	ungranted := gated.loginAs(t, firstTestPrincipalID)
	grantedOtherType := gated.loginAs(t, secondTestPrincipalID)
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	submit := func(cookie *http.Cookie, key string) *httptest.ResponseRecorder {
		return gated.requestAs(t, cookie, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, key))
	}

	// Lenient: holding no grant of the relation restricts nothing; holding
	// one restricts to what it names.
	if response := submit(ungranted, "lenient-1"); response.Code != http.StatusAccepted {
		t.Fatalf("lenient, no grants: %d %s", response.Code, response.Body.String())
	}
	if response := submit(grantedOtherType, "lenient-2"); response.Code != http.StatusForbidden || errorCodeOf(t, response) != "not_granted" {
		t.Fatalf("lenient, granted another type: %d %s", response.Code, response.Body.String())
	}

	if response := gated.requestAsServiceWithBody(t, "PUT", "/settings/operations.grant_enforcement", `{"value":"sometimes"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("a value that is neither lenient nor strict: %d %s", response.Code, response.Body.String())
	}
	if response := gated.requestAsServiceWithBody(t, "PUT", "/settings/operations.grant_enforcement", `{"value":"strict"}`); response.Code != http.StatusOK {
		t.Fatalf("switch to strict: %d %s", response.Code, response.Body.String())
	}
	if response := submit(ungranted, "strict-1"); response.Code != http.StatusForbidden || errorCodeOf(t, response) != "not_granted" {
		t.Fatalf("strict, no grants: %d %s", response.Code, response.Body.String())
	}
	if response := submit(administrator, "strict-2"); response.Code != http.StatusAccepted {
		t.Fatalf("strict, administrator: %d %s", response.Code, response.Body.String())
	}
}

func TestOrganizationBudgetsAreSetByOperatorsAndStopNewWork(t *testing.T) {
	gated, coordinator := newOperationsTestServer(t, testModelClassifier(0))
	member := gated.loginAs(t, firstTestPrincipalID)
	budgetPath := "/operation-budgets/" + groupTestPrincipalID

	if response := gated.requestAs(t, member, "PUT", budgetPath, map[string]any{"monthly_limit_usd": 5}); response.Code != http.StatusForbidden {
		t.Fatalf("a principal setting a budget: %d", response.Code)
	}
	if response := gated.requestAsServiceWithBody(t, "PUT", "/operation-budgets/"+secondTestPrincipalID, `{"monthly_limit_usd":5}`); response.Code != http.StatusBadRequest || errorCodeOf(t, response) != "organization_not_group" {
		t.Fatalf("a budget on a person: %d %s", response.Code, response.Body.String())
	}
	if response := gated.requestAsServiceWithBody(t, "PUT", budgetPath, `{"monthly_limit_usd":-1}`); response.Code != http.StatusBadRequest {
		t.Fatalf("a negative budget: %d", response.Code)
	}
	set := gated.requestAsServiceWithBody(t, "PUT", budgetPath, `{"monthly_limit_usd":0}`)
	var budget msg.OrganizationBudget
	json.Unmarshal(set.Body.Bytes(), &budget)
	if set.Code != http.StatusOK || budget.OrganizationID != groupTestPrincipalID || budget.MonthStartsAt.Day() != 1 {
		t.Fatalf("set budget: %d %s", set.Code, set.Body.String())
	}
	refused := gated.requestAs(t, member, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, "k1"))
	if refused.Code != http.StatusPaymentRequired || errorCodeOf(t, refused) != "organization_budget_exhausted" {
		t.Fatalf("submit with no budget left: %d %s", refused.Code, refused.Body.String())
	}
	if response := gated.requestAsServiceWithBody(t, "DELETE", budgetPath, ""); response.Code != http.StatusNoContent {
		t.Fatalf("delete budget: %d", response.Code)
	}
	if response := gated.requestAs(t, member, "POST", "/operations", classificationIntentFor(groupTestPrincipalID, "k1")); response.Code != http.StatusAccepted {
		t.Fatalf("submit after the budget was removed: %d %s", response.Code, response.Body.String())
	}
	var listed []msg.OrganizationBudget
	json.Unmarshal(gated.requestAsService(t, "GET", "/operation-budgets").Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Fatalf("budgets after delete: %+v", listed)
	}
	_ = coordinator
}

func TestOperationTypesNeedNoCredential(t *testing.T) {
	gated, _ := newOperationsTestServer(t, testModelClassifier(0))
	response := gated.requestAs(t, nil, "GET", "/operation-types", nil)
	var types []msg.OperationTypeDescription
	json.Unmarshal(response.Body.Bytes(), &types)
	if response.Code != http.StatusOK || len(types) != 1 {
		t.Fatalf("anonymous /operation-types: %d %s", response.Code, response.Body.String())
	}
}
