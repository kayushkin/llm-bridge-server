package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// storedEvent is one row of an events table. Both stores keep the same columns
// that matter here, and Data is the serialized msg.Event both were given.
type storedEvent struct {
	RowID     int64
	Type      string
	CreatedAt string
	Data      string
}

// sessionComparison is what one session's two copies disagree on. An event
// stored twice in one copy and once in the other appears once in the list.
type sessionComparison struct {
	SessionID      string
	BridgeCount    int
	LogStoreCount  int
	OnlyInBridge   []storedEvent
	OnlyInLogStore []storedEvent
}

func (c sessionComparison) differs() bool {
	return len(c.OnlyInBridge) > 0 || len(c.OnlyInLogStore) > 0
}

// compareSessionCopies matches the two copies of one session's events by the
// bytes of their data, as multisets: row ids and created_at differ between the
// stores by construction, and the body is the one thing both were handed.
func compareSessionCopies(sessionID string, bridge, logStore []storedEvent) sessionComparison {
	remainingInLogStore := map[[sha256.Size]byte][]storedEvent{}
	for _, ev := range logStore {
		digest := sha256.Sum256([]byte(ev.Data))
		remainingInLogStore[digest] = append(remainingInLogStore[digest], ev)
	}
	comparison := sessionComparison{SessionID: sessionID, BridgeCount: len(bridge), LogStoreCount: len(logStore)}
	for _, ev := range bridge {
		digest := sha256.Sum256([]byte(ev.Data))
		if matches := remainingInLogStore[digest]; len(matches) > 0 {
			remainingInLogStore[digest] = matches[1:]
			continue
		}
		comparison.OnlyInBridge = append(comparison.OnlyInBridge, ev)
	}
	for _, unmatched := range remainingInLogStore {
		comparison.OnlyInLogStore = append(comparison.OnlyInLogStore, unmatched...)
	}
	sort.Slice(comparison.OnlyInLogStore, func(i, j int) bool {
		return comparison.OnlyInLogStore[i].RowID < comparison.OnlyInLogStore[j].RowID
	})
	return comparison
}

// eventClass names the kind of an event for the report: its type, and for a
// system event its subtype too, since harness_id_set and the harness's own
// system messages go down different paths.
func eventClass(ev storedEvent) string {
	if ev.Type != "system" {
		return ev.Type
	}
	var body struct {
		System struct {
			Subtype string `json:"subtype"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &body); err != nil {
		return "system/(unparseable)"
	}
	return "system/" + body.System.Subtype
}

// sessionsQuietInWindow lists the sessions the bridge stored an event for at
// or after windowStart and none at or after settledBefore. A session still
// writing is left for a later run: its log-store copy is pushed from a queue
// and may be a few events behind without anything being wrong.
//
// Both bounds are compared as text against created_at, which SQLite's
// CURRENT_TIMESTAMP writes as "YYYY-MM-DD HH:MM:SS" in UTC.
func sessionsQuietInWindow(bridge *sql.DB, windowStart, settledBefore string) ([]string, error) {
	rows, err := bridge.Query(`SELECT session_id FROM events WHERE created_at >= ?
		GROUP BY session_id HAVING max(created_at) < ? ORDER BY session_id`, windowStart, settledBefore)
	if err != nil {
		return nil, fmt.Errorf("list sessions in the window: %w", err)
	}
	defer rows.Close()
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	return sessionIDs, rows.Err()
}

// latestEventCreatedAt is the newest created_at the store holds for the
// session, or "" when it holds none.
func latestEventCreatedAt(db *sql.DB, sessionID string) (string, error) {
	var latest sql.NullString
	if err := db.QueryRow(`SELECT max(created_at) FROM events WHERE session_id=?`, sessionID).Scan(&latest); err != nil {
		return "", fmt.Errorf("latest event of %s: %w", sessionID, err)
	}
	return latest.String, nil
}

func readSessionEvents(db *sql.DB, sessionID string) ([]storedEvent, error) {
	rows, err := db.Query(`SELECT id, type, created_at, data FROM events WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read events of %s: %w", sessionID, err)
	}
	defer rows.Close()
	var events []storedEvent
	for rows.Next() {
		var ev storedEvent
		if err := rows.Scan(&ev.RowID, &ev.Type, &ev.CreatedAt, &ev.Data); err != nil {
			return nil, fmt.Errorf("read events of %s: %w", sessionID, err)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}
