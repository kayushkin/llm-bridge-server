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
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			bridge_id    TEXT PRIMARY KEY,
			harness_id   TEXT NOT NULL DEFAULT '',
			client_id    TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			harness      TEXT NOT NULL,
			state        TEXT NOT NULL,
			pid          INTEGER NOT NULL DEFAULT 0,
			agent_id     TEXT NOT NULL DEFAULT '',
			spawner_id   TEXT NOT NULL DEFAULT '',
			parent_id    TEXT NOT NULL DEFAULT '',
			instance_id  TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state);
		CREATE INDEX IF NOT EXISTS idx_sessions_harness ON sessions(harness);

		CREATE TABLE IF NOT EXISTS events (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id         TEXT NOT NULL,
			type               TEXT NOT NULL,
			message_id         TEXT NOT NULL DEFAULT '',
			harness_message_id TEXT NOT NULL DEFAULT '',
			data               TEXT NOT NULL,
			created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (session_id) REFERENCES sessions(bridge_id)
		);
		CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id);
	`)
	if err != nil {
		return err
	}
	// Add message id columns to existing event tables created by older versions.
	s.db.Exec(`ALTER TABLE events ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE events ADD COLUMN harness_message_id TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_message_id ON events(session_id, message_id)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_harness_msg_id ON events(session_id, harness_message_id)`)
	// Migrations for existing DBs (old schema used 'id' as PK)
	s.db.Exec("ALTER TABLE sessions ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN instance_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN harness_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN client_id TEXT NOT NULL DEFAULT ''")
	// Backfill: old rows have 'id' but no bridge_id — handled by the rename below.
	// If upgrading from old schema where PK was 'id', rename it to bridge_id.
	s.db.Exec("ALTER TABLE sessions RENAME COLUMN id TO bridge_id")
	s.db.Exec("ALTER TABLE sessions RENAME COLUMN client_request_id TO client_id")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN harness_config TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN info TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN folder_name TEXT NOT NULL DEFAULT ''")
	// Index on harness_id must be created after ALTER TABLE migration adds the column.
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_harness_id ON sessions(harness_id)")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_folder ON sessions(folder_name)")

	// Folder registry — tracks the ordered list of user-defined folders.
	// Folders may exist with no sessions (created and not yet populated).
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS folders (
			name     TEXT PRIMARY KEY,
			position INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_folders_position ON folders(position);
	`)
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateSession(sess *Session) error {
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.UpdatedAt = now
	harnessConfig := ""
	if sess.HarnessConfig != nil {
		harnessConfig = string(sess.HarnessConfig)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions (bridge_id, harness_id, client_id, display_name, harness, instance_id, state, pid, agent_id, spawner_id, parent_id, harness_config, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.BridgeID, sess.HarnessID, sess.ClientID, sess.DisplayName, sess.Harness, sess.InstanceID, sess.State, sess.PID, sess.AgentID, sess.SpawnerID, sess.ParentID, harnessConfig, sess.CreatedAt, sess.UpdatedAt,
	)
	return err
}

const sessionColumns = `bridge_id, COALESCE(harness_id, ''), COALESCE(client_id, ''), display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, spawner_id, parent_id, COALESCE(harness_config, ''), COALESCE(info, ''), COALESCE(folder_name, ''), created_at, updated_at`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var harnessConfig string
	var info string
	err := sc.Scan(&sess.BridgeID, &sess.HarnessID, &sess.ClientID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.SpawnerID, &sess.ParentID, &harnessConfig, &info, &sess.FolderName, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if harnessConfig != "" {
		sess.HarnessConfig = json.RawMessage(harnessConfig)
	}
	if info != "" {
		var parsed msg.SessionInfo
		if err := json.Unmarshal([]byte(info), &parsed); err == nil {
			sess.Info = &parsed
		}
	}
	return &sess, nil
}

// SetSessionInfo persists the harness-reported session info for a session.
func (s *Store) SetSessionInfo(bridgeID string, info *msg.SessionInfo) error {
	now := time.Now().UTC()
	var payload string
	if info != nil {
		data, err := json.Marshal(info)
		if err != nil {
			return fmt.Errorf("marshal session info: %w", err)
		}
		payload = string(data)
	}
	_, err := s.db.Exec(`UPDATE sessions SET info=?, updated_at=? WHERE bridge_id=?`, payload, now, bridgeID)
	return err
}

// GetSession looks up a session by bridge_id.
func (s *Store) GetSession(bridgeID string) (*Session, error) {
	return scanSession(s.db.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE bridge_id=?`, bridgeID,
	))
}

// GetSessionByHarnessID looks up a session by its harness-reported session ID.
func (s *Store) GetSessionByHarnessID(harnessID string) (*Session, error) {
	return scanSession(s.db.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE harness_id=?`, harnessID,
	))
}

func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT ` + sessionColumns + ` FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, *sess)
	}
	return sessions, rows.Err()
}

func (s *Store) ListSessionsByState(state string) ([]Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions WHERE state=? ORDER BY created_at DESC`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, *sess)
	}
	return sessions, rows.Err()
}

func (s *Store) UpdateSessionState(bridgeID, state string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET state=?, updated_at=? WHERE bridge_id=?`, state, now, bridgeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateSessionPID(bridgeID string, pid int) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET pid=?, updated_at=? WHERE bridge_id=?`, pid, now, bridgeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetHarnessID fills in the harness-reported session ID on a session.
func (s *Store) SetHarnessID(bridgeID, harnessID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE sessions SET harness_id=?, updated_at=? WHERE bridge_id=?`, harnessID, now, bridgeID)
	return err
}

// SetDisplayNameIfEmpty sets display_name only when it is currently empty.
// Returns true if a row was updated.
func (s *Store) SetDisplayNameIfEmpty(bridgeID, displayName string) (bool, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE sessions SET display_name=?, updated_at=? WHERE bridge_id=? AND display_name=''`,
		displayName, now, bridgeID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateSessionDisplayName sets the session's display_name unconditionally.
func (s *Store) UpdateSessionDisplayName(bridgeID, displayName string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE sessions SET display_name=?, updated_at=? WHERE bridge_id=?`,
		displayName, now, bridgeID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}


// StoreEvent persists a serialized event for a session. messageID and
// harnessMessageID may be empty for events that don't belong to a chat
// message (system, session_state, session_info, etc).
func (s *Store) StoreEvent(sessionID, eventType, messageID, harnessMessageID string, data []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO events (session_id, type, message_id, harness_message_id, data) VALUES (?,?,?,?,?)`,
		sessionID, eventType, messageID, harnessMessageID, string(data),
	)
	return err
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
func (s *Store) StoreEventReturningID(sessionID, eventType, messageID, harnessMessageID string, data []byte) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO events (session_id, type, message_id, harness_message_id, data) VALUES (?,?,?,?,?)`,
		sessionID, eventType, messageID, harnessMessageID, string(data),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// HarnessToBridgeMap returns the (harness_message_id → bridge message_id)
// mapping for a session, used to rehydrate manager state after a process
// restart so resume-replays from the harness can be reconciled back to
// their original bridge messages.
func (s *Store) HarnessToBridgeMap(sessionID string) (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT harness_message_id, message_id FROM events
		 WHERE session_id=? AND harness_message_id != '' AND message_id != ''
		 GROUP BY harness_message_id`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var h, b string
		if err := rows.Scan(&h, &b); err != nil {
			return nil, err
		}
		out[h] = b
	}
	return out, rows.Err()
}

// ToolUseBinding pairs a tool_use_id with the bridge and harness message ids
// of the bubble that contained it. Used to resolve task_progress events (which
// carry tool_use_id) back to their message bubble.
type ToolUseBinding struct {
	BridgeMessageID  string
	HarnessMessageID string
}

// ToolUseToMessageMap returns the (tool_use_id → bubble message ids) mapping
// for a session, used to rehydrate manager state so task_progress events
// received after a process restart can still be correlated. Scans existing
// tool_call/tool_result events in the DB; tool_use_id is pulled from the
// stored event JSON.
func (s *Store) ToolUseToMessageMap(sessionID string) (map[string]ToolUseBinding, error) {
	rows, err := s.db.Query(
		`SELECT
			COALESCE(json_extract(data, '$.tool_call.tool_id'),
			         json_extract(data, '$.tool_result.tool_id')) AS tool_use_id,
			message_id,
			harness_message_id
		 FROM events
		 WHERE session_id=?
		   AND type IN ('tool_call', 'tool_result')
		   AND message_id != ''`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]ToolUseBinding)
	for rows.Next() {
		var tid sql.NullString
		var b, h string
		if err := rows.Scan(&tid, &b, &h); err != nil {
			return nil, err
		}
		if !tid.Valid || tid.String == "" {
			continue
		}
		if _, seen := out[tid.String]; seen {
			continue
		}
		out[tid.String] = ToolUseBinding{BridgeMessageID: b, HarnessMessageID: h}
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(bridgeID string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE bridge_id=?`, bridgeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ── Folder management ─────────────────────────────────────────────────────────

// ListFolders returns all folder names ordered by their stored position.
func (s *Store) ListFolders() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM folders ORDER BY position ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CreateFolder appends a folder to the registry. No-op if it already exists.
func (s *Store) CreateFolder(name string) error {
	_, err := s.db.Exec(
		`INSERT INTO folders (name, position) VALUES (?, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
		name,
	)
	return err
}

// DeleteFolder removes a folder from the registry and clears its assignment
// from any sessions currently in it. Sessions are not deleted; they become unfiled.
func (s *Store) DeleteFolder(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE sessions SET folder_name='' WHERE folder_name=?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM folders WHERE name=?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// RenameFolder renames a folder, preserving its position. If newName already
// exists, the two folders are merged: sessions in oldName move to newName and
// the oldName row is dropped.
func (s *Store) RenameFolder(oldName, newName string) error {
	if oldName == newName || oldName == "" || newName == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE sessions SET folder_name=? WHERE folder_name=?`, newName, oldName); err != nil {
		return err
	}
	// Check whether newName already exists (merge case).
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM folders WHERE name=?`, newName).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		if _, err := tx.Exec(`DELETE FROM folders WHERE name=?`, oldName); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE folders SET name=? WHERE name=?`, newName, oldName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetSessionFolder assigns a session to a folder. Empty folder clears the
// assignment. Auto-creates the folder in the registry if it doesn't exist.
func (s *Store) SetSessionFolder(bridgeID, folder string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if folder != "" {
		if _, err := tx.Exec(
			`INSERT INTO folders (name, position) VALUES (?, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
			folder,
		); err != nil {
			return err
		}
	}
	res, err := tx.Exec(
		`UPDATE sessions SET folder_name=?, updated_at=? WHERE bridge_id=?`,
		folder, time.Now().UTC(), bridgeID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// UpsertDiscoveredSession inserts a discovered session if it doesn't already exist.
// harnessID is the harness-native session ID (e.g. CC UUID).
// instanceID is the instance that discovered this session.
// Returns true if a new row was inserted.
func (s *Store) UpsertDiscoveredSession(harnessID, displayName, harness, instanceID string, createdAt, updatedAt time.Time) (bool, error) {
	// Check if session already exists by harness_id
	var existingBridgeID, existingInstanceID, existingDisplayName string
	err := s.db.QueryRow(`SELECT bridge_id, COALESCE(instance_id, ''), COALESCE(display_name, '') FROM sessions WHERE harness_id=?`, harnessID).Scan(&existingBridgeID, &existingInstanceID, &existingDisplayName)
	if err == nil {
		// Already exists - update timestamp, display_name, and instance_id if currently empty
		newInstanceID := existingInstanceID
		if existingInstanceID == "" && instanceID != "" {
			newInstanceID = instanceID
		}
		newDisplayName := existingDisplayName
		if displayName != "" && (existingDisplayName == "" || (strings.HasPrefix(existingDisplayName, "/") && !strings.HasPrefix(displayName, "/"))) {
			newDisplayName = displayName
		}
		s.db.Exec(`UPDATE sessions SET updated_at=?, instance_id=?, display_name=? WHERE bridge_id=?`, updatedAt, newInstanceID, newDisplayName, existingBridgeID)
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}

	// Insert new discovered session with state "idle"
	bridgeID := fmt.Sprintf("br_%d", time.Now().UnixNano())
	_, err = s.db.Exec(
		`INSERT INTO sessions (bridge_id, harness_id, client_id, display_name, harness, instance_id, state, pid, agent_id, spawner_id, parent_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		bridgeID, harnessID, "", displayName, harness, instanceID, "idle", 0, "", "", "", createdAt, updatedAt,
	)
	if err != nil {
		return false, err
	}
	return true, nil
}
