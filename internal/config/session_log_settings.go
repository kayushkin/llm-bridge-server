package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the settings of the session_log module: log-store.
const (
	SettingSessionLogLogstackURL = "session_log.logstack_url"
)

// sessionLogSettingDefinitions declares the settings above.
func sessionLogSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingSessionLogLogstackURL, EnvironmentVariable: "LLMBRIDGE_SESSION_LOG_LOGSTACK_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where logstack answers. The session log forwards each result event's statistics there, as the standalone log-store did; empty forwards nothing, and the start says so. A wrong address drops every one of them, which the startup probe and GET /log-store/health report."},
	}
}
