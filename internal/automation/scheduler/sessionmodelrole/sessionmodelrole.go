// Package sessionmodelrole names the model-store role a new llm-bridge-server
// session runs on, after checking that the session's harness can run the model
// the role stands for.
//
// A session this repo starts without naming a model takes the per-harness
// default on the bridge's Settings page (bridge-prefs.defaults.<harness>.model),
// one value shared by every session on that harness. Set to a cheap model for
// an afternoon, it moved every scheduled agent onto that model with nothing in
// the scheduler saying so. So every session this repo starts names a role, and
// the role is a scheduler setting or a job field.
//
// llm-bridge-server takes the role in harness_config.model, resolves it to the
// role's FIRST model, and records that the session chose it. It does not try
// the role's later models, and at create it does not check that the harness
// can run the model: a role whose first model is an OpenAI model would be
// pinned on a claude_code session and fail only when the harness spawned. So
// Check asks model-store what the role resolves to and llm-bridge-server which
// providers the harness runs, and refuses the pair before any session exists.
package sessionmodelrole

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// harnessConfigKeyModel is the harness_config key llm-bridge-server reads the
// session's own model choice from; a canonical role name is accepted there.
const harnessConfigKeyModel = "model"

// Model is the model a role resolves to, as model-store describes it.
type Model struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// RoleChecker says which model a session on a harness would start on for a
// role, and refuses a model the harness cannot run. Checker is the production
// one; a caller takes the interface so a test can stand in for both services.
type RoleChecker interface {
	Check(role string, harness msg.Harness) (Model, error)
}

var _ RoleChecker = Checker{}

// Checker asks model-store and llm-bridge-server whether a role can start a
// session on a harness.
type Checker struct {
	ModelStoreURL string
	BridgeURL     string
	// HTTPClient carries both calls. Its Transport is left nil so the call to
	// the bridge goes out through the default transport bridgeauth stamps
	// with the service token.
	HTTPClient *http.Client
}

// New returns a Checker for the model-store and llm-bridge-server at the
// given addresses.
func New(modelStoreURL, bridgeURL string) Checker {
	return Checker{ModelStoreURL: modelStoreURL, BridgeURL: bridgeURL, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// Check returns the model a session on harness would start on if it asked for
// role: the role's first model, as llm-bridge-server resolves it. It fails when
// either service cannot answer, when model-store has no such role, when the
// bridge does not know the harness, and when the harness cannot run the
// model's provider.
func (checker Checker) Check(role string, harness msg.Harness) (Model, error) {
	if role == "" {
		return Model{}, fmt.Errorf("no model role to start a %s session on", harness)
	}
	model, err := checker.firstModel(role)
	if err != nil {
		return Model{}, err
	}
	providers, err := checker.harnessProviders(harness)
	if err != nil {
		return Model{}, err
	}
	// The bridge sends no list for a harness that runs any provider.
	if providers != nil && !slices.Contains(providers, model.Provider) {
		return Model{}, fmt.Errorf("role %q starts a session on %s, a %s model, and harness %s runs only %s models; name a role whose first model it can run, or change the role's models in model-store",
			role, model.ID, model.Provider, harness, strings.Join(providers, ", "))
	}
	return model, nil
}

// firstModel reads model-store's GET /api/roles/{role}, the role's first model.
func (checker Checker) firstModel(role string) (Model, error) {
	address := strings.TrimRight(checker.ModelStoreURL, "/") + "/api/roles/" + url.PathEscape(role)
	body, err := checker.get(address)
	if err != nil {
		return Model{}, fmt.Errorf("resolve model role %q with model-store: %w", role, err)
	}
	var model Model
	if err := json.Unmarshal(body, &model); err != nil {
		return Model{}, fmt.Errorf("resolve model role %q: decode GET %s: %w", role, address, err)
	}
	if model.ID == "" || model.Provider == "" {
		return Model{}, fmt.Errorf("resolve model role %q: model-store answered a model with no id or no provider: %s", role, strings.TrimSpace(string(body)))
	}
	return model, nil
}

// harnessProviders reads the providers llm-bridge-server's GET /harnesses says
// harness runs. nil means any provider.
func (checker Checker) harnessProviders(harness msg.Harness) ([]string, error) {
	address := strings.TrimRight(checker.BridgeURL, "/") + "/harnesses"
	body, err := checker.get(address)
	if err != nil {
		return nil, fmt.Errorf("read the providers harness %s runs from llm-bridge-server: %w", harness, err)
	}
	var harnesses []struct {
		Name               string   `json:"name"`
		SupportedProviders []string `json:"supported_providers"`
	}
	if err := json.Unmarshal(body, &harnesses); err != nil {
		return nil, fmt.Errorf("decode GET %s: %w", address, err)
	}
	for _, described := range harnesses {
		if described.Name == string(harness) {
			return described.SupportedProviders, nil
		}
	}
	return nil, fmt.Errorf("llm-bridge-server at %s does not know harness %q", checker.BridgeURL, harness)
}

func (checker Checker) get(address string) ([]byte, error) {
	response, err := checker.HTTPClient.Get(address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read GET %s: %w", address, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", address, response.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// SetModelRole writes role into create's harness_config as the session's own
// model choice, keeping every other key already there.
func SetModelRole(create *msg.CreateSessionRequest, role string) error {
	if role == "" {
		return fmt.Errorf("set model role: the role is empty")
	}
	config := map[string]json.RawMessage{}
	if len(create.HarnessConfig) > 0 {
		if err := json.Unmarshal(create.HarnessConfig, &config); err != nil {
			return fmt.Errorf("set model role: harness_config is not a JSON object: %w", err)
		}
	}
	if existing, ok := config[harnessConfigKeyModel]; ok {
		return fmt.Errorf("set model role %q: harness_config already names a model (%s)", role, existing)
	}
	encodedRole, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("set model role: %w", err)
	}
	config[harnessConfigKeyModel] = encodedRole
	merged, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("set model role: %w", err)
	}
	create.HarnessConfig = merged
	return nil
}
