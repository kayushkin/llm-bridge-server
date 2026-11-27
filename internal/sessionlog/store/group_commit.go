package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Events of different sessions that arrive while an earlier write is going
// to Postgres are written together: one transaction, two round trips and one
// commit for all of them (group commit). A commit waits for the disk, so with
// default durability one commit per event put every live session's events in
// a line behind each other's flush; measured with 5 and 20 sessions, that
// line was the tail of the live path.
//
// A group holds at most one event of each session. Events of one session are
// written in the order they arrived, each after the one before it committed,
// because an event's turn is placed from the state its session's earlier
// events left. A group whose writes fail is rolled back and each of its
// events is written again on its own, so one event's failure is never
// another's.

// maximumEventsInAGroup bounds a group, and so the size of its batches.
const maximumEventsInAGroup = 64

// errStoreClosed is a write that arrived after Close.
var errStoreClosed = errors.New("session log: the store is closed")

// appendRequest is one AppendEvent waiting to be written.
type appendRequest struct {
	ctx    context.Context
	event  EventToAppend
	result chan appendResult
}

type appendResult struct {
	appended AppendedEvent
	err      error
}

// groupWriter is the goroutine that writes every appended event.
type groupWriter struct {
	requests chan *appendRequest
	stop     chan struct{}
	stopped  chan struct{}
	once     sync.Once
}

func newGroupWriter() *groupWriter {
	return &groupWriter{requests: make(chan *appendRequest), stop: make(chan struct{}), stopped: make(chan struct{})}
}

// closeGroupWriter stops the writer after the group it is writing.
func (s *Store) closeGroupWriter() {
	s.appends.once.Do(func() { close(s.appends.stop) })
	<-s.appends.stopped
}

// submitAppend hands an event to the writer and waits for its result. It
// waits even when ctx ends: the writer may be running the event's DataForID
// or AlsoWrite, which the caller's own values are read by. ctx bounds the
// write itself, and an event whose ctx ended before its group began is not
// written.
func (s *Store) submitAppend(ctx context.Context, event EventToAppend) (AppendedEvent, error) {
	request := &appendRequest{ctx: ctx, event: event, result: make(chan appendResult, 1)}
	select {
	case s.appends.requests <- request:
	case <-ctx.Done():
		return AppendedEvent{}, ctx.Err()
	case <-s.appends.stop:
		return AppendedEvent{}, errStoreClosed
	}
	result := <-request.result
	return result.appended, result.err
}

// writeAppendsInGroups is the writer: it takes the waiting events of distinct
// sessions as one group, writes them, and answers each.
func (s *Store) writeAppendsInGroups() {
	defer close(s.appends.stopped)
	// held are events whose session already has an event in the group being
	// formed, or an earlier held one; they lead the next group, in order.
	var held []*appendRequest
	for {
		var group []*appendRequest
		inGroup := map[string]bool{}
		heldSessions := map[string]bool{}
		var stillHeld []*appendRequest
		for _, request := range held {
			session := request.event.SessionID
			if inGroup[session] || heldSessions[session] || len(group) == maximumEventsInAGroup {
				stillHeld = append(stillHeld, request)
				heldSessions[session] = true
				continue
			}
			group = append(group, request)
			inGroup[session] = true
		}
		held = stillHeld
		if len(group) == 0 {
			select {
			case request := <-s.appends.requests:
				group = append(group, request)
				inGroup[request.event.SessionID] = true
			case <-s.appends.stop:
				return
			}
		}
	drain:
		for len(group) < maximumEventsInAGroup {
			select {
			case request := <-s.appends.requests:
				session := request.event.SessionID
				if inGroup[session] || heldSessions[session] {
					held = append(held, request)
					heldSessions[session] = true
					continue
				}
				group = append(group, request)
				inGroup[session] = true
			default:
				break drain
			}
		}
		s.writeGroup(group)
	}
}

// writeGroup writes one group and answers each of its events.
func (s *Store) writeGroup(group []*appendRequest) {
	var live []*appendRequest
	for _, request := range group {
		if err := request.ctx.Err(); err != nil {
			request.result <- appendResult{err: err}
			continue
		}
		live = append(live, request)
	}
	if len(live) == 0 {
		return
	}
	if len(live) == 1 {
		appended, err := s.appendOne(live[0].ctx, live[0].event)
		live[0].result <- appendResult{appended: appended, err: err}
		return
	}
	ctx, cancel := latestDeadlineOf(live)
	defer cancel()
	events := make([]EventToAppend, len(live))
	for index, request := range live {
		events[index] = request.event
	}
	results, err := s.appendTogether(ctx, events)
	if err != nil {
		// Nothing of the group committed. Each is written on its own, so
		// a failure is the failing event's alone.
		for _, request := range live {
			appended, err := s.appendOne(request.ctx, request.event)
			request.result <- appendResult{appended: appended, err: err}
		}
		return
	}
	for index, request := range live {
		request.result <- results[index]
	}
}

// latestDeadlineOf is a context for a group's write that ends when the last
// of its events' contexts would: a group never runs past what one of its
// callers allowed, and no caller's write is cut short by another's.
func latestDeadlineOf(group []*appendRequest) (context.Context, context.CancelFunc) {
	var latest time.Time
	for _, request := range group {
		deadline, ok := request.ctx.Deadline()
		if !ok {
			return context.WithCancel(context.Background())
		}
		if deadline.After(latest) {
			latest = deadline
		}
	}
	return context.WithDeadline(context.Background(), latest)
}

// appendOne writes one event alone, as appendEventOnConnection describes.
func (s *Store) appendOne(ctx context.Context, event EventToAppend) (AppendedEvent, error) {
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

// appendTogether writes events of distinct sessions in one transaction: BEGIN
// and every event's first read (and required row) in one round trip, then
// every write and COMMIT in the second. An event found under its key, or
// whose required row is missing, is answered in its result and written
// nothing. An error means nothing was committed.
func (s *Store) appendTogether(ctx context.Context, events []EventToAppend) ([]appendResult, error) {
	connection, err := s.writer.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	var results []appendResult
	err = connection.Raw(func(driverConnection any) error {
		var err error
		results, err = s.appendTogetherOnConnection(ctx, driverConnection.(*stdlib.Conn).Conn(), events)
		return err
	})
	return results, err
}

func (s *Store) appendTogetherOnConnection(ctx context.Context, connection *pgx.Conn, events []EventToAppend) (results []appendResult, err error) {
	defer func() {
		if err != nil {
			err = endUnfinishedTransaction(ctx, connection, err)
		}
	}()
	read := &pgx.Batch{}
	read.Queue("BEGIN")
	for _, event := range events {
		var key any
		if event.IdempotencyKey != "" {
			key = event.IdempotencyKey
		}
		read.Queue(appendStateQuery, key, event.SessionID)
		if event.RequiredRow != nil {
			read.Queue(event.RequiredRow.Query, event.RequiredRow.Arguments...)
		}
	}
	states := make([]appendState, len(events))
	rowFound := make([]bool, len(events))
	sent := connection.SendBatch(ctx, read)
	if _, err := sent.Exec(); err != nil {
		return nil, errors.Join(fmt.Errorf("begin the group's transaction: %w", err), sent.Close())
	}
	for index, event := range events {
		if err := states[index].scan(sent.QueryRow()); err != nil {
			return nil, errors.Join(err, sent.Close())
		}
		if event.RequiredRow == nil {
			continue
		}
		rows, err := sent.Query()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("read the row the event belongs with: %w", err), sent.Close())
		}
		rowFound[index] = rows.Next()
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, errors.Join(fmt.Errorf("read the row the event belongs with: %w", err), sent.Close())
		}
	}
	if err := sent.Close(); err != nil {
		return nil, err
	}

	results = make([]appendResult, len(events))
	batch := &statementBatch{}
	for index, event := range events {
		state := states[index]
		switch {
		case state.existingID.Valid:
			// The id step 1 drew is left unused; ids may have gaps.
			results[index].appended = AppendedEvent{ID: state.existingID.Int64, Data: []byte(state.existingData.String), AlreadyStored: true}
		case event.RequiredRow != nil && !rowFound[index]:
			results[index].err = ErrRequiredRowMissing
		default:
			appended, err := s.queueEventWrites(batch, state, event)
			if err != nil {
				return nil, err
			}
			results[index].appended = appended
		}
	}
	batch.Exec("COMMIT")
	if err := batch.send(ctx, connection); err != nil {
		return nil, err
	}
	return results, nil
}
