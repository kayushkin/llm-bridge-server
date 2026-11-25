package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The session event log as the server's only copy of session events: the
// write the harness manager makes for each event, the numbering that keeps
// its ids above the bridge database's, and the reads that used to go to the
// bridge database's events table. Migration 0002 adds what they need.

// EventFields are the values of an event its write reads, which a writer
// holding the event passes rather than have the store read its body again.
// Each is ” when the event names none.
type EventFields struct {
	// MessageID, HarnessMessageID, ToolUseID and ToolName are kept beside
	// the body, for the readers that select by them. ToolUseID is the tool
	// call's id on a tool_call or tool_result event, ToolName the tool's name
	// on a tool_call.
	MessageID        string
	HarnessMessageID string
	ToolUseID        string
	ToolName         string
	// TurnID places the event in its turn.
	TurnID string
	// HarnessSessionID is the harness's own id for the session, which the
	// sessions row carries.
	HarnessSessionID string
}

// EventFieldsOf reads the fields from an event.
func EventFieldsOf(ev *msg.Event) EventFields {
	columns := EventFields{MessageID: ev.MessageID, HarnessMessageID: ev.HarnessMessageID, TurnID: ev.TurnID, HarnessSessionID: ev.HarnessSessionID}
	switch {
	case ev.ToolCall != nil:
		columns.ToolUseID = ev.ToolCall.ToolID
		columns.ToolName = ev.ToolCall.Name
	case ev.ToolResult != nil:
		columns.ToolUseID = ev.ToolResult.ToolID
	}
	return columns
}

// eventFieldsOfBody is the part of a stored body EventFieldsOfBody reads.
type eventFieldsOfBody struct {
	MessageID        string `json:"message_id"`
	HarnessMessageID string `json:"harness_message_id"`
	TurnID           string `json:"turn_id"`
	HarnessSessionID string `json:"harness_session_id"`
	ToolCall         *struct {
		ToolID string `json:"tool_id"`
		Name   string `json:"name"`
	} `json:"tool_call"`
	ToolResult *struct {
		ToolID string `json:"tool_id"`
	} `json:"tool_result"`
}

// EventFieldsOfBody reads the fields from an event's JSON, for a writer that
// holds only the body: StoreEvent, the import.
func EventFieldsOfBody(data []byte) (EventFields, error) {
	var fields eventFieldsOfBody
	if err := json.Unmarshal(data, &fields); err != nil {
		return EventFields{}, err
	}
	columns := EventFields{MessageID: fields.MessageID, HarnessMessageID: fields.HarnessMessageID, TurnID: fields.TurnID, HarnessSessionID: fields.HarnessSessionID}
	switch {
	case fields.ToolCall != nil:
		columns.ToolUseID = fields.ToolCall.ToolID
		columns.ToolName = fields.ToolCall.Name
	case fields.ToolResult != nil:
		columns.ToolUseID = fields.ToolResult.ToolID
	}
	return columns, nil
}

// EventToAppend is one event to store.
type EventToAppend struct {
	SessionID string
	Type      string
	Fields    EventFields
	// IdempotencyKey names the write. Empty stores the event whatever was
	// stored before; set, a second write with the same key stores nothing
	// and answers the first one's row.
	IdempotencyKey string
	// Data is the body. Exactly one of Data and DataForID is set.
	Data []byte
	// DataForID makes the body once the event's id is known, for an event
	// that carries its own id (a session status, whose as_of is it). It is
	// called inside the write, so the id it is given is the one stored.
	DataForID func(id int64) ([]byte, error)
}

// AppendedEvent is the row AppendEvent stored, or found already stored.
type AppendedEvent struct {
	ID int64
	// Data is the body stored under ID. For an event already stored it is
	// the first write's body, not this one's.
	Data []byte
	// AlreadyStored says the idempotency key named a row that was there.
	AlreadyStored bool
}

// ErrEventBodyMissing is an EventToAppend with neither or both bodies.
var ErrEventBodyMissing = errors.New("session log: an event to append needs exactly one of Data and DataForID")

// HighestEventID is the highest event id stored, or 0 for none.
func (s *Store) HighestEventID() (int64, error) {
	var id int64
	err := s.reader.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&id)
	return id, err
}

// SingleCopyNumbering is the row RaiseNumberingAboveBridgeDatabase writes.
type SingleCopyNumbering struct {
	// FirstEventID is the first id numbered with this module as the only
	// copy. Every id below it was handed out before: by the bridge
	// database's events table, or by this module while it was the second
	// copy, and a cursor or status a client holds that is below it came
	// from the bridge database's numbering.
	FirstEventID                 int64
	BridgeDatabaseHighestEventID int64
	RaisedAt                     time.Time
}

// RaiseNumberingAboveBridgeDatabase makes the next event id larger than
// bridgeDatabaseHighestEventID and than every id this module handed out, and
// records where that numbering starts, the first time it runs. Later calls
// leave the record as it is and only raise the numbering again if the bridge
// database has handed out higher ids since (the server ran the old path in
// between).
func (s *Store) RaiseNumberingAboveBridgeDatabase(ctx context.Context, bridgeDatabaseHighestEventID int64) (SingleCopyNumbering, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return SingleCopyNumbering{}, err
	}
	defer tx.Rollback()
	// nextval hands out one id, which no event takes: the cost of reading
	// the sequence's position without naming it.
	var next, highestStored int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('events', 'id'))`).Scan(&next); err != nil {
		return SingleCopyNumbering{}, fmt.Errorf("read the event numbering: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&highestStored); err != nil {
		return SingleCopyNumbering{}, err
	}
	target := max(next, highestStored+1, bridgeDatabaseHighestEventID+1)
	if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('events', 'id'), $1, false)`, target); err != nil {
		return SingleCopyNumbering{}, fmt.Errorf("number events from %d: %w", target, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO single_copy_numbering (first_event_id, bridge_database_highest_event_id) VALUES ($1, $2)
		 ON CONFLICT (only_row) DO NOTHING`,
		target, bridgeDatabaseHighestEventID,
	); err != nil {
		return SingleCopyNumbering{}, fmt.Errorf("record the numbering: %w", err)
	}
	var numbering SingleCopyNumbering
	if err := tx.QueryRowContext(ctx,
		`SELECT first_event_id, bridge_database_highest_event_id, raised_at FROM single_copy_numbering`,
	).Scan(&numbering.FirstEventID, &numbering.BridgeDatabaseHighestEventID, &numbering.RaisedAt); err != nil {
		return SingleCopyNumbering{}, err
	}
	return numbering, tx.Commit()
}

// SingleCopyNumberingRecord answers the numbering record, and false when
// this module has never been the only copy.
func (s *Store) SingleCopyNumberingRecord() (SingleCopyNumbering, bool, error) {
	var numbering SingleCopyNumbering
	err := s.reader.QueryRow(
		`SELECT first_event_id, bridge_database_highest_event_id, raised_at FROM single_copy_numbering`,
	).Scan(&numbering.FirstEventID, &numbering.BridgeDatabaseHighestEventID, &numbering.RaisedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SingleCopyNumbering{}, false, nil
	}
	if err != nil {
		return SingleCopyNumbering{}, false, err
	}
	return numbering, true, nil
}

// StoredEventBody is one stored event's id and its body as it was sent.
type StoredEventBody struct {
	ID   int64
	Data json.RawMessage
}

func scanEventRows(rows *sql.Rows) ([]StoredEventBody, error) {
	defer rows.Close()
	var events []StoredEventBody
	for rows.Next() {
		var event StoredEventBody
		var data string
		if err := rows.Scan(&event.ID, &data); err != nil {
			return nil, err
		}
		event.Data = json.RawMessage(data)
		events = append(events, event)
	}
	return events, rows.Err()
}

// LatestEventID is the highest event id of a session, or 0 for none: the
// session's stream head.
func (s *Store) LatestEventID(sessionID string) (int64, error) {
	var id int64
	err := s.reader.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1`, sessionID).Scan(&id)
	return id, err
}

// EventRowsAfter is a session's events after afterID, oldest first, each as
// it was sent.
func (s *Store) EventRowsAfter(sessionID string, afterID int64) ([]StoredEventBody, error) {
	rows, err := s.reader.Query(`SELECT id, data FROM events WHERE session_id = $1 AND id > $2 ORDER BY id`, sessionID, afterID)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

// CurrentTurnEventRows is every event after the session's last user_message
// except user messages, oldest first: what a stream replays on connect.
func (s *Store) CurrentTurnEventRows(sessionID string) ([]StoredEventBody, error) {
	rows, err := s.reader.Query(`
		SELECT id, data FROM events
		WHERE session_id = $1 AND type <> 'user_message'
		  AND id > (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1 AND type = 'user_message')
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

// LatestEventCreatedAt is when the session's newest event was stored, to the
// second, or the zero time for a session with none. The bridge database kept
// whole seconds, and its callers read the time that way.
func (s *Store) LatestEventCreatedAt(sessionID string) (time.Time, error) {
	var createdAt time.Time
	err := s.reader.QueryRow(`SELECT created_at FROM events WHERE session_id = $1 ORDER BY id DESC LIMIT 1`, sessionID).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return createdAt.UTC().Truncate(time.Second), nil
}

// HarnessToBridgeMessageIDs maps each harness message id of a session to the
// bridge message id stored with it, from the first event that holds both.
func (s *Store) HarnessToBridgeMessageIDs(sessionID string) (map[string]string, error) {
	rows, err := s.reader.Query(`
		SELECT DISTINCT ON (harness_message_id) harness_message_id, message_id FROM events
		WHERE session_id = $1 AND harness_message_id <> '' AND message_id <> ''
		ORDER BY harness_message_id, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var harnessMessageID, messageID string
		if err := rows.Scan(&harnessMessageID, &messageID); err != nil {
			return nil, err
		}
		out[harnessMessageID] = messageID
	}
	return out, rows.Err()
}

// ToolUseMessageIDs are the message ids stored with a tool call or result.
type ToolUseMessageIDs struct {
	MessageID        string
	HarnessMessageID string
}

// ToolUseToMessageIDs maps each tool use id of a session to the message ids
// of its first tool_call or tool_result that carries a bridge message id.
func (s *Store) ToolUseToMessageIDs(sessionID string) (map[string]ToolUseMessageIDs, error) {
	rows, err := s.reader.Query(`
		SELECT DISTINCT ON (tool_use_id) tool_use_id, message_id, harness_message_id FROM events
		WHERE session_id = $1 AND type IN ('tool_call', 'tool_result') AND message_id <> '' AND tool_use_id <> ''
		ORDER BY tool_use_id, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ToolUseMessageIDs{}
	for rows.Next() {
		var toolUseID string
		var ids ToolUseMessageIDs
		if err := rows.Scan(&toolUseID, &ids.MessageID, &ids.HarnessMessageID); err != nil {
			return nil, err
		}
		out[toolUseID] = ids
	}
	return out, rows.Err()
}

// CountDistinctUserTurnIDs counts the distinct turn ids among a session's
// user_message events. A user message is stored twice, when the server takes
// it and when the harness echoes it, under one turn id.
func (s *Store) CountDistinctUserTurnIDs(sessionID string) (int, error) {
	var n int
	err := s.reader.QueryRow(
		`SELECT COUNT(DISTINCT data->>'turn_id') FROM events WHERE session_id = $1 AND type = 'user_message'`, sessionID,
	).Scan(&n)
	return n, err
}

// CurrentTurnText is the text of the session's last user_message and every
// non-blank text block after it, oldest first.
func (s *Store) CurrentTurnText(sessionID string) (string, []string, error) {
	var lastUserMessageID int64
	var lastUserMessage sql.NullString
	err := s.reader.QueryRow(
		`SELECT id, data->'result'->>'text' FROM events WHERE session_id = $1 AND type = 'user_message' ORDER BY id DESC LIMIT 1`, sessionID,
	).Scan(&lastUserMessageID, &lastUserMessage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	rows, err := s.reader.Query(`
		SELECT data->'block'->'block'->'text_block'->>'text' FROM events
		WHERE session_id = $1 AND id > $2 AND type = 'block' AND data->'block'->'block'->>'type' = 'text'
		ORDER BY id`, sessionID, lastUserMessageID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var text sql.NullString
		if err := rows.Scan(&text); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(text.String) == "" {
			continue
		}
		texts = append(texts, text.String)
	}
	return lastUserMessage.String, texts, rows.Err()
}

// FirstUserMessageText is the text of the session's first user_message, or
// "" for none.
func (s *Store) FirstUserMessageText(sessionID string) (string, error) {
	var text sql.NullString
	err := s.reader.QueryRow(
		`SELECT data->'result'->>'text' FROM events WHERE session_id = $1 AND type = 'user_message' ORDER BY id LIMIT 1`, sessionID,
	).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text.String, err
}

// LatestSystemEventOfSubtype is the body of the session's newest system event
// of a subtype, or nil for none.
func (s *Store) LatestSystemEventOfSubtype(sessionID, subtype string) (json.RawMessage, error) {
	var data string
	err := s.reader.QueryRow(`
		SELECT data FROM events
		WHERE session_id = $1 AND type = 'system' AND data->'system'->>'subtype' = $2
		ORDER BY id DESC LIMIT 1`, sessionID, subtype,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// ToolCallAndResult is a tool call and the newest tool result after it that
// carries the harness's tool_use_result.
type ToolCallAndResult struct {
	ToolID string
	// Input is the call's input.prompt as text.
	Prompt string
	// ScheduledForMilliseconds is the result's
	// raw.tool_use_result.scheduledFor; not Valid when it names none or it
	// is not a number.
	ScheduledForMilliseconds sql.NullInt64
}

// LatestToolCallWithResult finds the session's newest tool_call of toolName
// stored at or after the second of since, and the newest tool_result after it
// for the same tool id that carries raw.tool_use_result. It answers nil when
// there is no such call, or the call has no such result.
func (s *Store) LatestToolCallWithResult(sessionID, toolName string, since time.Time) (*ToolCallAndResult, error) {
	var found ToolCallAndResult
	var toolID, prompt sql.NullString
	var scheduledFor sql.NullFloat64
	err := s.reader.QueryRow(`
		SELECT call.data->'tool_call'->>'tool_id',
		       call.data->'tool_call'->'input'->>'prompt',
		       json_number(result.data->'raw'->'tool_use_result'->'scheduledFor')::double precision
		FROM (
			SELECT id, data FROM events
			WHERE session_id = $1 AND type = 'tool_call' AND created_at >= $2
			  AND tool_name = $3
			ORDER BY id DESC LIMIT 1) call
		JOIN events result
		  ON result.session_id = $1 AND result.type = 'tool_result' AND result.id > call.id
		 AND result.tool_use_id = call.data->'tool_call'->>'tool_id'
		 AND result.data->'raw'->'tool_use_result' IS NOT NULL
		 AND json_typeof(result.data->'raw'->'tool_use_result') <> 'null'
		ORDER BY result.id DESC LIMIT 1`,
		sessionID, since.UTC().Truncate(time.Second), toolName,
	).Scan(&toolID, &prompt, &scheduledFor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	found.ToolID = toolID.String
	found.Prompt = prompt.String
	if scheduledFor.Valid {
		found.ScheduledForMilliseconds = sql.NullInt64{Int64: int64(scheduledFor.Float64), Valid: true}
	}
	return &found, nil
}

// UserMessageStoredSince reports whether the session has a user_message
// stored at or after the second of since.
func (s *Store) UserMessageStoredSince(sessionID string, since time.Time) (bool, error) {
	var found bool
	err := s.reader.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM events WHERE session_id = $1 AND type = 'user_message' AND created_at >= $2)`,
		sessionID, since.UTC().Truncate(time.Second),
	).Scan(&found)
	return found, err
}

// SessionStatusEvent is a session_status event's id and body.
type SessionStatusEvent struct {
	SessionID string
	ID        int64
	Data      json.RawMessage
}

// LatestSessionStatusesAmongNewestEvents answers, for each session with a
// session_status among the newest window events of the whole log, its newest
// one. It reads a window rather than every session's history because a
// status whose row write a crash cut off is among the last events written.
func (s *Store) LatestSessionStatusesAmongNewestEvents(window int64) ([]SessionStatusEvent, error) {
	rows, err := s.reader.Query(`
		SELECT DISTINCT ON (session_id) session_id, id, data FROM events
		WHERE id > (SELECT COALESCE(MAX(id), 0) FROM events) - $1 AND type = 'session_status'
		ORDER BY session_id, id DESC`, window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []SessionStatusEvent
	for rows.Next() {
		var status SessionStatusEvent
		var data string
		if err := rows.Scan(&status.SessionID, &status.ID, &data); err != nil {
			return nil, err
		}
		status.Data = json.RawMessage(data)
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func logProjectionFailure(projection, sessionID, eventType string, err error) {
	log.Printf("[log-store] update %s for %s/%s: %v", projection, sessionID, eventType, err)
}
