package harness

import (
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// sessionEventsNotEnabled is the session event store before
// SetSessionEventStore names one: every store and read fails.
type sessionEventsNotEnabled struct{}

var _ sessionlog.Store = sessionEventsNotEnabled{}

func (sessionEventsNotEnabled) StoreSessionEvent(store.SessionEventRecord) (int64, error) {
	return 0, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) StoreSessionEventAdvancingRunnerSpawn(store.SessionEventRecord, string, string, uint64) (int64, error) {
	return 0, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) WriteSessionStatus(string, *msg.Event) (int64, error) {
	return 0, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) ReconcileSessions(...msg.SessionState) ([]store.ReconciledSession, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) MaxEventRowID(string) (int64, error) {
	return 0, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) MaxEventRowIDs([]string) (map[string]int64, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) ListEventsSinceID(string, int) ([]store.EventWithID, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) ListCurrentTurnEventsWithIDs(string) ([]store.EventWithID, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) StreamCursorFromEarlierNumbering(int64) bool { return false }

func (sessionEventsNotEnabled) HarnessToBridgeMap(string) (map[string]string, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) ToolUseToMessageMap(string) (map[string]store.ToolUseBinding, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) CountUserTurns(string) (int, error) {
	return 0, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) LastActivityAt(string) (time.Time, error) {
	return time.Time{}, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) CurrentTurnText(string) (string, []string, error) {
	return "", nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) FirstUserMessage(string) (string, error) {
	return "", ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) LatestScheduledWakeup(string, time.Time) (*store.ScheduledWakeup, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) UserMessageLoggedSince(string, time.Time) (bool, error) {
	return false, ErrSessionEventLogNotEnabled
}

func (sessionEventsNotEnabled) LastReportedBackgroundTasks(string) ([]msg.BackgroundTask, error) {
	return nil, ErrSessionEventLogNotEnabled
}
