package possessions

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The store runs inside llm-bridge-server, which declares its settings
// (internal/config). What is declared here is possession-add-photos', the
// command that brings photos in through the bridge.

// Keys of possession-add-photos' settings.
const (
	SettingBridgeURL      = "bridge_url"
	SettingPrincipalToken = "principal_token"
)

// AddPhotosSettingDefinitions declares every environment variable
// possession-add-photos reads. A chat session the bridge started as a
// principal has both.
func AddPhotosSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingBridgeURL, EnvironmentVariable: "LLM_BRIDGE_GATEWAY_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where llm-bridge-server answers: the original is kept at its /possession-store/, the photo added at its /photo-store/, and the review queued at its /possession-store/."},
		{Key: SettingPrincipalToken, EnvironmentVariable: "LLM_BRIDGE_PRINCIPAL_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The session agent token the bridge gives a session started as a principal. The photos, originals and reviews are that principal's."},
	}
}

// NewAddPhotosSettingsRegistry reads possession-add-photos' settings. It owns
// no prefix: both variables are the bridge's.
func NewAddPhotosSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("possession-add-photos", nil, AddPhotosSettingDefinitions(), environment)
}
