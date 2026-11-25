// Package modelstoreroles checks a model-role name against model-store, which
// owns the set of roles and the models each one means.
package modelstoreroles

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Roles is model-store as a role check reads it: every role name it accepts,
// in order, and each role's ordered model list. The scheduler runs inside
// llm-bridge-server, which hands it model-store in process.
type Roles interface {
	CanonicalRoleNames() []string
	RoleModelLists() (map[string][]string, error)
}

// Checker asks model-store whether a role exists and names at least one model.
type Checker struct {
	Roles Roles
}

// CheckRole is a servicesettings validator. It refuses a role model-store does
// not list, or one with no model behind it, and returns an error wrapping
// servicesettings.ErrSettingOwnerUnavailable when model-store cannot be asked.
func (checker Checker) CheckRole(role string) error {
	if checker.Roles == nil {
		return fmt.Errorf("%w: model-store: this scheduler was given no model-store", servicesettings.ErrSettingOwnerUnavailable)
	}
	lists, err := checker.Roles.RoleModelLists()
	if err != nil {
		return fmt.Errorf("%w: model-store: %v", servicesettings.ErrSettingOwnerUnavailable, err)
	}
	canonical := checker.Roles.CanonicalRoleNames()
	if !slices.Contains(canonical, role) {
		return fmt.Errorf("model-store has no role %q; its roles are %s", role, strings.Join(canonical, ", "))
	}
	if len(lists[role]) == 0 {
		return fmt.Errorf("model-store's role %q names no model", role)
	}
	return nil
}
