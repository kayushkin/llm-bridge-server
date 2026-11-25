// Package modelstoreroles checks a model-role name against model-store, which
// owns the set of roles and the models each one means.
package modelstoreroles

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// rolesAnswer is the part of model-store's GET /api/roles this package reads.
type rolesAnswer struct {
	// Canonical is every role name model-store accepts, in order.
	Canonical []string `json:"canonical"`
	// Models is each role's ordered model list: the first model, then the
	// fallbacks.
	Models map[string][]string `json:"models"`
}

// Checker asks model-store whether a role exists and names at least one model.
type Checker struct {
	ModelStoreURL string
	HTTPClient    *http.Client
}

// New returns a Checker for the model-store at modelStoreURL.
func New(modelStoreURL string) Checker {
	return Checker{ModelStoreURL: modelStoreURL, HTTPClient: &http.Client{Timeout: 10 * time.Second}}
}

// CheckRole is a servicesettings validator. It refuses a role model-store does
// not list, or one with no model behind it, and returns an error wrapping
// servicesettings.ErrSettingOwnerUnavailable when model-store cannot be asked.
func (checker Checker) CheckRole(role string) error {
	url := strings.TrimRight(checker.ModelStoreURL, "/") + "/api/roles"
	response, err := checker.HTTPClient.Get(url)
	if err != nil {
		return fmt.Errorf("%w: model-store: %v", servicesettings.ErrSettingOwnerUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("%w: model-store: read %s: %v", servicesettings.ErrSettingOwnerUnavailable, url, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: model-store: GET %s: %s: %s", servicesettings.ErrSettingOwnerUnavailable, url, response.Status, strings.TrimSpace(string(body)))
	}
	var answer rolesAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return fmt.Errorf("%w: model-store: decode GET %s: %v", servicesettings.ErrSettingOwnerUnavailable, url, err)
	}
	if !slices.Contains(answer.Canonical, role) {
		return fmt.Errorf("model-store has no role %q; its roles are %s", role, strings.Join(answer.Canonical, ", "))
	}
	if len(answer.Models[role]) == 0 {
		return fmt.Errorf("model-store's role %q names no model", role)
	}
	return nil
}
