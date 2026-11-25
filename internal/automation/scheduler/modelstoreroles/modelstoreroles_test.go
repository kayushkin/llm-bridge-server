package modelstoreroles

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

func serveRoles(t *testing.T, body string) Checker {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/roles" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return New(server.URL)
}

const rolesBody = `{"canonical":["best","default","balanced","efficient"],
 "models":{"balanced":["model-a","model-b"],"best":["model-c"],"default":["model-c"],"efficient":[]}}`

func TestCheckRoleAcceptsARoleWithModels(t *testing.T) {
	if err := serveRoles(t, rolesBody).CheckRole("balanced"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRoleRefusesAnUnknownRoleAndAnEmptyOne(t *testing.T) {
	checker := serveRoles(t, rolesBody)
	if err := checker.CheckRole("cheapest"); err == nil || !strings.Contains(err.Error(), "no role \"cheapest\"") {
		t.Errorf("cheapest: err = %v, want a refusal naming it", err)
	}
	if err := checker.CheckRole("efficient"); err == nil || !strings.Contains(err.Error(), "names no model") {
		t.Errorf("efficient with no models: err = %v, want a refusal", err)
	}
}

func TestCheckRoleSaysWhenModelStoreCannotBeAsked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if err := New(server.URL).CheckRole("balanced"); !errors.Is(err, servicesettings.ErrSettingOwnerUnavailable) {
		t.Errorf("503: err = %v, want ErrSettingOwnerUnavailable", err)
	}
	if err := New("http://127.0.0.1:1").CheckRole("balanced"); !errors.Is(err, servicesettings.ErrSettingOwnerUnavailable) {
		t.Errorf("unreachable: err = %v, want ErrSettingOwnerUnavailable", err)
	}
}
