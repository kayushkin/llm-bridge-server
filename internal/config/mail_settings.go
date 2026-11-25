package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of mailstack's settings in this server. mailstack keeps its records in
// the mail module of database.postgres_url, fetches Gmail and Outlook tokens
// from auth-store with this server's auth_store.url and auth_store.token, and
// keeps attachment bytes in the file-store module; it has no address, token
// or path of its own. Each is read once, at start.
const (
	SettingMailstackProviderConnectionsEnabled = "mailstack.provider_connections.enabled"
)

// mailSettingDefinitions declares the settings above.
func mailSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingMailstackProviderConnectionsEnabled, EnvironmentVariable: "LLMBRIDGE_MAILSTACK_PROVIDER_CONNECTIONS_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether mailstack's Gmail, Outlook and IMAP accounts reach their mail providers, with tokens from auth-store (auth_store.url, auth_store.token) and IMAP logins from the mail module. Off, every read or change on such an account answers 503 mail_provider_connections_off, nothing asks auth-store for a token and no mail is fetched; local accounts answer either way. mailstack sends no mail on any setting. On with no auth_store.token refuses to start. Read at start."},
	}
}
