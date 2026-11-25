package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the settings of the session_log module: log-store.
const (
	SettingSessionLogLogstackURL        = "session_log.logstack_url"
	SettingSessionLogSingleCopy         = "session_log.single_copy"
	SettingSessionLogAsynchronousCommit = "session_log.asynchronous_commit"
)

// sessionLogSettingDefinitions declares the settings above.
func sessionLogSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingSessionLogLogstackURL, EnvironmentVariable: "LLMBRIDGE_SESSION_LOG_LOGSTACK_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where logstack answers. The session log forwards each result event's statistics there, as the standalone log-store did; empty forwards nothing, and the start says so. A wrong address drops every one of them, which the startup probe and GET /log-store/health report."},
		{Key: SettingSessionLogSingleCopy, EnvironmentVariable: "LLMBRIDGE_SESSION_LOG_SINGLE_COPY", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether the session log is the only copy of session events. On, each event is written once, to the session log, in order, and the id that write answers is the SSE event id; the stream, crash recovery, the renamer, the brief pass, the reaper and wakeup redelivery read it there, and the bridge database's events table is written no more. Off, the old path: every event goes to the bridge database's events table, which serves those reads, and is queued to the session log. Read at start only. Turning it on the first time moves the session log's event numbering above the bridge database's highest id."},
		{Key: SettingSessionLogAsynchronousCommit, EnvironmentVariable: "LLMBRIDGE_SESSION_LOG_ASYNCHRONOUS_COMMIT", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether the session log's writes return before Postgres has them on disk (synchronous_commit off on its writer pool). On, a write waits only for Postgres to take the commit, and Postgres writes it to disk within three times wal_writer_delay (600 ms by default): a crash of this server loses nothing, but a crash of Postgres or of the machine loses that last stretch of events, which the stream may already have sent. Off, each write waits for the disk. Read at start only."},
	}
}
