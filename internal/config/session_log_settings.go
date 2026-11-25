package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the settings of the session_log module: log-store.
const (
	SettingSessionLogLogstackURL        = "session_log.logstack_url"
	SettingSessionLogAsynchronousCommit = "session_log.asynchronous_commit"
)

// sessionLogSettingDefinitions declares the settings above.
func sessionLogSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingSessionLogLogstackURL, EnvironmentVariable: "LLMBRIDGE_SESSION_LOG_LOGSTACK_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where logstack answers. The session log forwards each result event's statistics there, as the standalone log-store did; empty forwards nothing, and the start says so. A wrong address drops every one of them, which the startup probe and GET /log-store/health report."},
		{Key: SettingSessionLogAsynchronousCommit, EnvironmentVariable: "LLMBRIDGE_SESSION_LOG_ASYNCHRONOUS_COMMIT", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether the session log's writes return before Postgres has them on disk (synchronous_commit off on its writer pool). On, a write waits only for Postgres to take the commit, and Postgres writes it to disk within three times wal_writer_delay (600 ms by default): a crash of this server loses nothing, but a crash of Postgres or of the machine loses that last stretch of events, which the stream may already have sent. Off, each write waits for the disk. Read at start only."},
	}
}
