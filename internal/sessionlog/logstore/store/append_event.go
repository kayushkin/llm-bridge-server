package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// AppendEvent stores one event, places it in its turn and adds it to the
// session's totals, in two round trips to Postgres on the single writer:
//
//  1. one read: the event's id, the session's turn index state, and the row
//     an idempotency key already names;
//  2. one batch of every write, which Postgres runs as one implicit
//     transaction with one commit.
//
// Event ids are handed out in the order events commit, because the writer is
// one connection held for both steps: a reader that has seen id N has seen
// every id below it. The event's created_at is the database's clock at the
// read, so its hour is known before the batch is sent.
//
// It replaced a write of fifteen or more round trips, one per statement, which
// measured at about 1.9 ms an event and put the session log behind the
// bridge database's events table on the live path.
//
// A total that cannot be read from the event's body (a malformed result) is
// logged and left out, as before. A statement that fails fails the whole
// write: the event, its turn and its totals are kept together or not at all.
//
// ctx bounds the write: a write it cancels may still have been stored, as a
// commit that is cut off cannot say whether it landed. A write with an
// idempotency key can be sent again to find out.
func (s *Store) AppendEvent(ctx context.Context, event EventToAppend) (AppendedEvent, error) {
	if (event.Data == nil) == (event.DataForID == nil) {
		return AppendedEvent{}, ErrEventBodyMissing
	}
	connection, err := s.writer.Conn(ctx)
	if err != nil {
		return AppendedEvent{}, err
	}
	defer connection.Close()
	var appended AppendedEvent
	err = connection.Raw(func(driverConnection any) error {
		var err error
		appended, err = s.appendEventOnConnection(ctx, driverConnection.(*stdlib.Conn).Conn(), event)
		return err
	})
	return appended, err
}

// appendState is what step 1 reads.
type appendState struct {
	existingID   sql.NullInt64
	existingData sql.NullString
	id           int64
	createdAt    time.Time
	// The session's turn index row, when it has one.
	indexVersion        sql.NullInt64
	lastTurnID          sql.NullString
	nextTurnSeq         sql.NullInt64
	sessionHasAnyEvents bool
}

// appendStateQuery is step 1's read.
const appendStateQuery = `
		SELECT existing.id, existing.data::text,
		       nextval(pg_get_serial_sequence('events', 'id')), now(),
		       turn_index.index_version, turn_index.last_turn_id, turn_index.next_turn_seq,
		       EXISTS (SELECT 1 FROM events WHERE session_id = $2)
		FROM (SELECT 1) AS one
		LEFT JOIN events AS existing ON existing.idempotency_key = $1::text
		LEFT JOIN turn_index_sessions AS turn_index ON turn_index.session_id = $2`

func (state *appendState) scan(row pgx.Row) error {
	if err := row.Scan(&state.existingID, &state.existingData, &state.id, &state.createdAt,
		&state.indexVersion, &state.lastTurnID, &state.nextTurnSeq, &state.sessionHasAnyEvents); err != nil {
		return fmt.Errorf("read the turn index state: %w", err)
	}
	return nil
}

func (s *Store) appendEventOnConnection(ctx context.Context, connection *pgx.Conn, event EventToAppend) (appended AppendedEvent, err error) {
	var key any
	if event.IdempotencyKey != "" {
		key = event.IdempotencyKey
	}
	// An event with a required row is written in an explicit transaction:
	// BEGIN goes with step 1's read and COMMIT with step 2's batch, so it
	// costs no round trip more than the implicit one. A write that stops
	// part way leaves the connection in that transaction, so it is rolled
	// back here, and a connection that cannot say so is not used again.
	explicit := event.RequiredRow != nil
	if explicit {
		defer func() {
			if err != nil {
				err = endUnfinishedTransaction(ctx, connection, err)
			}
		}()
	}
	var state appendState
	if !explicit {
		if err := state.scan(connection.QueryRow(ctx, appendStateQuery, key, event.SessionID)); err != nil {
			return AppendedEvent{}, err
		}
	} else {
		rowFound, err := readStateInNewTransaction(ctx, connection, &state, key, event.SessionID, *event.RequiredRow)
		if err != nil {
			return AppendedEvent{}, err
		}
		if state.existingID.Valid || !rowFound {
			if _, err := connection.Exec(ctx, "ROLLBACK"); err != nil {
				return AppendedEvent{}, fmt.Errorf("end the event's transaction: %w", err)
			}
			if !rowFound && !state.existingID.Valid {
				return AppendedEvent{}, ErrRequiredRowMissing
			}
		}
	}
	if state.existingID.Valid {
		// The id step 1 drew is left unused; ids may have gaps.
		return AppendedEvent{ID: state.existingID.Int64, Data: []byte(state.existingData.String), AlreadyStored: true}, nil
	}

	appended = AppendedEvent{ID: state.id, Data: event.Data}
	if event.DataForID != nil {
		var err error
		if appended.Data, err = event.DataForID(state.id); err != nil {
			return AppendedEvent{}, err
		}
	}

	batch := &statementBatch{}
	batch.Exec(`INSERT INTO events (id, session_id, type, data, created_at, message_id, harness_message_id, tool_use_id, tool_name, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		appended.ID, event.SessionID, event.Type, string(appended.Data), state.createdAt,
		event.Fields.MessageID, event.Fields.HarnessMessageID, event.Fields.ToolUseID, event.Fields.ToolName, key)
	if err := s.indexLiveEvent(batch, state, event.SessionID, appended.ID, event.Type, event.Fields.TurnID, appended.Data); err != nil {
		return AppendedEvent{}, fmt.Errorf("index event in its turn: %w", err)
	}
	if err := s.updateSessionProjection(batch, event.SessionID, event.Type, event.Fields.HarnessSessionID, appended.Data); err != nil {
		logProjectionFailure("sessions projection", event.SessionID, event.Type, err)
	}
	if err := s.addEventToUsageByHour(batch, hourStartUnixOf(state.createdAt), event.SessionID, event.Type, appended.Data); err != nil {
		logProjectionFailure("hourly usage", event.SessionID, event.Type, err)
	}
	if event.AlsoWrite != nil {
		statements, err := event.AlsoWrite(appended.ID)
		if err != nil {
			return AppendedEvent{}, err
		}
		for _, statement := range statements {
			batch.Exec(statement.Query, statement.Arguments...)
		}
	}
	if explicit {
		batch.Exec("COMMIT")
	}
	if err := batch.send(ctx, connection); err != nil {
		return AppendedEvent{}, err
	}
	return appended, nil
}

// readStateInNewTransaction begins a transaction and reads step 1's state and
// the required row in it, in one round trip, and answers whether the row was
// there.
func readStateInNewTransaction(ctx context.Context, connection *pgx.Conn, state *appendState, key any, sessionID string, requiredRow Statement) (bool, error) {
	batch := &pgx.Batch{}
	batch.Queue("BEGIN")
	batch.Queue(appendStateQuery, key, sessionID)
	batch.Queue(requiredRow.Query, requiredRow.Arguments...)
	results := connection.SendBatch(ctx, batch)
	if _, err := results.Exec(); err != nil {
		return false, errors.Join(fmt.Errorf("begin the event's transaction: %w", err), results.Close())
	}
	if err := state.scan(results.QueryRow()); err != nil {
		return false, errors.Join(err, results.Close())
	}
	rows, err := results.Query()
	if err != nil {
		return false, errors.Join(fmt.Errorf("read the row the event belongs with: %w", err), results.Close())
	}
	found := rows.Next()
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, errors.Join(fmt.Errorf("read the row the event belongs with: %w", err), results.Close())
	}
	return found, results.Close()
}

// endUnfinishedTransaction rolls back the transaction a failed write left
// open. A connection whose state is unknown after that is given back to the
// pool as bad (driver.ErrBadConn), so database/sql closes it.
func endUnfinishedTransaction(ctx context.Context, connection *pgx.Conn, writeErr error) error {
	if connection.IsClosed() {
		return errors.Join(writeErr, driver.ErrBadConn)
	}
	if connection.PgConn().TxStatus() == 'I' {
		return writeErr
	}
	if _, err := connection.Exec(context.WithoutCancel(ctx), "ROLLBACK"); err != nil {
		return errors.Join(writeErr, fmt.Errorf("roll back the event's transaction: %w", err), driver.ErrBadConn)
	}
	return writeErr
}

// indexLiveEvent queues the writes that place one new event in its turn, by
// the rule BuildTurnIndex applies (writeEventTurn): its turn_id, else the
// session's last turn, else a turn named for the event. A session whose index
// is built by other rules, or that has older events and no index yet, is left
// to BuildTurnIndex, which indexes this event with the rest.
func (s *Store) indexLiveEvent(batch *statementBatch, state appendState, sessionID string, eventID int64, eventType, rawTurnID string, data []byte) error {
	var turn sessionTurnState
	switch {
	case state.indexVersion.Valid && int(state.indexVersion.Int64) != s.turnIndexVersion:
		return nil
	case state.indexVersion.Valid:
		turn = sessionTurnState{lastTurnID: state.lastTurnID.String, nextTurnSeq: state.nextTurnSeq.Int64}
	case state.sessionHasAnyEvents:
		return nil
	default:
		// This event is the session's first: its index starts current here.
		turn = sessionTurnState{nextTurnSeq: 1}
		batch.Exec(`INSERT INTO turn_index_sessions (session_id, index_version, indexed_through_event_id, last_turn_id, next_turn_seq) VALUES ($1, $2, 0, '', 1)`,
			sessionID, s.turnIndexVersion)
	}

	turnID := turn.assignTurn(eventID, rawTurnID)
	isPrompt := eventType == "user_message"
	proposedTurnSeq := turn.nextTurnSeq

	// The turn is opened with the next number, or extended if it exists; the
	// statements after it read its number back from the row.
	batch.Exec(`INSERT INTO turns (session_id, turn_seq, turn_id, first_event_id, last_event_id, event_count, has_user_message)
		VALUES ($1, $2, $3, $4, $4, 1, $5)
		ON CONFLICT (session_id, turn_id) DO UPDATE SET
			last_event_id = excluded.last_event_id,
			event_count = turns.event_count + 1,
			has_user_message = turns.has_user_message OR excluded.has_user_message`,
		sessionID, proposedTurnSeq, turnID, eventID, isPrompt)
	batch.Exec(`INSERT INTO event_turns (event_id, session_id, turn_seq)
		SELECT $1, $2, turn_seq FROM turns WHERE session_id = $2 AND turn_id = $3`,
		eventID, sessionID, turnID)
	if s.dedupCandidateOf != nil {
		keyHash, source, ok, err := s.dedupCandidateOf(eventType, data)
		if err != nil {
			return fmt.Errorf("dedup candidate of event %d: %w", eventID, err)
		}
		if ok {
			batch.Exec(`INSERT INTO dedup_candidates (event_id, session_id, turn_seq, key_hash, source)
				SELECT $1, $2, turn_seq, $4, $5 FROM turns WHERE session_id = $2 AND turn_id = $3`,
				eventID, sessionID, turnID, keyHash, source)
		}
	}
	// next_turn_seq moves on only when the turn above was opened with it: an
	// existing turn's number is always below it.
	batch.Exec(`UPDATE turn_index_sessions SET indexed_through_event_id = $1, last_turn_id = $2,
			next_turn_seq = CASE WHEN (SELECT turn_seq FROM turns WHERE session_id = $4 AND turn_id = $2) = $3 THEN $3 + 1 ELSE $3 END
		WHERE session_id = $4`,
		eventID, turnID, proposedTurnSeq, sessionID)
	return nil
}

// statementWriter is what a projection writes through: a statement and its
// arguments, whose result it does not read.
type statementWriter interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// statementBatch collects statements to send together.
type statementBatch struct {
	batch pgx.Batch
}

// Exec queues a statement. It answers nothing: the batch runs when sent.
func (b *statementBatch) Exec(query string, args ...any) (sql.Result, error) {
	b.batch.Queue(query, args...)
	return nil, nil
}

// send runs the batch as one implicit transaction and answers the first
// statement that failed, which rolls back all of them.
func (b *statementBatch) send(ctx context.Context, connection *pgx.Conn) error {
	results := connection.SendBatch(ctx, &b.batch)
	for index := 0; index < b.batch.Len(); index++ {
		if _, err := results.Exec(); err != nil {
			return errors.Join(fmt.Errorf("statement %d of the event's write: %w", index+1, err), results.Close())
		}
	}
	return results.Close()
}
