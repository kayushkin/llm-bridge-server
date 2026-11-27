package main

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const commandName = "budget-suggest-categories"

// ownedPrefix is this command's own. A set variable carrying it that the
// definitions below do not name stops the run.
const ownedPrefix = "BUDGET_SUGGEST_"

const (
	settingBudgetStoreURL = "budget_store_url"
	settingBridgeURL      = "bridge_url"
	settingBridgeToken    = "bridge_token"
	settingOrganizationID = "organization_id"
	settingModelRole      = "model_role"
)

func settingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: settingBudgetStoreURL, EnvironmentVariable: "BUDGET_SUGGEST_BUDGET_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8340",
			Description: "Where budget-store answers: the candidates are read from it and the suggestions written to it."},
		{Key: settingBridgeURL, EnvironmentVariable: "BUDGET_SUGGEST_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where llm-bridge-server answers: each batch is one classification.run operation there."},
		{Key: settingBridgeToken, EnvironmentVariable: "BUDGET_SUGGEST_BRIDGE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "A principal token llm-bridge-server accepts as a bearer; the operations run as that principal."},
		{Key: settingOrganizationID, EnvironmentVariable: "BUDGET_SUGGEST_ORGANIZATION_ID", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The principal-store group the model calls are made for; its budget and grants apply."},
		{Key: settingModelRole, EnvironmentVariable: "BUDGET_SUGGEST_MODEL_ROLE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Default: "best",
			Description: "The model-store role that categorizes. The receipt records which model answered."},
	}
}

func newSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(commandName, []string{ownedPrefix}, settingDefinitions(), environment)
}
