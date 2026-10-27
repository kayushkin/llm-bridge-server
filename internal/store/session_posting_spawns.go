package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A session's posting token is bound to one spawn of that session: every
// spawn draws a new random value, kept here, and signs it into the token it
// hands the harness child. A token from an earlier spawn names a value that is
// no longer the session's, so it dies when the session is spawned again, and
// the server refuses it outright once the session has ended.

func (s *Store) migrateSessionPostingSpawns() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS session_posting_spawns (
			session_id TEXT PRIMARY KEY,
			spawn_key  TEXT NOT NULL,
			minted_at  DATETIME NOT NULL
		);
	`)
	return err
}

// RecordSessionPostingSpawn makes spawnKey the session's current spawn, so
// only a posting token carrying it is accepted from now on.
func (s *Store) RecordSessionPostingSpawn(sessionID, spawnKey string) error {
	if sessionID == "" || spawnKey == "" {
		return errors.New("a posting spawn needs a session id and a spawn key")
	}
	_, err := s.db.Exec(`
		INSERT INTO session_posting_spawns (session_id, spawn_key, minted_at) VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET spawn_key = excluded.spawn_key, minted_at = excluded.minted_at`,
		sessionID, spawnKey, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record the posting spawn of session %s: %w", sessionID, err)
	}
	return nil
}

// SessionPostingSpawnKey is the session's current spawn key, and false when
// none was ever recorded.
func (s *Store) SessionPostingSpawnKey(sessionID string) (string, bool, error) {
	var spawnKey string
	err := s.db.QueryRow(`SELECT spawn_key FROM session_posting_spawns WHERE session_id = ?`, sessionID).Scan(&spawnKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the posting spawn of session %s: %w", sessionID, err)
	}
	return spawnKey, true, nil
}
