package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge/msg"
)

// model-store's routes answer from the registry under /model-store/, with
// the paths the standalone `ms serve` had, to the operator only.
func TestModelStoreRoutesServeTheRegistryUnderThePrefix(t *testing.T) {
	gated := newGatedTestServer(t, nil)

	models := gated.requestAsService(t, "GET", "/model-store/api/models")
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"mock-model"`) {
		t.Fatalf("GET /model-store/api/models = %d %s", models.Code, models.Body)
	}
	resolved := gated.requestAsService(t, "GET", "/model-store/api/roles/default")
	var model struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resolved.Body.Bytes(), &model); resolved.Code != http.StatusOK || err != nil || model.ID != "mock-model" {
		t.Fatalf("GET /model-store/api/roles/default = %d %s", resolved.Code, resolved.Body)
	}
	set := gated.requestAsServiceWithJSON(t, "POST", "/model-store/api/roles", map[string]any{"role": "best", "models": []string{"mock-model-alt", "mock-model"}})
	if set.Code != http.StatusOK {
		t.Fatalf("POST /model-store/api/roles = %d %s", set.Code, set.Body)
	}
	if list := gated.requestAsService(t, "GET", "/model-store/api/roles/best/models"); !strings.Contains(list.Body.String(), `"id":"mock-model-alt"`) {
		t.Fatalf("GET /model-store/api/roles/best/models = %d %s", list.Code, list.Body)
	}
	if unknown := gated.requestAsService(t, "GET", "/model-store/api/roles/nonexistent"); unknown.Code != http.StatusNotFound {
		t.Errorf("an unknown role = %d %s, want 404", unknown.Code, unknown.Body)
	}

	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/model-store/api/models", nil); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "operator_route") {
		t.Errorf("a principal who is not an administrator = %d %s, want 403 operator_route", response.Code, response.Body)
	}
}

// A runner reaches agent-store's four seed routes in this process with its
// bearer token, and no other agent-store route through /api/agent-store/.
func TestAgentStoreSeedRoutesAnswerARunnerInProcess(t *testing.T) {
	srv := newServerWithAllStores(t)
	if err := srv.harnessStore.CreateMachine(&msg.Machine{ID: "m_runner", Name: "runner", Transport: msg.TransportRunner}); err != nil {
		t.Fatal(err)
	}
	const token = "runner-token-for-this-test"
	if err := srv.harnessStore.SetMachineRunnerTokenHash("m_runner", machines.HashRunnerToken(token)); err != nil {
		t.Fatal(err)
	}
	get := func(path, bearer string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("GET", path, nil)
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		return recorder
	}

	manifest := get("/api/agent-store/seed/manifest?machine_id=m_runner", token)
	if manifest.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(manifest.Body.String()), "{") {
		t.Fatalf("the seed manifest = %d %s", manifest.Code, manifest.Body)
	}
	if missing := get("/api/agent-store/seed/manifest", token); missing.Code != http.StatusBadRequest {
		t.Errorf("the manifest without machine_id = %d %s, want the store's own 400", missing.Code, missing.Body)
	}
	if unauthorized := get("/api/agent-store/seed/manifest?machine_id=m_runner", "not-a-runner-token"); unauthorized.Code != http.StatusUnauthorized {
		t.Errorf("a wrong runner token = %d %s, want 401", unauthorized.Code, unauthorized.Body)
	}
	if other := get("/api/agent-store/agents", token); other.Code == http.StatusOK {
		t.Errorf("GET /api/agent-store/agents = %d %s; only the four seed routes are mounted", other.Code, other.Body)
	}
}
