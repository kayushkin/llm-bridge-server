// Package sessionlog is where the server keeps each session's events, and
// reads them back for the stream, crash recovery, the renamer, the brief
// pass, the reaper and wakeup redelivery.
//
// The session log is the only copy (SingleCopy): each event is written once,
// in order, and the id that write returns is the SSE event id. The bridge's
// SQLite events table, which held every event until the bridge's own records
// moved to Postgres, is archived history: nothing reads or writes it.
package sessionlog

import (
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Writes are the session event writes the harness manager makes.
type Writes interface {
	// StoreSessionEvent stores one event and answers its id.
	StoreSessionEvent(record store.SessionEventRecord) (int64, error)
	// StoreSessionEventAdvancingRunnerSpawn stores an event that came as
	// entry sequence of a runner spawn, and records the spawn as stored
	// through that entry, so a runner that sends the entry again after a
	// restart never gets it stored twice.
	StoreSessionEventAdvancingRunnerSpawn(record store.SessionEventRecord, spawnSessionID, spawnID string, sequence uint64) (int64, error)
	// WriteSessionStatus stores a session_status event, stamps its status
	// with the event's id as AsOf, and writes the status onto the session
	// row. sql.ErrNoRows means no such session, and nothing was stored.
	WriteSessionStatus(bridgeID string, ev *msg.Event) (int64, error)
	// ReconcileSessions resets the sessions a previous process left in one of
	// states, writing each one's status; see store.ReconcileSessions.
	ReconcileSessions(states ...msg.SessionState) ([]store.ReconciledSession, error)
}

// Reads are the reads of session events the server makes.
type Reads interface {
	// MaxEventRowID is the session's stream head: its highest event id.
	MaxEventRowID(sessionID string) (int64, error)
	// MaxEventRowIDs is the stream head of each session, 0 for one with no
	// events.
	MaxEventRowIDs(sessionIDs []string) (map[string]int64, error)
	// ListEventsSinceID is the session's events after afterID, as sent.
	ListEventsSinceID(sessionID string, afterID int) ([]store.EventWithID, error)
	// ListCurrentTurnEventsWithIDs is every event after the session's last
	// user_message but user messages.
	ListCurrentTurnEventsWithIDs(sessionID string) ([]store.EventWithID, error)
	// StreamCursorFromEarlierNumbering says a Last-Event-ID names an event
	// numbered by the archived events table, so events after it cannot be
	// found by id here.
	StreamCursorFromEarlierNumbering(cursor int64) bool
	HarnessToBridgeMap(sessionID string) (map[string]string, error)
	ToolUseToMessageMap(sessionID string) (map[string]store.ToolUseBinding, error)
	CountUserTurns(sessionID string) (int, error)
	LastActivityAt(sessionID string) (time.Time, error)
	CurrentTurnText(sessionID string) (userMessage string, assistantTexts []string, err error)
	FirstUserMessage(sessionID string) (string, error)
	LatestScheduledWakeup(bridgeID string, callsLoggedSince time.Time) (*store.ScheduledWakeup, error)
	UserMessageLoggedSince(bridgeID string, since time.Time) (bool, error)
	LastReportedBackgroundTasks(bridgeID string) ([]msg.BackgroundTask, error)
}

// Store is both.
type Store interface {
	Writes
	Reads
}

var _ Store = (*SingleCopy)(nil)
