package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	_ "modernc.org/sqlite"
)

// Session is the canonical type from llm-bridge/msg/server.go.
// DO NOT define API types here. Add them to msg/ instead, then run
// generate-ts.sh so the TypeScript frontend stays in sync.
// Kept as a type alias so existing store code compiles unchanged.
type Session = msg.ManagedSession

type Store struct {
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// Enable WAL mode and busy timeout to handle concurrent writes.
	if _, err := d.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		d.Close()
		return nil, fmt.Errorf("sqlite pragmas: %w", err)
	}

	s := &Store{db: d}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	if err := s.migrateSlots(); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id           TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT '',
			harness      TEXT NOT NULL,
			state        TEXT NOT NULL,
			pid          INTEGER NOT NULL DEFAULT 0,
			agent_id     TEXT NOT NULL DEFAULT '',
			spawner_id   TEXT NOT NULL DEFAULT '',
			parent_id    TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state);
		CREATE INDEX IF NOT EXISTS idx_sessions_harness ON sessions(harness);

		CREATE TABLE IF NOT EXISTS events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			type       TEXT NOT NULL,
			data       TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (session_id) REFERENCES sessions(id)
		);
		CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id);
	`)
	if err != nil {
		return err
	}
	// Migrations for existing DBs
	s.db.Exec("ALTER TABLE sessions ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN instance_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN client_request_id TEXT NOT NULL DEFAULT ''")
	return nil
}

func (s *Store) CreateSession(sess *Session) error {
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.UpdatedAt = now
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, display_name, harness, instance_id, state, pid, agent_id, spawner_id, parent_id, client_request_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.DisplayName, sess.Harness, sess.InstanceID, sess.State, sess.PID, sess.AgentID, sess.SpawnerID, sess.ParentID, sess.ClientRequestID, sess.CreatedAt, sess.UpdatedAt,
	)
	return err
}

func (s *Store) GetSession(id string) (*Session, error) {
	var sess Session
	err := s.db.QueryRow(
		`SELECT id, display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, spawner_id, parent_id, COALESCE(client_request_id, ''), created_at, updated_at FROM sessions WHERE id=?`,
		id,
	).Scan(&sess.ID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.SpawnerID, &sess.ParentID, &sess.ClientRequestID, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, spawner_id, parent_id, COALESCE(client_request_id, ''), created_at, updated_at FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.SpawnerID, &sess.ParentID, &sess.ClientRequestID, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *Store) ListSessionsByState(state string) ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, spawner_id, parent_id, COALESCE(client_request_id, ''), created_at, updated_at FROM sessions WHERE state=? ORDER BY created_at DESC`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.SpawnerID, &sess.ParentID, &sess.ClientRequestID, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *Store) UpdateSessionState(id, state string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET state=?, updated_at=? WHERE id=?`, state, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateSessionPID(id string, pid int) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET pid=?, updated_at=? WHERE id=?`, pid, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RemapSessionID updates a session's ID (temp → real harness ID transition).
// Updates the session row and all associated events.
func (s *Store) RemapSessionID(oldID, newID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Update session ID
	now := time.Now().UTC()
	_, err = tx.Exec(`UPDATE sessions SET id=?, updated_at=? WHERE id=?`, newID, now, oldID)
	if err != nil {
		return fmt.Errorf("update session id: %w", err)
	}

	// Update all events to use new session ID
	_, err = tx.Exec(`UPDATE events SET session_id=? WHERE session_id=?`, newID, oldID)
	if err != nil {
		return fmt.Errorf("update events session_id: %w", err)
	}

	return tx.Commit()
}

// StoreEvent persists a serialized event for a session.
func (s *Store) StoreEvent(sessionID, eventType string, data []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO events (session_id, type, data) VALUES (?,?,?)`,
		sessionID, eventType, string(data),
	)
	return err
}


// MaxEventID returns the highest event row ID for a session.
func (s *Store) MaxEventID(sessionID string) (int, error) {
	var id int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id=?`, sessionID).Scan(&id)
	return id, err
}

// EventWithID is a raw event with its database row ID.
type EventWithID struct {
	RowID int
	Data  json.RawMessage
}

// ListCurrentTurnEventsWithIDs returns current-turn events with row IDs.
func (s *Store) ListCurrentTurnEventsWithIDs(sessionID string) ([]EventWithID, error) {
	var lastUserID int
	_ = s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id=? AND type='user_message'`,
		sessionID,
	).Scan(&lastUserID)

	rows, err := s.db.Query(
		`SELECT id, data FROM events WHERE session_id=? AND id > ? AND type != 'user_message' ORDER BY id ASC`,
		sessionID, lastUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventWithID
	for rows.Next() {
		var ev EventWithID
		var data string
		if err := rows.Scan(&ev.RowID, &data); err != nil {
			return nil, err
		}
		ev.Data = json.RawMessage(data)
		events = append(events, ev)
	}
	return events, rows.Err()
}

// ListEventsSinceID returns events after a specific row ID (for SSE reconnection).
func (s *Store) ListEventsSinceID(sessionID string, afterID int) ([]EventWithID, error) {
	rows, err := s.db.Query(
		`SELECT id, data FROM events WHERE session_id=? AND id > ? ORDER BY id ASC`,
		sessionID, afterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventWithID
	for rows.Next() {
		var ev EventWithID
		var data string
		if err := rows.Scan(&ev.RowID, &data); err != nil {
			return nil, err
		}
		ev.Data = json.RawMessage(data)
		events = append(events, ev)
	}
	return events, rows.Err()
}

// StoreEventReturningID persists an event and returns its row ID.
func (s *Store) StoreEventReturningID(sessionID, eventType string, data []byte) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO events (session_id, type, data) VALUES (?,?,?)`,
		sessionID, eventType, string(data),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) DeleteSession(id string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpsertDiscoveredSession inserts a discovered session if it doesn't already exist.
// instanceID is the instance that discovered this session (the one running the harness binary).
// Returns true if a new row was inserted.
func (s *Store) UpsertDiscoveredSession(id, displayName, harness, instanceID string, createdAt, updatedAt time.Time) (bool, error) {
	// Check if session already exists
	var existingInstanceID, existingDisplayName string
	err := s.db.QueryRow(`SELECT COALESCE(instance_id, ''), COALESCE(display_name, '') FROM sessions WHERE id=?`, id).Scan(&existingInstanceID, &existingDisplayName)
	if err == nil {
		// Already exists - update timestamp, display_name, and instance_id if currently empty
		newInstanceID := existingInstanceID
		if existingInstanceID == "" && instanceID != "" {
			newInstanceID = instanceID
		}
		newDisplayName := existingDisplayName
		// Update display_name if empty OR if current is a path-based fallback and new one is a real prompt
		if displayName != "" && (existingDisplayName == "" || (strings.HasPrefix(existingDisplayName, "/") && !strings.HasPrefix(displayName, "/"))) {
			newDisplayName = displayName
		}
		s.db.Exec(`UPDATE sessions SET updated_at=?, instance_id=?, display_name=? WHERE id=?`, updatedAt, newInstanceID, newDisplayName, id)
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}

	// Insert new discovered session with state "idle"
	_, err = s.db.Exec(
		`INSERT INTO sessions (id, display_name, harness, instance_id, state, pid, agent_id, spawner_id, parent_id, client_request_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, displayName, harness, instanceID, "idle", 0, "", "", "", "", createdAt, updatedAt,
	)
	if err != nil {
		return false, err
	}
	return true, nil
}
