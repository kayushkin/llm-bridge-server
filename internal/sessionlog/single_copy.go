package sessionlog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/client"
	sessionlogserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/server"
	sessionlogstore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/store"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// SingleCopy keeps every session event in the session log, the only copy.
// The bridge's core module keeps the rows events describe — a session's
// status, a runner spawn's stored-through number — and each is written in the
// transaction of its event, on the session log's writer connection.
//
// # The rows written with their events
//
// The session log's role may read and write only those columns of the core
// module's tables (store.GrantSessionLogWrites), and the statements come from
// the core module (store.SessionRowLockForStatus, store.SessionStatusRowWrite,
// store.RunnerSpawnAdvance), which owns their tables:
//
//   - A status locks its session row first, in the same round trip as the
//     event's own first read, and is stored only when the row exists; the
//     row is written in the batch that stores the event, and both commit
//     together. A session with no row gets no status event.
//   - A runner entry's event and its spawn's number commit together, so a
//     runner is never told an entry is stored that is not, nor sent one
//     twice.
//
// # Retries
//
// Each write still carries an idempotency key: a runner entry's names its
// spawn and number, and a status's is drawn for each write. A write is
// retried while Postgres restarts, and a retry after a commit that was cut
// off cannot know whether it landed; the key makes it find the stored event
// rather than store a second one. The rows committed with that event, so the
// retry writes nothing else.
//
// # Numbering
//
// OpenSingleCopy moves the session log's numbering above the highest id the
// bridge's archived SQLite events table handed out, which the import of the
// bridge database recorded, so a Last-Event-ID or a status AsOf that a client
// holds from it is below every id stored here.
type SingleCopy struct {
	bridge     *store.Store
	sessionLog *sessionlogserver.Server
	events     *sessionlogstore.Store
	numbering  sessionlogstore.SingleCopyNumbering

	// downSince is when a write last ran out of retries, in UnixNano, or 0
	// while the session log is believed up. While it is down each write is
	// tried once, so a dead database costs each live event one attempt
	// rather than a retry deadline.
	downSince atomic.Int64
	sleep     func(time.Duration)
}

// The retry schedule of a write: doubling from the base delay to the cap,
// given up at the deadline. It covers a restart of Postgres.
const (
	writeRetryBaseDelay = 50 * time.Millisecond
	writeRetryMaxDelay  = 2 * time.Second
	writeRetryDeadline  = 30 * time.Second
)

// OpenSingleCopy makes the session log the only copy of session events: it
// raises the session log's numbering above the archived events' highest id.
// Call it before any session starts.
func OpenSingleCopy(ctx context.Context, bridge *store.Store, sessionLog *sessionlogserver.Server) (*SingleCopy, error) {
	highest, _, err := bridge.HighestArchivedEventID()
	if err != nil {
		return nil, fmt.Errorf("read the highest id of the archived session events: %w", err)
	}
	events := sessionLog.Store()
	numbering, err := events.RaiseNumberingAboveBridgeDatabase(ctx, highest)
	if err != nil {
		return nil, fmt.Errorf("raise the session log's numbering above %d: %w", highest, err)
	}
	return &SingleCopy{bridge: bridge, sessionLog: sessionLog, events: events, numbering: numbering, sleep: time.Sleep}, nil
}

// Numbering is the record of where the session log's own numbering starts.
func (c *SingleCopy) Numbering() sessionlogstore.SingleCopyNumbering { return c.numbering }

// append writes one event, retrying a failure that is safe to retry: any
// failure of a write with an idempotency key, and for one without, a failure
// before anything was sent. An event the session log refused, one whose
// session row is missing, and a write Postgres refused (refusedByPostgres)
// are never retried.
func (c *SingleCopy) append(ev *msg.Event, event sessionlogstore.EventToAppend) (sessionlogstore.AppendedEvent, error) {
	deadline := time.Now().Add(writeRetryDeadline)
	delay := writeRetryBaseDelay
	for attempt := 1; ; attempt++ {
		appended, err := c.sessionLog.AppendEvent(ev, event)
		if err == nil {
			if since := c.downSince.Swap(0); since != 0 {
				log.Printf("[session-events] the session log takes writes again, after failing since %s", time.Unix(0, since).UTC().Format(time.RFC3339))
			} else if attempt > 1 {
				log.Printf("[session-events] %s event of %s stored on attempt %d", event.Type, event.SessionID, attempt)
			}
			return appended, nil
		}
		var rejected *client.EventRejectedError
		if errors.As(err, &rejected) || errors.Is(err, sessionlogstore.ErrRequiredRowMissing) || errors.Is(err, errBuildingCompanionWrite) {
			return appended, err
		}
		retryable := !refusedByPostgres(err) && !poolClosed(err) && (event.IdempotencyKey != "" || pgconn.SafeToRetry(err))
		if !retryable || c.downSince.Load() != 0 {
			return appended, fmt.Errorf("store %s event of %s: %w", event.Type, event.SessionID, err)
		}
		if time.Now().Add(delay).After(deadline) {
			c.downSince.CompareAndSwap(0, time.Now().UnixNano())
			return appended, fmt.Errorf("store %s event of %s: gave up after %d attempts in %s: %w", event.Type, event.SessionID, attempt, writeRetryDeadline, err)
		}
		log.Printf("[session-events] storing %s event of %s failed (attempt %d), retrying in %s: %v", event.Type, event.SessionID, attempt, delay, err)
		c.sleep(delay)
		delay = min(delay*2, writeRetryMaxDelay)
	}
}

// refusedByPostgres says Postgres ran the write and refused it — a constraint,
// a permission, a statement it could not run — which trying again would only
// repeat. A lost connection, a server shutting down or not yet taking
// connections, and a transaction that lost a deadlock or a serialization race
// are not refusals.
func refusedByPostgres(err error) bool {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return false
	}
	switch {
	case strings.HasPrefix(postgresError.Code, "08"), // connection exception
		strings.HasPrefix(postgresError.Code, "57P"), // shutdown, cannot connect now
		postgresError.Code == "40001",                // serialization failure
		postgresError.Code == "40P01":                // deadlock detected
		return false
	}
	return true
}

// poolClosed says the session log's pool was closed, as at shutdown: no retry
// can succeed. database/sql keeps its error for this unexported, so it is
// known by its text.
func poolClosed(err error) bool {
	return errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "sql: database is closed")
}

func eventToAppend(record store.SessionEventRecord) sessionlogstore.EventToAppend {
	return sessionlogstore.EventToAppend{
		SessionID: record.SessionID,
		Type:      record.Type,
		Fields: sessionlogstore.EventFields{
			MessageID:        record.MessageID,
			HarnessMessageID: record.HarnessMessageID,
			ToolUseID:        record.ToolUseID,
			ToolName:         record.ToolName,
			TurnID:           record.Event.TurnID,
			HarnessSessionID: record.Event.HarnessSessionID,
		},
		Data: record.Data,
	}
}

// StoreSessionEvent stores one event in the session log.
func (c *SingleCopy) StoreSessionEvent(record store.SessionEventRecord) (int64, error) {
	appended, err := c.append(record.Event, eventToAppend(record))
	return appended.ID, err
}

// RunnerEntryIdempotencyKey names entry sequence of a runner spawn.
func RunnerEntryIdempotencyKey(spawnID string, sequence uint64) string {
	return "runner_entry:" + spawnID + ":" + strconv.FormatUint(sequence, 10)
}

// errBuildingCompanionWrite marks a failure to build a row's statement, which
// a retry would only repeat.
var errBuildingCompanionWrite = errors.New("build the write of the event's row")

func sessionLogStatement(statement store.SessionLogStatement) sessionlogstore.Statement {
	return sessionlogstore.Statement{Query: statement.Query, Arguments: statement.Arguments}
}

// StoreSessionEventAdvancingRunnerSpawn stores the event and records the
// spawn as stored through the entry, in one transaction.
func (c *SingleCopy) StoreSessionEventAdvancingRunnerSpawn(record store.SessionEventRecord, spawnSessionID, spawnID string, sequence uint64) (int64, error) {
	event := eventToAppend(record)
	event.IdempotencyKey = RunnerEntryIdempotencyKey(spawnID, sequence)
	event.AlsoWrite = func(int64) ([]sessionlogstore.Statement, error) {
		return []sessionlogstore.Statement{sessionLogStatement(store.RunnerSpawnAdvance(spawnSessionID, spawnID, sequence))}, nil
	}
	appended, err := c.append(record.Event, event)
	if err != nil {
		return 0, err
	}
	if appended.AlreadyStored {
		log.Printf("[session-events] session %s: runner entry %d of %s was already stored as event %d, with the spawn's number", spawnSessionID, sequence, spawnID, appended.ID)
	}
	return appended.ID, nil
}

// WriteSessionStatus stores the status event and writes the status onto the
// session row, in one transaction.
func (c *SingleCopy) WriteSessionStatus(bridgeID string, ev *msg.Event) (int64, error) {
	return c.writeSessionStatus(bridgeID, ev, true)
}

func (c *SingleCopy) writeSessionStatus(bridgeID string, ev *msg.Event, touchUpdatedAt bool) (int64, error) {
	if ev == nil || ev.Type != msg.EventSessionStatus || ev.Status == nil {
		return 0, fmt.Errorf("WriteSessionStatus: want a %s event carrying a status", msg.EventSessionStatus)
	}
	key, err := newSessionStatusIdempotencyKey(bridgeID)
	if err != nil {
		return 0, err
	}
	requiredRow := sessionLogStatement(store.SessionRowLockForStatus(bridgeID))
	ev.Status.AsOf = 0
	appended, err := c.append(ev, sessionlogstore.EventToAppend{
		SessionID:      bridgeID,
		Type:           string(msg.EventSessionStatus),
		Fields:         sessionlogstore.EventFields{TurnID: ev.TurnID, HarnessSessionID: ev.HarnessSessionID},
		IdempotencyKey: key,
		RequiredRow:    &requiredRow,
		DataForID: func(id int64) ([]byte, error) {
			ev.Status.AsOf = id
			return json.Marshal(ev)
		},
		AlsoWrite: func(id int64) ([]sessionlogstore.Statement, error) {
			ev.Status.AsOf = id
			write, err := store.SessionStatusRowWrite(bridgeID, ev.Status, touchUpdatedAt)
			if err != nil {
				return nil, errors.Join(errBuildingCompanionWrite, err)
			}
			return []sessionlogstore.Statement{sessionLogStatement(write)}, nil
		},
	})
	if errors.Is(err, sessionlogstore.ErrRequiredRowMissing) {
		ev.Status.AsOf = 0
		return 0, sql.ErrNoRows
	}
	if err != nil {
		ev.Status.AsOf = 0
		return 0, err
	}
	ev.Status.AsOf = appended.ID
	c.bridge.NotifySessionChanged(bridgeID)
	return appended.ID, nil
}

func newSessionStatusIdempotencyKey(bridgeID string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("draw a status write key: %w", err)
	}
	return "session_status:" + bridgeID + ":" + hex.EncodeToString(random), nil
}

// ReconcileSessions resets the sessions a previous process left in one of
// states and writes each one's startup status, as store.ReconcileSessions
// does in one database.
func (c *SingleCopy) ReconcileSessions(states ...msg.SessionState) ([]store.ReconciledSession, error) {
	sessions, err := c.bridge.ResetStaleSessions(states...)
	if err != nil || len(sessions) == 0 {
		return nil, err
	}
	reconciled := make([]store.ReconciledSession, 0, len(sessions))
	for i := range sessions {
		ev := &msg.Event{
			Type:            msg.EventSessionStatus,
			Harness:         sessions[i].Harness,
			BridgeSessionID: sessions[i].SessionID,
			Timestamp:       time.Now(),
			Status:          store.StartupReconcileStatus(&sessions[i]),
		}
		// The row of a reset session keeps its updated_at, which
		// FileInactive and ArchiveOld read as its last real change.
		if _, err := c.writeSessionStatus(sessions[i].SessionID, ev, false); err != nil {
			return nil, fmt.Errorf("reconcile session %s: %w", sessions[i].SessionID, err)
		}
		reconciled = append(reconciled, store.ReconciledSession{Session: sessions[i], Status: *ev})
	}
	return reconciled, nil
}

func eventsWithIDs(rows []sessionlogstore.StoredEventBody) []store.EventWithID {
	if rows == nil {
		return nil
	}
	out := make([]store.EventWithID, len(rows))
	for i, row := range rows {
		out[i] = store.EventWithID{RowID: int(row.ID), Data: row.Data}
	}
	return out
}

func (c *SingleCopy) MaxEventRowID(sessionID string) (int64, error) {
	return c.events.LatestEventID(sessionID)
}

// MaxEventRowIDs reads every session's stream head in one query.
func (c *SingleCopy) MaxEventRowIDs(sessionIDs []string) (map[string]int64, error) {
	return c.events.LatestEventIDs(sessionIDs)
}

func (c *SingleCopy) ListEventsSinceID(sessionID string, afterID int) ([]store.EventWithID, error) {
	rows, err := c.events.EventRowsAfter(sessionID, int64(afterID))
	return eventsWithIDs(rows), err
}

func (c *SingleCopy) ListCurrentTurnEventsWithIDs(sessionID string) ([]store.EventWithID, error) {
	rows, err := c.events.CurrentTurnEventRows(sessionID)
	return eventsWithIDs(rows), err
}

// StreamCursorFromEarlierNumbering says a cursor is below the first id the
// session log numbered as the only copy: the bridge database's numbering.
func (c *SingleCopy) StreamCursorFromEarlierNumbering(cursor int64) bool {
	return cursor > 0 && cursor < c.numbering.FirstEventID
}

func (c *SingleCopy) HarnessToBridgeMap(sessionID string) (map[string]string, error) {
	return c.events.HarnessToBridgeMessageIDs(sessionID)
}

func (c *SingleCopy) ToolUseToMessageMap(sessionID string) (map[string]store.ToolUseBinding, error) {
	ids, err := c.events.ToolUseToMessageIDs(sessionID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.ToolUseBinding, len(ids))
	for toolUseID, binding := range ids {
		out[toolUseID] = store.ToolUseBinding{BridgeMessageID: binding.MessageID, HarnessMessageID: binding.HarnessMessageID}
	}
	return out, nil
}

func (c *SingleCopy) CountUserTurns(sessionID string) (int, error) {
	return c.events.CountDistinctUserTurnIDs(sessionID)
}

func (c *SingleCopy) LastActivityAt(sessionID string) (time.Time, error) {
	return c.events.LatestEventCreatedAt(sessionID)
}

func (c *SingleCopy) CurrentTurnText(sessionID string) (string, []string, error) {
	return c.events.CurrentTurnText(sessionID)
}

func (c *SingleCopy) FirstUserMessage(sessionID string) (string, error) {
	return c.events.FirstUserMessageText(sessionID)
}

func (c *SingleCopy) LatestScheduledWakeup(bridgeID string, callsLoggedSince time.Time) (*store.ScheduledWakeup, error) {
	found, err := c.events.LatestToolCallWithResult(bridgeID, store.ClaudeCodeScheduleWakeupToolName, callsLoggedSince)
	if err != nil || found == nil {
		return nil, err
	}
	if !found.ScheduledForMilliseconds.Valid || found.ScheduledForMilliseconds.Int64 <= 0 {
		return nil, nil
	}
	return &store.ScheduledWakeup{
		SessionID: bridgeID,
		ToolID:    found.ToolID,
		Prompt:    found.Prompt,
		DueAt:     time.UnixMilli(found.ScheduledForMilliseconds.Int64).UTC(),
	}, nil
}

func (c *SingleCopy) UserMessageLoggedSince(bridgeID string, since time.Time) (bool, error) {
	return c.events.UserMessageStoredSince(bridgeID, since)
}

func (c *SingleCopy) LastReportedBackgroundTasks(bridgeID string) ([]msg.BackgroundTask, error) {
	data, err := c.events.LatestSystemEventOfSubtype(bridgeID, msg.SystemSubtypeBackgroundTasksChanged)
	if err != nil || data == nil {
		return nil, err
	}
	var ev msg.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, err
	}
	if ev.System == nil {
		return nil, nil
	}
	return ev.System.BackgroundTasks, nil
}
