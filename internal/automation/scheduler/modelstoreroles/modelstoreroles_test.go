package modelstoreroles

import (
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// fakeRoles is model-store with four roles, one of which names no model.
type fakeRoles struct{ failure error }

func (roles fakeRoles) CanonicalRoleNames() []string {
	return []string{"best", "default", "balanced", "efficient"}
}

func (roles fakeRoles) RoleModelLists() (map[string][]string, error) {
	if roles.failure != nil {
		return nil, roles.failure
	}
	return map[string][]string{"balanced": {"model-a", "model-b"}, "best": {"model-c"}, "default": {"model-c"}, "efficient": {}}, nil
}

func TestCheckRoleAcceptsARoleWithModels(t *testing.T) {
	if err := (Checker{Roles: fakeRoles{}}).CheckRole("balanced"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRoleRefusesAnUnknownRoleAndAnEmptyOne(t *testing.T) {
	checker := Checker{Roles: fakeRoles{}}
	if err := checker.CheckRole("cheapest"); err == nil || !strings.Contains(err.Error(), "no role \"cheapest\"") {
		t.Errorf("cheapest: err = %v, want a refusal naming it", err)
	}
	if err := checker.CheckRole("efficient"); err == nil || !strings.Contains(err.Error(), "names no model") {
		t.Errorf("efficient with no models: err = %v, want a refusal", err)
	}
}

func TestCheckRoleSaysWhenModelStoreCannotBeAsked(t *testing.T) {
	if err := (Checker{Roles: fakeRoles{failure: errors.New("down")}}).CheckRole("balanced"); !errors.Is(err, servicesettings.ErrSettingOwnerUnavailable) {
		t.Errorf("a failing model-store: err = %v, want ErrSettingOwnerUnavailable", err)
	}
	if err := (Checker{}).CheckRole("balanced"); !errors.Is(err, servicesettings.ErrSettingOwnerUnavailable) {
		t.Errorf("no model-store: err = %v, want ErrSettingOwnerUnavailable", err)
	}
}
