package timeledger

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The store has no settings of its own: it runs inside llm-bridge-server,
// whose settings say where its Postgres is. What is declared here is
// time-ledger-review's, the one command that reads the environment.

// Keys of time-ledger-review's settings.
const (
	SettingBridgeURL          = "bridge_url"
	SettingBridgeServiceToken = "bridge_service_token"
	SettingModelStoreURL      = "model_store_url"
)

// ReviewName is time-ledger-review's name in its settings.
const ReviewName = "time-ledger-review"

// ReviewSettingDefinitions declares every environment variable
// time-ledger-review reads. Models are asked through llm-bridge-server's
// POST /oneshot, and their list prices come from model-store.
func ReviewSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingBridgeURL, EnvironmentVariable: "LLMBRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8160",
			Description: "Where llm-bridge-server answers POST /oneshot."},
		{Key: SettingBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "llm-bridge-server's service token, sent only to LLMBRIDGE_URL and never printed."},
		{Key: SettingModelStoreURL, EnvironmentVariable: "MODEL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8155",
			Description: "Where model-store answers GET /api/models, for each model's list price."},
	}
}

// NewReviewSettingsRegistry reads time-ledger-review's settings. It owns no
// prefix: LLMBRIDGE_ and MODEL_STORE_ belong to other services.
func NewReviewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ReviewName, nil, ReviewSettingDefinitions(), environment)
}
