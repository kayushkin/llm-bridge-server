package store

import (
	"database/sql"
	"errors"
	"time"
)

// external_activity_marks records, per session, the time up to which the
// dash already has what the session's harness wrote. The external activity
// watcher (internal/server/external_activity.go) imports only entries written
// after it, when a person ran the conversation with the CLI outside the
// bridge. The harness manager moves it to the moment the bridge's own process
// for the session finished exiting, since everything before that the bridge
// reported itself.

// externalActivityMarkFormat is fixed-width so marks compare as text in SQL:
// RFC 3339 with trailing zeros trimmed does not ("…05Z" sorts after "…05.5Z").
const externalActivityMarkFormat = "2006-01-02T15:04:05.000000000Z"

// migrateExternalActivityMarks creates external_activity_marks. Called from
// migrate().
func (s *Store) migrateExternalActivityMarks() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS external_activity_marks (
			bridge_session_id TEXT PRIMARY KEY,
			imported_through  TEXT NOT NULL
		);
	`)
	return err
}

// ExternalActivityMark returns the session's mark, and false when it has none.
func (s *Store) ExternalActivityMark(bridgeSessionID string) (time.Time, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT imported_through FROM external_activity_marks WHERE bridge_session_id = ?`, bridgeSessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	mark, err := time.Parse(externalActivityMarkFormat, raw)
	if err != nil {
		return time.Time{}, false, err
	}
	return mark, true, nil
}

// AdvanceExternalActivityMark sets the session's mark to through, unless it
// already stands later: a mark only moves forward.
func (s *Store) AdvanceExternalActivityMark(bridgeSessionID string, through time.Time) error {
	value := through.UTC().Format(externalActivityMarkFormat)
	_, err := s.db.Exec(`
		INSERT INTO external_activity_marks (bridge_session_id, imported_through) VALUES (?, ?)
		ON CONFLICT(bridge_session_id) DO UPDATE SET imported_through = excluded.imported_through
		WHERE excluded.imported_through > external_activity_marks.imported_through`,
		bridgeSessionID, value)
	return err
}
