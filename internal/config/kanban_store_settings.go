package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of kanban-store's settings in this server. Its other owners are this
// server's own settings (multichat.url, auth_store.*) and the file-store and
// reference-store modules in this process. Each is read once, at start.
const (
	SettingKanbanStoreMessageTriggersEnabled = "kanban_store.message_triggers.enabled"
)

// kanbanStoreSettingDefinitions declares the settings above.
func kanbanStoreSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingKanbanStoreMessageTriggersEnabled, EnvironmentVariable: "LLMBRIDGE_KANBAN_STORE_MESSAGE_TRIGGERS_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether kanban-store's message triggers send through multichat (multichat.url, with multichat's API token from auth-store). Off, a matching trigger still renders and its delivery is recorded not_configured, as the standalone store ran on this host. On with no multichat.url or no auth_store.token refuses to start. Read at start."},
	}
}
