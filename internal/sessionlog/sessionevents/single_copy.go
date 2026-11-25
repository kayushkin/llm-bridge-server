package sessionevents

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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/client"
	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	logstorestore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// SingleCopy keeps every session event in the session log only. The bridge
// database keeps the rows events describe — a session's status, a runner
// spawn's stored-through number — and those are written after their event.
//
// # Two writes across two databases
//
// A status and its session row, and a runner entry and its spawn's number,
// were each one SQLite transaction. Across two databases the event is
// written first, with an idempotency key, and the row second:
//
//   - A runner entry's key is its spawn and number. If the server stops
//     after the event and before the number, the runner sends the entry
//     again; the event is found under its key and not stored twice, and the
//     number is then written.
//   - A status's key is drawn for each write, so a write retried after a
//     commit that may have landed stores one event. The row write is
//     conditional on AsOf, so an older status never replaces a newer one
//     and writing a status twice changes nothing. If the server stops
//     between the two, the row is behind its newest status event, and
//     RepairSessionStatusRows copies that status onto the row at the next
//     start, before the startup reconcile reads the rows.
//
// # Numbering
//
// OpenSingleCopy moves the session log's numbering above the highest id the
// bridge database handed out, so a Last-Event-ID or a status AsOf that a
// client holds from it is below every id stored here.
type SingleCopy struct {
	bridge     *store.Store
	sessionLog *logstoreserver.Server
	events     *logstorestore.Store
	numbering  logstorestore.SingleCopyNumbering

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

// statusRepairWindow is how many of the newest events RepairSessionStatusRows
// reads. A status whose row write was cut off is among the last events the
// stopped process wrote, and tens of thousands of events are many minutes of
// the busiest load measured.
const statusRepairWindow = 20000

// OpenSingleCopy makes the session log the only copy: it raises the session
// log's numbering above the bridge database's, and repairs any session row a
// stop left behind its newest status. Call it before any session starts.
func OpenSingleCopy(ctx context.Context, bridge *store.Store, sessionLog *logstoreserver.Server) (*SingleCopy, error) {
	highest, err := bridge.HighestEventID()
	if err != nil {
		return nil, fmt.Errorf("read the bridge database's highest event id: %w", err)
	}
	events := sessionLog.Store()
	numbering, err := events.RaiseNumberingAboveBridgeDatabase(ctx, highest)
	if err != nil {
		return nil, fmt.Errorf("raise the session log's numbering above %d: %w", highest, err)
	}
	c := &SingleCopy{bridge: bridge, sessionLog: sessionLog, events: events, numbering: numbering, sleep: time.Sleep}
	repaired, err := c.RepairSessionStatusRows()
	if err != nil {
		return nil, fmt.Errorf("repair session rows from their status events: %w", err)
	}
	if repaired > 0 {
		log.Printf("[session-events] %d session rows were behind their newest status event and now carry it", repaired)
	}
	return c, nil
}

// Numbering is the record of where the session log's own numbering starts.
func (c *SingleCopy) Numbering() logstorestore.SingleCopyNumbering { return c.numbering }

// RepairSessionStatusRows copies onto each session row the newest status
// event stored for it since the session log became the only copy, where the
// row holds an older one. It answers how many rows changed.
func (c *SingleCopy) RepairSessionStatusRows() (int, error) {
	statuses, err := c.events.LatestSessionStatusesAmongNewestEvents(statusRepairWindow)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, status := range statuses {
		if status.ID < c.numbering.FirstEventID {
			// Written by the old path, which wrote its row in the same
			// transaction and stamped it with the bridge database's id.
			continue
		}
		var ev msg.Event
		if err := json.Unmarshal(status.Data, &ev); err != nil {
			return repaired, fmt.Errorf("decode status event %d: %w", status.ID, err)
		}
		if ev.Status == nil {
			return repaired, fmt.Errorf("status event %d carries no status", status.ID)
		}
		changed, err := c.bridge.WriteSessionStatusRow(status.SessionID, ev.Status, false)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return repaired, fmt.Errorf("session %s: %w", status.SessionID, err)
		}
		if changed {
			log.Printf("[session-events] session %s: its row was behind status event %d (%s); repaired", status.SessionID, status.ID, ev.Status.State)
			repaired++
		}
	}
	return repaired, nil
}

// append writes one event, retrying a failure that is safe to retry: any
// failure of a write with an idempotency key, and for one without, a failure
// before anything was sent. A refused event is never retried.
func (c *SingleCopy) append(ev *msg.Event, event logstorestore.EventToAppend) (logstorestore.AppendedEvent, error) {
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
		if errors.As(err, &rejected) {
			return appended, err
		}
		retryable := event.IdempotencyKey != "" || pgconn.SafeToRetry(err)
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

func eventToAppend(record store.SessionEventRecord) logstorestore.EventToAppend {
	return logstorestore.EventToAppend{
		SessionID: record.SessionID,
		Type:      record.Type,
		Columns: logstorestore.EventColumns{
			MessageID:        record.MessageID,
			HarnessMessageID: record.HarnessMessageID,
			ToolUseID:        record.ToolUseID,
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

// StoreSessionEventAdvancingRunnerSpawn stores the event under its entry's
// key, then records the spawn as stored through the entry. An entry sent
// again after a stop between the two is found stored, and only the number
// is written.
func (c *SingleCopy) StoreSessionEventAdvancingRunnerSpawn(record store.SessionEventRecord, spawnSessionID, spawnID string, sequence uint64) (int64, error) {
	event := eventToAppend(record)
	event.IdempotencyKey = RunnerEntryIdempotencyKey(spawnID, sequence)
	appended, err := c.append(record.Event, event)
	if err != nil {
		return 0, err
	}
	if appended.AlreadyStored {
		log.Printf("[session-events] session %s: runner entry %d of %s was already stored as event %d; recording the spawn as stored through it", spawnSessionID, sequence, spawnID, appended.ID)
	}
	if err := c.bridge.AdvanceRunnerSpawn(spawnSessionID, spawnID, sequence); err != nil {
		return appended.ID, err
	}
	return appended.ID, nil
}

// WriteSessionStatus stores the status event in the session log, then writes
// the status onto the session row.
func (c *SingleCopy) WriteSessionStatus(bridgeID string, ev *msg.Event) (int64, error) {
	return c.writeSessionStatus(bridgeID, ev, true)
}

func (c *SingleCopy) writeSessionStatus(bridgeID string, ev *msg.Event, touchUpdatedAt bool) (int64, error) {
	if ev == nil || ev.Type != msg.EventSessionStatus || ev.Status == nil {
		return 0, fmt.Errorf("WriteSessionStatus: want a %s event carrying a status", msg.EventSessionStatus)
	}
	// A session with no row gets no status event, as in one transaction: a
	// hook-only session raises a status on every event and has no row.
	exists, err := c.bridge.SessionExists(bridgeID)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, sql.ErrNoRows
	}
	key, err := newSessionStatusIdempotencyKey(bridgeID)
	if err != nil {
		return 0, err
	}
	ev.Status.AsOf = 0
	appended, err := c.append(ev, logstorestore.EventToAppend{
		SessionID:      bridgeID,
		Type:           string(msg.EventSessionStatus),
		IdempotencyKey: key,
		DataForID: func(id int64) ([]byte, error) {
			ev.Status.AsOf = id
			return json.Marshal(ev)
		},
	})
	if err != nil {
		ev.Status.AsOf = 0
		return 0, err
	}
	ev.Status.AsOf = appended.ID
	if _, err := c.bridge.WriteSessionStatusRow(bridgeID, ev.Status, touchUpdatedAt); err != nil {
		return appended.ID, fmt.Errorf("status event %d is stored, but writing it onto the session row failed (the next start repairs the row): %w", appended.ID, err)
	}
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

func eventsWithIDs(rows []logstorestore.StoredEventBody) []store.EventWithID {
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
