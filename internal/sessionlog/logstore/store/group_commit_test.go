package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Many sessions writing at once are written in groups. Every event is stored
// once, in its session's order; an event whose required row is missing, and
// one found under its key, store nothing; two writers of one session never
// share a group; and each session's turns, placed live in groups, are the
// turns a rebuild of its index places.
func TestEventsOfManySessionsWrittenAtOnceAreEachStoredOnceAndInTheirTurns(t *testing.T) {
	s := newTestStore(t)
	const sessions, turns, eventsPerTurn = 16, 4, 9
	ctx := context.Background()
	present := &Statement{Query: `SELECT 1`}
	missing := &Statement{Query: `SELECT 1 WHERE false`}

	var wait sync.WaitGroup
	idsBySession := make([][]int64, sessions)
	failures := make(chan error, sessions*turns*eventsPerTurn*3)
	for session := 0; session < sessions; session++ {
		sessionID := fmt.Sprintf("br_group_%02d", session)
		wait.Add(1)
		go func() {
			defer wait.Done()
			for turn := 0; turn < turns; turn++ {
				turnID := fmt.Sprintf("turn-%d", turn)
				for index := 0; index < eventsPerTurn; index++ {
					eventType := "block"
					if index == 0 {
						eventType = "user_message"
					}
					event := EventToAppend{SessionID: sessionID, Type: eventType, Fields: EventFields{TurnID: turnID},
						Data: []byte(fmt.Sprintf(`{"type":%q,"turn_id":%q,"n":%d}`, eventType, turnID, index))}
					switch index % 3 {
					case 1:
						event.RequiredRow = present
					case 2:
						event.IdempotencyKey = fmt.Sprintf("%s:%d:%d", sessionID, turn, index)
					}
					appended, err := s.AppendEvent(ctx, event)
					if err != nil {
						failures <- fmt.Errorf("%s turn %d event %d: %w", sessionID, turn, index, err)
						return
					}
					idsBySession[session] = append(idsBySession[session], appended.ID)
					if event.IdempotencyKey != "" {
						again, err := s.AppendEvent(ctx, event)
						if err != nil || !again.AlreadyStored || again.ID != appended.ID {
							failures <- fmt.Errorf("%s: the key's second write answered %+v, %v; want event %d found stored", sessionID, again, err, appended.ID)
						}
					}
					refused := EventToAppend{SessionID: sessionID, Type: "session_status", Data: []byte(`{"type":"session_status"}`), RequiredRow: missing}
					if _, err := s.AppendEvent(ctx, refused); !errors.Is(err, ErrRequiredRowMissing) {
						failures <- fmt.Errorf("%s: an event with no row answered %v; want ErrRequiredRowMissing", sessionID, err)
					}
				}
			}
		}()
	}
	// A second writer of four of the sessions, at the same time.
	extraBySession := map[string]int{}
	var extraMutex sync.Mutex
	for session := 0; session < 4; session++ {
		sessionID := fmt.Sprintf("br_group_%02d", session)
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < 10; index++ {
				if _, err := s.AppendEvent(ctx, EventToAppend{SessionID: sessionID, Type: "system", Data: []byte(`{"type":"system"}`)}); err != nil {
					failures <- err
					return
				}
				extraMutex.Lock()
				extraBySession[sessionID]++
				extraMutex.Unlock()
			}
		}()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}

	for session := 0; session < sessions; session++ {
		sessionID := fmt.Sprintf("br_group_%02d", session)
		ids := idsBySession[session]
		for index := 1; index < len(ids); index++ {
			if ids[index] <= ids[index-1] {
				t.Fatalf("%s: event %d has id %d after %d; one writer's events must number in its order", sessionID, index, ids[index], ids[index-1])
			}
		}
		var stored int
		if err := s.reader.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = $1`, sessionID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if want := turns*eventsPerTurn + extraBySession[sessionID]; stored != want {
			t.Errorf("%s: %d events stored; want %d, each once, none for a missing row", sessionID, stored, want)
		}
		live, err := s.NewestTurns(sessionID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.deletePartialTurnIndex(sessionID); err != nil {
			t.Fatal(err)
		}
		if err := s.BuildTurnIndex(sessionID); err != nil {
			t.Fatal(err)
		}
		rebuilt, err := s.NewestTurns(sessionID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != len(rebuilt) {
			t.Fatalf("%s: %d turns placed live, %d by a rebuild", sessionID, len(live), len(rebuilt))
		}
		for index := range live {
			a, b := live[index], rebuilt[index]
			if a.Seq != b.Seq || a.TurnID != b.TurnID || a.FirstEventID != b.FirstEventID || a.LastEventID != b.LastEventID || a.EventCount != b.EventCount || a.HasUserMessage != b.HasUserMessage {
				t.Errorf("%s: turn placed live %+v, by a rebuild %+v", sessionID, a, b)
			}
		}
	}
}

// A group whose batch fails is written again event by event, so only the
// failing event fails.
func TestAFailingEventInAGroupFailsAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	failing := func(int64) ([]Statement, error) {
		return []Statement{{Query: `SELECT 1 / 0`}}, nil
	}
	var wait sync.WaitGroup
	results := make([]error, 12)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			event := EventToAppend{SessionID: fmt.Sprintf("br_alone_%02d", index), Type: "system", Data: []byte(`{"type":"system"}`)}
			if index == 5 {
				event.AlsoWrite = failing
			}
			_, results[index] = s.AppendEvent(ctx, event)
		}()
	}
	wait.Wait()
	for index, err := range results {
		if (index == 5) != (err != nil) {
			t.Errorf("event %d answered %v", index, err)
		}
	}
	var stored int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id LIKE 'br_alone_%'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != len(results)-1 {
		t.Errorf("%d events stored; want every one but the failing one", stored)
	}
}
