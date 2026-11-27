package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/budget"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enableBudgetStoreOnTestDatabase serves budget-store from a Postgres
// database of the test's own.
func enableBudgetStoreOnTestDatabase(t *testing.T, gated *gatedTestServer) *budget.Store {
	t.Helper()
	enableConnectedRecordsOnTestDatabase(t, gated)
	return enableBudgetStoreModuleOnTestDatabase(t, gated)
}

// enableBudgetStoreModuleOnTestDatabase is enableBudgetStoreOnTestDatabase
// for a server whose connected records stores are already open.
func enableBudgetStoreModuleOnTestDatabase(t *testing.T, gated *gatedTestServer) *budget.Store {
	t.Helper()
	module, err := budget.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	store := budget.New(database.Open(module))
	if err := gated.server.EnableBudgetStore(store); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestBudgetStoreAnswers503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/budget-store/accounts", nil); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "budget_store_off") {
		t.Errorf("GET /budget-store/accounts = %d %s, want 503 budget_store_off", response.Code, response.Body)
	}
}

// An account is reached only by the person whose books it is in, an
// administrator and the internal service; a list holds only the caller's
// own; and the service acting as nobody names the books it works in.
func TestABudgetAccountIsReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableBudgetStoreOnTestDatabase(t, gated)
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/budget-store/accounts", map[string]any{"name": "Checking", "kind": "checking"})
	if created.Code != http.StatusCreated {
		t.Fatalf("the owner's POST = %d %s", created.Code, created.Body)
	}
	var account struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil || account.ID == "" {
		t.Fatalf("created %s: %v", created.Body, err)
	}
	path := "/budget-store/accounts/" + account.ID
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the owner's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", path, nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal's GET = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/budget-store/accounts", nil); response.Code != http.StatusOK || strings.Contains(response.Body.String(), account.ID) {
		t.Errorf("another principal's list = %d %s, want none of the owner's accounts", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", path); response.Code != http.StatusOK {
		t.Errorf("the internal service's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", "/budget-store/accounts"); response.Code != http.StatusForbidden {
		t.Errorf("the internal service's list naming no owner = %d, want 403", response.Code)
	}
	if response := gated.requestAsService(t, "GET", "/budget-store/accounts?owner_principal_id="+firstTestPrincipalID); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), account.ID) {
		t.Errorf("the internal service's list of the owner's books = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
		t.Errorf("nobody's GET = %d, want 401", response.Code)
	}

	// reference-store asks budget-store in this process as the caller: the
	// owner's chip resolves, and another principal is told of nothing.
	resolve := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{account.ID}})
	if resolve.Code != http.StatusOK || !strings.Contains(resolve.Body.String(), `"type":"budget_account"`) {
		t.Errorf("the administrator's resolve = %d %s", resolve.Code, resolve.Body)
	}
	other := gated.requestAsPrincipal(t, secondTestPrincipalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{account.ID}})
	if other.Code != http.StatusOK || strings.Contains(other.Body.String(), "Checking") {
		t.Errorf("another principal's resolve = %d %s, want nothing of the account", other.Code, other.Body)
	}
}

func TestBudgetStorePassesRefuseSettingsThatCouldNotRun(t *testing.T) {
	for name, settings := range map[string]budgetStorePassSettings{
		"bank sync with Plaid off": {owners: []string{firstTestPrincipalID}, interval: 1, bankSync: true},
		"trips with no markers":    {owners: []string{firstTestPrincipalID}, interval: 1, trips: true, tripTimeZoneName: "UTC", tripLookbackDays: 1, locationStoreIsServed: true},
		"trips with no time zone":  {owners: []string{firstTestPrincipalID}, interval: 1, trips: true, tripHomeMarkers: []string{"HOME TOWN"}, tripLookbackDays: 1, locationStoreIsServed: true},
		"a bad owner":              {owners: []string{"someone"}, interval: 1, interests: true},
	} {
		if err := checkBudgetStorePassSettings(settings); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkBudgetStorePassSettings(budgetStorePassSettings{}); err != nil {
		t.Errorf("every pass off: %v", err)
	}
}
