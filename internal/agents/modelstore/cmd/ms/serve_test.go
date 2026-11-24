package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	ms "github.com/kayushkin/model-store"
)

func newTestServer(t *testing.T) (*ms.Store, *httptest.Server) {
	t.Helper()
	store, err := ms.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.AddProvider(ms.Provider{ID: "anthropic", Name: "Anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddModel(ms.Model{
		ID: "claude-fable-5-1", Provider: "anthropic", Name: "Claude Fable 5.1",
		ShortName: "fable-5.1", MaxTokens: 1000000,
		InputCost: 10, OutputCost: 50, Enabled: true, Priority: 5,
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newServeHandler(store))
	t.Cleanup(server.Close)
	return store, server
}

func TestServeModelsCarriesShortName(t *testing.T) {
	_, server := newTestServer(t)

	resp, err := http.Get(server.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var models []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	m := models[0]
	// The 2026-04-06 binary served this route without short_name; the field
	// existing here is the point of the rewrite.
	if m["short_name"] != "fable-5.1" {
		t.Errorf("expected short_name fable-5.1, got %v", m["short_name"])
	}
	for _, field := range []string{"id", "provider", "input_cost", "output_cost", "enabled", "priority"} {
		if _, ok := m[field]; !ok {
			t.Errorf("model-poll reads %q from this response; it is missing", field)
		}
	}
}

func TestServeToggleFlipsEnabled(t *testing.T) {
	store, server := newTestServer(t)

	resp, err := http.Post(server.URL+"/api/models/toggle", "application/json",
		strings.NewReader(`{"model":"claude-fable-5-1","enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	m, err := store.ResolveModel("claude-fable-5-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Enabled {
		t.Error("toggle did not disable the model")
	}
}

func TestServeToggleUnknownModelIs400(t *testing.T) {
	_, server := newTestServer(t)
	resp, err := http.Post(server.URL+"/api/models/toggle", "application/json",
		strings.NewReader(`{"model":"no-such-model","enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for unknown model, got %d", resp.StatusCode)
	}
}

func TestServeCredentialRoutesAreGoneNot404(t *testing.T) {
	_, server := newTestServer(t)
	for _, path := range []string{"/api/credentials", "/api/credentials/toggle", "/api/models/test"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Errorf("%s: expected 410 Gone, got %d", path, resp.StatusCode)
		}
	}
}

func TestServeRolesGetSetResolve(t *testing.T) {
	store, server := newTestServer(t)
	if err := store.SetRole(ms.RoleDefault, "claude-fable-5-1"); err != nil {
		t.Fatal(err)
	}

	// GET /api/roles lists the assignment and the canonical set.
	resp, err := http.Get(server.URL + "/api/roles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Roles     map[string]string `json:"roles"`
		Canonical []string          `json:"canonical"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if listed.Roles["default"] != "claude-fable-5-1" {
		t.Fatalf("default role = %q, want claude-fable-5-1", listed.Roles["default"])
	}
	if len(listed.Canonical) != len(ms.CanonicalRoles) {
		t.Fatalf("canonical roles = %v, want %v", listed.Canonical, ms.CanonicalRoles)
	}

	// GET /api/roles/{role} resolves to the full model.
	resp2, err := http.Get(server.URL + "/api/roles/default")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var m ms.Model
	if err := json.NewDecoder(resp2.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "claude-fable-5-1" || m.MaxTokens != 1000000 {
		t.Fatalf("resolved model = %+v, want fable-5.1 with 1M context", m)
	}

	// An unassigned role is 404, not a fabricated fallback.
	resp3, err := http.Get(server.URL + "/api/roles/best")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("unassigned role status = %d, want 404", resp3.StatusCode)
	}
}
