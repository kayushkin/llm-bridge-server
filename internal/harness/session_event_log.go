package harness

import (
	"encoding/json"
	"errors"

	logstore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/client"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// SessionEventLog is the session event log the manager writes every event to
// and reads turn state back from: log-store. In the server it is the log-store
// module in this process; the log-store HTTP client offers the same methods,
// which tests use against a fake log-store.
type SessionEventLog interface {
	// PushEvent stores one event and answers its row id.
	PushEvent(ev msg.Event) (int64, error)
	// GetTurnState answers the session's latest user message and
	// terminator ids, and whether a turn is in flight.
	GetTurnState(sessionID string) (logstore.TurnState, error)
	// ListEvents answers the session's events after afterID, oldest first,
	// each with event_id spliced in, of the given types (nil for all).
	ListEvents(sessionID string, afterID int64, types []string) ([]json.RawMessage, error)
	// SessionsHoldingHarnessSessionID answers every log-store session that
	// already holds a transcript for the harness session id.
	SessionsHoldingHarnessSessionID(harnessSessionID string) ([]logstore.HeldSession, error)
}

// ErrSessionEventLogNotEnabled is every write and read of the session event
// log before SetSessionEventLog names one.
var ErrSessionEventLogNotEnabled = errors.New("the session event log is not enabled in this server")

// sessionEventLogHolder lets the manager swap its log in one atomic store.
type sessionEventLogHolder struct {
	log SessionEventLog
}

// SetSessionEventLog names the session event log the manager writes to and
// reads from. The server calls it once, before it starts or resumes any
// session.
func (m *Manager) SetSessionEventLog(log SessionEventLog) {
	if log == nil {
		panic("harness: SetSessionEventLog needs a session event log")
	}
	m.logStore.Store(&sessionEventLogHolder{log: log})
}

// sessionEventLog is the log SetSessionEventLog named, or one that refuses
// every call when none is named yet.
func (m *Manager) sessionEventLog() SessionEventLog {
	if holder := m.logStore.Load(); holder != nil {
		return holder.log
	}
	return sessionEventLogNotEnabled{}
}

// SessionsHoldingHarnessSessionID asks the session event log which of its
// sessions already hold a transcript for a harness session id.
func (m *Manager) SessionsHoldingHarnessSessionID(harnessSessionID string) ([]logstore.HeldSession, error) {
	return m.sessionEventLog().SessionsHoldingHarnessSessionID(harnessSessionID)
}

type sessionEventLogNotEnabled struct{}

func (sessionEventLogNotEnabled) PushEvent(msg.Event) (int64, error) {
	return 0, ErrSessionEventLogNotEnabled
}

func (sessionEventLogNotEnabled) GetTurnState(string) (logstore.TurnState, error) {
	return logstore.TurnState{}, ErrSessionEventLogNotEnabled
}

func (sessionEventLogNotEnabled) ListEvents(string, int64, []string) ([]json.RawMessage, error) {
	return nil, ErrSessionEventLogNotEnabled
}

func (sessionEventLogNotEnabled) SessionsHoldingHarnessSessionID(string) ([]logstore.HeldSession, error) {
	return nil, ErrSessionEventLogNotEnabled
}

// sessionEventStoreHolder is where the manager stores and reads back events.
type sessionEventStoreHolder struct {
	events sessionevents.Store
}

// SetSessionEventStore names where every event is stored and read back: the
// session log, the only copy. The server calls it once, before it starts or
// resumes any session; until then every store and read of an event fails with
// ErrSessionEventLogNotEnabled.
func (m *Manager) SetSessionEventStore(events sessionevents.Store) {
	if events == nil {
		panic("harness: SetSessionEventStore needs a store")
	}
	m.sessionEventStore.Store(&sessionEventStoreHolder{events: events})
}

// SessionEvents is where events are stored and read back.
func (m *Manager) SessionEvents() sessionevents.Store {
	if holder := m.sessionEventStore.Load(); holder != nil {
		return holder.events
	}
	return sessionEventsNotEnabled{}
}

// sessionEventRecord is the record of one event of sessionID, stored with the
// message ids the caller files it under.
func sessionEventRecord(sessionID string, ev *msg.Event, messageID, harnessMessageID string, data []byte) store.SessionEventRecord {
	record := store.SessionEventRecord{
		SessionID: sessionID, Type: string(ev.Type), MessageID: messageID, HarnessMessageID: harnessMessageID,
		Data: data, Event: ev,
	}
	switch {
	case ev.ToolCall != nil:
		record.ToolUseID = ev.ToolCall.ToolID
		record.ToolName = ev.ToolCall.Name
	case ev.ToolResult != nil:
		record.ToolUseID = ev.ToolResult.ToolID
	}
	return record
}
