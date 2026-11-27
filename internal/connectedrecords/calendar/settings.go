package calendar

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of google sync's settings.
const (
	SettingDashURL   = "dash_url"
	SettingDashToken = "dash_token"
)

// GoogleSyncName is calendar-store-sync-google's name in its settings.
const GoogleSyncName = "calendar-store-sync-google"

// GoogleSyncSettingDefinitions declares every environment variable
// calendar-store-sync-google reads. dash is the one way to Google Calendar on
// a host: the command reads a calendar there and hands the events to the
// store, so the store never holds the token.
func GoogleSyncSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingDashURL, EnvironmentVariable: "DASH_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8101",
			Description: "Where dash answers: the calendar's events are read from its GET /api/calendar/google."},
		{Key: SettingDashToken, EnvironmentVariable: "DASH_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "dash's bearer token, sent only to DASH_URL and never printed."},
	}
}

// NewGoogleSyncSettingsRegistry reads calendar-store-sync-google's settings.
// It owns no prefix: DASH_ belongs to dash.
func NewGoogleSyncSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(GoogleSyncName, nil, GoogleSyncSettingDefinitions(), environment)
}
