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

// Notifier receives session-row mutation signals from the store. Set via
// SetNotifier; nil-safe. Implementations must be non-blocking (drop, don't
// stall the writer).
type Notifier interface {
	OnSessionChanged(bridgeID string)
	OnSessionDeleted(bridgeID string)
}

type Store struct {
	db       *sql.DB // writer pool, pinned to 1 connection
	dbRO     *sql.DB // reader pool, concurrent readers under WAL
	notifier Notifier
}

// SetNotifier registers a callback fired after successful session-row
// mutations. Pass nil to clear. Replays the new state on UpsertDiscovered too.
func (s *Store) SetNotifier(n Notifier) { s.notifier = n }

func (s *Store) notifyChanged(bridgeID string) {
	if s.notifier != nil && bridgeID != "" {
		s.notifier.OnSessionChanged(bridgeID)
	}
}

func (s *Store) notifyDeleted(bridgeID string) {
	if s.notifier != nil && bridgeID != "" {
		s.notifier.OnSessionDeleted(bridgeID)
	}
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

	// Single connection serializes writes through Go's sql pool. Without this,
	// modernc.org/sqlite still hits SQLITE_BUSY (5) under concurrent writers
	// from multiple harness streams + /send handlers, despite WAL+busy_timeout.
	d.SetMaxOpenConns(1)

	// Reader pool — separate sql.DB so reads don't queue behind the single
	// writer connection. WAL allows many concurrent readers alongside one
	// writer; the SetMaxOpenConns(1) limit applies only to the writer.
	// busy_timeout via DSN so each connection in the pool gets it on open.
	dbRO, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("sqlite open ro: %w", err)
	}
	dbRO.SetMaxOpenConns(8)
	dbRO.SetMaxIdleConns(4)

	s := &Store{db: d, dbRO: dbRO}
	if err := s.migrate(); err != nil {
		d.Close()
		dbRO.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.dbRO != nil {
		s.dbRO.Close()
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			bridge_id          TEXT PRIMARY KEY,
			harness_session_id TEXT NOT NULL DEFAULT '',
			client_id          TEXT NOT NULL DEFAULT '',
			display_name       TEXT NOT NULL DEFAULT '',
			harness            TEXT NOT NULL,
			state              TEXT NOT NULL,
			pid                INTEGER NOT NULL DEFAULT 0,
			agent_id           TEXT NOT NULL DEFAULT '',
			spawner_id         TEXT NOT NULL DEFAULT '',
			parent_id          TEXT NOT NULL DEFAULT '',
			instance_id        TEXT NOT NULL DEFAULT '',
			created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
	// harness_session_id was introduced in this column under the older name
	// `harness_id`. The rename block below handles the rename for DBs that
	// already added the old column. Fresh DBs get harness_session_id from the
	// CREATE TABLE above. We still ADD COLUMN here (idempotent) so a DB that
	// pre-dates harness_id entirely still gets the column under the new name.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN harness_session_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN client_id TEXT NOT NULL DEFAULT ''")
	// Backfill: old rows have 'id' but no bridge_id — handled by the rename below.
	// If upgrading from old schema where PK was 'id', rename it to bridge_id.
	s.db.Exec("ALTER TABLE sessions RENAME COLUMN id TO bridge_id")
	s.db.Exec("ALTER TABLE sessions RENAME COLUMN client_request_id TO client_id")
	// session-chain rename: harness_id -> harness_session_id. Detect and run only
	// on DBs that still have the old column. Fresh DBs and already-migrated DBs
	// no-op cleanly. (RENAME COLUMN was added in SQLite 3.25.)
	if rows, err := s.db.Query("PRAGMA table_info(sessions)"); err == nil {
		hasOld := false
		hasNew := false
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				switch name {
				case "harness_id":
					hasOld = true
				case "harness_session_id":
					hasNew = true
				}
			}
		}
		rows.Close()
		if hasOld && !hasNew {
			s.db.Exec("ALTER TABLE sessions RENAME COLUMN harness_id TO harness_session_id")
			s.db.Exec("DROP INDEX IF EXISTS idx_sessions_harness_id")
		} else if hasOld && hasNew {
			// Both columns coexist (interrupted migration or extra ADD COLUMN
			// landed after rename). Coalesce into harness_session_id and drop
			// the duplicate. Use UPDATE+SELECT since SQLite has no merge.
			s.db.Exec("UPDATE sessions SET harness_session_id = harness_id WHERE COALESCE(harness_session_id, '') = '' AND COALESCE(harness_id, '') != ''")
			s.db.Exec("DROP INDEX IF EXISTS idx_sessions_harness_id")
			// SQLite supports DROP COLUMN since 3.35.
			s.db.Exec("ALTER TABLE sessions DROP COLUMN harness_id")
		}
	}
	s.db.Exec("ALTER TABLE sessions ADD COLUMN harness_config TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN info TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN folder_name TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT ''")
	// Auto-rename bookkeeping. autogenerated defaults to 1: every fresh session
	// is treated as auto-named until the user explicitly renames it via the
	// /rename endpoint. named_at_turn is the user_message count at which the
	// most recent auto-rename ran (0 = never). renamer_session_id holds the
	// bridge_id of an in-flight renamer session, used as a CAS lock.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN display_name_autogenerated INTEGER NOT NULL DEFAULT 1")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN display_name_named_at_turn INTEGER NOT NULL DEFAULT 0")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN renamer_session_id TEXT NOT NULL DEFAULT ''")
	// pty-mode child 2: per-session I/O mode. Empty / "events" = legacy
	// structured-events flow; "pty" = pseudoterminal attached over WS.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN mode TEXT NOT NULL DEFAULT ''")
	// Phase I sub-step 3 of the session-identity migration: add session_id
	// (mirror of bridge_id during the dual-write window) and session_type
	// (caller-declared category). bridge_id stays as the PK until the rename
	// in sub-step 7. session_type is empty on legacy rows; backfill on read
	// is unsafe (callers must declare), so older rows surface as "" type.
	// See llm-bridge MIGRATION-session-identity.md.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN session_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN session_type TEXT NOT NULL DEFAULT ''")
	// One-time backfill: populate session_id from bridge_id for any row that
	// pre-dates this migration. New inserts write both columns directly.
	s.db.Exec("UPDATE sessions SET session_id = bridge_id WHERE session_id = ''")
	// Index on harness_session_id must be created after ALTER TABLE migration adds/renames the column.
	// Drop the legacy non-unique index in favor of a partial UNIQUE one below.
	s.db.Exec("DROP INDEX IF EXISTS idx_sessions_harness_session_id")
	// Self-heal phantom rows from the StoredSession.ID-polymorphism bug:
	// the harness bridge had been stuffing bridge_session_id values into the
	// harness_session_id slot. Two phantom signatures, both invalid under
	// the post-fix contract:
	//   1. harness_session_id matches another row's bridge_id — same-table
	//      collision (rare; only when the original session still exists).
	//   2. harness_session_id has the `br_*` prefix — by definition a
	//      bridge_session_id, never a harness-native id (CC UUID, Codex
	//      thread_id, Hermes id).
	// Either way the row is a phantom; the canonical session lives elsewhere
	// (or has since been deleted). Clear them before adding the UNIQUE
	// constraint so the migration doesn't trip on legitimate pre-bug data.
	s.db.Exec("DELETE FROM sessions WHERE harness_session_id != '' AND (harness_session_id LIKE 'br_%' OR harness_session_id IN (SELECT bridge_id FROM sessions))")
	// Partial UNIQUE: harness_session_id is empty for fresh sessions before
	// the harness reports its first event, and we cannot allow those empty
	// strings to collide. Once populated it must be unique — the phantom-row
	// bug existed precisely because the column wasn't constrained.
	s.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_harness_session_id ON sessions(harness_session_id) WHERE harness_session_id != ''")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_folder ON sessions(folder_name)")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source)")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at)")

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

	// Source-folder mapping — runtime overrides for the env-var defaults
	// (config.SourceFolders). The effective mapping is the env defaults
	// merged with this table; a row here for "scheduler" wins over the
	// env's "scheduler:Scheduled" entry. Deleting a row falls back to the
	// env default. updated_at is wall-clock for audit.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS source_folders (
			source      TEXT PRIMARY KEY,
			folder_name TEXT NOT NULL,
			updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if sess.FolderName != "" {
		if _, err := tx.Exec(
			`INSERT INTO folders (name, position) VALUES (?, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
			sess.FolderName,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO sessions (bridge_id, session_id, harness_session_id, client_id, display_name, harness, instance_id, state, pid, agent_id, spawner_id, parent_id, harness_config, source, session_type, folder_name, mode, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.BridgeID, sess.BridgeID, sess.HarnessSessionID, sess.ClientID, sess.DisplayName, sess.Harness, sess.InstanceID, sess.State, sess.PID, sess.AgentID, sess.SpawnerID, sess.ParentID, harnessConfig, sess.Source, string(sess.SessionType), sess.FolderName, string(sess.Mode), sess.CreatedAt, sess.UpdatedAt,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyChanged(sess.BridgeID)
	return nil
}

const sessionColumns = `bridge_id, COALESCE(harness_session_id, ''), COALESCE(client_id, ''), display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, spawner_id, parent_id, COALESCE(harness_config, ''), COALESCE(info, ''), COALESCE(folder_name, ''), COALESCE(source, ''), COALESCE(session_type, ''), COALESCE(mode, ''), created_at, updated_at`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var harnessConfig string
	var info string
	var mode string
	var sessionType string
	err := sc.Scan(&sess.BridgeID, &sess.HarnessSessionID, &sess.ClientID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.SpawnerID, &sess.ParentID, &harnessConfig, &info, &sess.FolderName, &sess.Source, &sessionType, &mode, &sess.CreatedAt, &sess.UpdatedAt)
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
	if mode != "" {
		sess.Mode = msg.SessionMode(mode)
	}
	if sessionType != "" {
		sess.SessionType = msg.SessionType(sessionType)
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
	return scanSession(s.dbRO.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE bridge_id=?`, bridgeID,
	))
}

// GetSessionByHarnessSessionID looks up a session by its harness-reported session ID.
func (s *Store) GetSessionByHarnessSessionID(harnessSessionID string) (*Session, error) {
	return scanSession(s.dbRO.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE harness_session_id=?`, harnessSessionID,
	))
}

func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.dbRO.Query(`SELECT ` + sessionColumns + ` FROM sessions ORDER BY created_at DESC`)
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
	rows, err := s.dbRO.Query(`SELECT `+sessionColumns+` FROM sessions WHERE state=? ORDER BY created_at DESC`, state)
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
	s.notifyChanged(bridgeID)
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

// UpdateSessionHarnessConfig replaces the harness_config blob for a session.
// Pass an empty/nil cfg to clear it. Used by per-session settings endpoints
// (e.g. /sessions/{id}/bypass-permissions) so changes survive harness restart.
func (s *Store) UpdateSessionHarnessConfig(bridgeID string, cfg json.RawMessage) error {
	now := time.Now().UTC()
	var payload string
	if len(cfg) > 0 {
		payload = string(cfg)
	}
	res, err := s.db.Exec(`UPDATE sessions SET harness_config=?, updated_at=? WHERE bridge_id=?`, payload, now, bridgeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	s.notifyChanged(bridgeID)
	return nil
}

// PendingTurnMessage returns the text of the most recent user_message when
// no 'result' event follows it — the signal of a turn killed mid-flight that
// needs to be replayed. Returns ok=false when the last turn is balanced or
// when no user_message exists.
func (s *Store) PendingTurnMessage(bridgeID string) (string, bool, error) {
	var userID int
	var userData string
	err := s.db.QueryRow(
		`SELECT id, data FROM events WHERE session_id=? AND type='user_message' ORDER BY id DESC LIMIT 1`,
		bridgeID,
	).Scan(&userID, &userData)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var resultID int
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id=? AND type IN ('result','error')`,
		bridgeID,
	).Scan(&resultID); err != nil {
		return "", false, err
	}
	if resultID >= userID {
		return "", false, nil
	}
	var ev msg.Event
	if err := json.Unmarshal([]byte(userData), &ev); err != nil {
		return "", false, err
	}
	if ev.Result == nil {
		return "", false, nil
	}
	return ev.Result.Text, true, nil
}

// ReconcileSessions resets every session in any of the given states to 'idle'
// with pid=0 and returns the sessions that were reconciled. Called at startup:
// the harness subprocess can only exist in memory, so any row marked with an
// active state from a previous server lifetime is stale. updated_at is
// intentionally NOT bumped so FileInactive / ArchiveOld can still see the
// original last-state-transition time. Auto-resume uses LastActivityAt
// instead, which reflects real event flow rather than just state changes.
//
// Pass msg.ActiveSessionStates() to cover every state implying a live
// subprocess. The variadic shape lets tests target a subset.
func (s *Store) ReconcileSessions(states ...msg.SessionState) ([]Session, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	in := strings.Join(placeholders, ",")
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions WHERE state IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, *sess)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	if _, err := s.db.Exec(`UPDATE sessions SET state='idle', pid=0 WHERE state IN (`+in+`)`, args...); err != nil {
		return nil, err
	}
	return sessions, nil
}

// LastActivityAt returns the timestamp of the most recent event logged for
// the session, or the zero time if the session has no events yet (which the
// auto-resume cutoff check correctly treats as "stale, skip"). Auto-resume
// uses this rather than sessions.updated_at because updated_at only bumps on
// state transitions and metadata writes — a long turn that emits tool_calls
// and stream chunks for many minutes without a state flip would otherwise
// look stale.
//
// We scan into a string and parse explicitly: COALESCE / aggregate columns
// in modernc.org/sqlite lose their declared DATETIME affinity and come back
// as raw strings, which won't scan into time.Time. events.created_at is
// always written by CURRENT_TIMESTAMP DEFAULT so the format is fixed.
func (s *Store) LastActivityAt(bridgeID string) (time.Time, error) {
	var raw sql.NullString
	if err := s.db.QueryRow(
		`SELECT MAX(created_at) FROM events WHERE session_id = ?`, bridgeID,
	).Scan(&raw); err != nil {
		return time.Time{}, err
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", raw.String, time.UTC)
}

// SetHarnessSessionID fills in the harness-reported session ID on a session.
func (s *Store) SetHarnessSessionID(bridgeID, harnessSessionID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE sessions SET harness_session_id=?, updated_at=? WHERE bridge_id=?`, harnessSessionID, now, bridgeID)
	return err
}

// SetDisplayNameIfEmpty sets display_name only when it is currently empty,
// and marks it autogenerated. Returns true if a row was updated.
func (s *Store) SetDisplayNameIfEmpty(bridgeID, displayName string) (bool, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE sessions SET display_name=?, display_name_autogenerated=1, updated_at=? WHERE bridge_id=? AND display_name=''`,
		displayName, now, bridgeID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateSessionDisplayName sets the session's display_name unconditionally and
// marks it user-set (autogenerated=0). Used by the public /rename endpoint.
func (s *Store) UpdateSessionDisplayName(bridgeID, displayName string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE sessions SET display_name=?, display_name_autogenerated=0, updated_at=? WHERE bridge_id=?`,
		displayName, now, bridgeID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	s.notifyChanged(bridgeID)
	return nil
}

// CountUserMessages returns the number of user_message events for a session.
// Used by the auto-renamer to decide whether enough turns have elapsed since
// the last auto-naming run to warrant another pass.
func (s *Store) CountUserMessages(bridgeID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE session_id=? AND type='user_message'`,
		bridgeID,
	).Scan(&n)
	return n, err
}

// RenamerState returns the auto-rename bookkeeping for a session: whether the
// current display name is autogenerated, the user_message count at which the
// last auto-rename ran (0 = never), and the bridge_id of any in-flight
// renamer session ("" = none).
func (s *Store) RenamerState(bridgeID string) (autogenerated bool, namedAtTurn int, renamerSessionID string, err error) {
	var autogen int
	err = s.db.QueryRow(
		`SELECT display_name_autogenerated, display_name_named_at_turn, renamer_session_id FROM sessions WHERE bridge_id=?`,
		bridgeID,
	).Scan(&autogen, &namedAtTurn, &renamerSessionID)
	if err != nil {
		return false, 0, "", err
	}
	return autogen != 0, namedAtTurn, renamerSessionID, nil
}

// ReserveRenamerSlot atomically claims the renamer slot on a session. Returns
// true on success — caller now owns the slot and must clear it (via
// ApplyAutoRename or ClearRenamerSlot) when the renamer terminates. Returns
// false if another renamer is already in flight.
func (s *Store) ReserveRenamerSlot(bridgeID, renamerSessionID string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET renamer_session_id=? WHERE bridge_id=? AND renamer_session_id=''`,
		renamerSessionID, bridgeID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearRenamerSlot drops the renamer reservation without changing the display
// name. Used when the renamer fails to start or aborts before producing a name.
func (s *Store) ClearRenamerSlot(bridgeID string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET renamer_session_id='' WHERE bridge_id=?`,
		bridgeID,
	)
	return err
}

// ApplyAutoRename verifies that renamerSessionID still owns the slot, then
// updates display_name (keeping autogenerated=1), stamps named_at_turn, and
// clears the slot. Returns sql.ErrNoRows when the renamer no longer owns the
// slot — typically because the user manually renamed mid-flight.
func (s *Store) ApplyAutoRename(bridgeID, renamerSessionID, displayName string, namedAtTurn int) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE sessions
		   SET display_name=?, display_name_autogenerated=1,
		       display_name_named_at_turn=?, renamer_session_id='',
		       updated_at=?
		 WHERE bridge_id=? AND renamer_session_id=?`,
		displayName, namedAtTurn, now, bridgeID, renamerSessionID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	s.notifyChanged(bridgeID)
	return nil
}

// TurnText is one user/assistant exchange extracted from the events stream.
// Either field may be empty if the corresponding event is missing.
type TurnText struct {
	User      string
	Assistant string
}

// RecentTurnTexts returns up to limit recent turns for a session, oldest first.
// Walks user_message + result events in chronological order and pairs each user
// message with the next result. Truncates each text field at 2000 runes so the
// renamer prompt stays compact even for long turns.
func (s *Store) RecentTurnTexts(bridgeID string, limit int) ([]TurnText, error) {
	rows, err := s.db.Query(
		`SELECT type, data FROM events
		 WHERE session_id=? AND type IN ('user_message','result')
		 ORDER BY id ASC`,
		bridgeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var turns []TurnText
	var pending *TurnText
	for rows.Next() {
		var typ, data string
		if err := rows.Scan(&typ, &data); err != nil {
			return nil, err
		}
		var ev msg.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		var text string
		if ev.Result != nil {
			text = ev.Result.Text
		}
		text = truncateRunes(text, 2000)
		if typ == "user_message" {
			if pending != nil {
				turns = append(turns, *pending)
			}
			pending = &TurnText{User: text}
			continue
		}
		// result
		if pending == nil {
			pending = &TurnText{Assistant: text}
		} else {
			pending.Assistant = text
		}
		turns = append(turns, *pending)
		pending = nil
	}
	if pending != nil {
		turns = append(turns, *pending)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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
	_ = s.dbRO.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id=? AND type='user_message'`,
		sessionID,
	).Scan(&lastUserID)

	rows, err := s.dbRO.Query(
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
	rows, err := s.dbRO.Query(
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

// ListToolCallInputs returns the raw `input` JSON for every tool_call event
// in the session, ordered oldest first. Used by the git endpoint to discover
// which file paths (and therefore which repos) the session has touched.
// Empty/null inputs are skipped.
func (s *Store) ListToolCallInputs(sessionID string) ([]json.RawMessage, error) {
	rows, err := s.db.Query(
		`SELECT json_extract(data, '$.tool_call.input') FROM events
		 WHERE session_id=? AND type='tool_call' ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		out = append(out, json.RawMessage(raw.String))
	}
	return out, rows.Err()
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

// InFlightTurnState captures the per-session turn-level state that the harness
// manager keeps in memory for stamping events. Returned by RecoverInFlightTurn
// when a session's process restarts mid-turn so the manager can resume
// stamping the same TurnID/MessageID instead of leaving subsequent events
// unstamped.
type InFlightTurnState struct {
	TurnID           string
	ClientRequestID  string
	BridgeMessageID  string
	HarnessMessageID string
}

// RecoverInFlightTurn inspects the events table to recover turn-level state
// for a session whose process is restarting. A turn is "in flight" when the
// most recent user_message has no result/error event after it. When in flight,
// the returned state carries the user_message's turn_id (and client_request_id),
// plus the most recent assistant bubble's bridge/harness message ids — so
// post-restart events for the same turn get stamped with the original ids.
//
// Returns (nil, nil) when no user_message has been recorded yet, or when the
// most recent turn already has a terminator. Errors propagate.
func (s *Store) RecoverInFlightTurn(sessionID string) (*InFlightTurnState, error) {
	var lastUserID int
	var lastUserData string
	err := s.db.QueryRow(
		`SELECT id, data FROM events
		 WHERE session_id=? AND type='user_message'
		 ORDER BY id DESC LIMIT 1`,
		sessionID,
	).Scan(&lastUserID, &lastUserData)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var terminatorID int
	err = s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events
		 WHERE session_id=? AND id > ? AND type IN ('result','error')`,
		sessionID, lastUserID,
	).Scan(&terminatorID)
	if err != nil {
		return nil, err
	}
	if terminatorID > 0 {
		return nil, nil
	}

	var userEv msg.Event
	if err := json.Unmarshal([]byte(lastUserData), &userEv); err != nil {
		return nil, fmt.Errorf("recover turn: unmarshal user_message: %w", err)
	}
	if userEv.TurnID == "" {
		return nil, nil
	}

	st := &InFlightTurnState{
		TurnID:          userEv.TurnID,
		ClientRequestID: userEv.ClientRequestID,
	}

	// Most recent assistant-side bubble in the in-flight turn — used so a
	// post-restart event arriving without a fresh harness id falls back into
	// the still-open bubble rather than minting a new one.
	rows, err := s.db.Query(
		`SELECT message_id, harness_message_id FROM events
		 WHERE session_id=? AND id > ?
		   AND type IN ('block','stream','thinking','tool_call','tool_result','plan','approval','result')
		   AND message_id != ''
		 ORDER BY id DESC LIMIT 1`,
		sessionID, lastUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&st.BridgeMessageID, &st.HarnessMessageID); err != nil {
			return nil, err
		}
	}
	return st, rows.Err()
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
	s.notifyDeleted(bridgeID)
	return nil
}

// ── Folder management ─────────────────────────────────────────────────────────

// ListFolders returns all folder names ordered by their stored position.
func (s *Store) ListFolders() ([]string, error) {
	rows, err := s.dbRO.Query(`SELECT name FROM folders ORDER BY position ASC, name ASC`)
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

// ArchiveFolder is the canonical destination folder for [Store.ArchiveOld].
const ArchiveFolder = "Archive"

// FileInactive moves every unfiled session whose updated_at is older than
// cutoff into folder. Returns the bridge IDs that were moved. Auto-creates
// the folder in the registry if it doesn't exist. A session with a non-empty
// folder_name is left alone even if it is older than cutoff — explicit user
// filing wins over automatic housekeeping.
func (s *Store) FileInactive(cutoff time.Time, folder string) ([]string, error) {
	if folder == "" {
		return nil, fmt.Errorf("folder is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO folders (name, position) VALUES (?, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
		folder,
	); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT bridge_id FROM sessions WHERE folder_name='' AND updated_at < ?`,
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Intentionally do NOT bump updated_at — filing is housekeeping, not
	// activity. Bumping it would make a freshly filed session look active
	// and defeat the next sweep's cutoff.
	if _, err := tx.Exec(
		`UPDATE sessions SET folder_name=? WHERE folder_name='' AND updated_at < ?`,
		folder, cutoff,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ArchiveOld moves every session whose updated_at is older than cutoff into
// the Archive folder, regardless of the session's current folder assignment.
// Running sessions and sessions already in the Archive folder are left alone.
// Returns the bridge IDs that were moved. Auto-creates the Archive folder in
// the registry if it doesn't exist.
func (s *Store) ArchiveOld(cutoff time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO folders (name, position) VALUES (?, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
		ArchiveFolder,
	); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT bridge_id FROM sessions WHERE folder_name != ? AND state != 'running' AND updated_at < ?`,
		ArchiveFolder, cutoff,
	)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Intentionally do NOT bump updated_at — archiving is housekeeping, not
	// activity. Bumping it would make a freshly archived session look active
	// and defeat the next sweep's cutoff.
	if _, err := tx.Exec(
		`UPDATE sessions SET folder_name=? WHERE folder_name != ? AND state != 'running' AND updated_at < ?`,
		ArchiveFolder, ArchiveFolder, cutoff,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
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
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyChanged(bridgeID)
	return nil
}

// UpsertDiscoveredSession inserts a discovered session if it doesn't already exist.
//
// harnessSessionID is the harness-native session ID (e.g. CC UUID, Codex thread_id,
// Hermes session id) — the dedupe key. Must NOT carry a `br_*` bridge_id; that
// indicates a contract violation in the calling harness bridge and is rejected
// loudly.
//
// bridgeSessionID is the chain head reported by the harness bridge when its own
// state.db has already adopted this session into a bridge_session_id chain. When
// non-empty AND we already have a session row for that bridge_id, this is a
// no-op — bridge-server already knows the session. Cold-imported synthetic
// chains where bridge_session_id == harness_session_id won't match (no `br_*`
// row exists) and fall through to the harness_session_id dedupe path.
//
// instanceID is the instance that discovered this session.
// source/folderName tag the session for sidebar grouping (see config.SourceFolders).
// Returns the canonical bridge_id (newly generated or existing) and whether a new row was inserted.
func (s *Store) UpsertDiscoveredSession(harnessSessionID, bridgeSessionID, displayName, harness, instanceID, source, folderName string, createdAt, updatedAt time.Time) (string, bool, error) {
	if strings.HasPrefix(harnessSessionID, "br_") {
		return "", false, fmt.Errorf("UpsertDiscoveredSession: harness_session_id %q has bridge_id prefix — harness bridge is emitting a bridge_session_id in the harness slot (contract violation)", harnessSessionID)
	}

	// Short-circuit: if the harness bridge reports a bridge_session_id that
	// bridge-server already owns, the session is already adopted; don't
	// re-discover it.
	if bridgeSessionID != "" {
		var bridgeID string
		err := s.db.QueryRow(`SELECT bridge_id FROM sessions WHERE bridge_id=?`, bridgeSessionID).Scan(&bridgeID)
		if err == nil {
			return bridgeID, false, nil
		}
		if err != sql.ErrNoRows {
			return "", false, err
		}
	}

	// Check if session already exists by harness_session_id
	var existingBridgeID, existingInstanceID, existingDisplayName, existingSource, existingFolder string
	err := s.db.QueryRow(`SELECT bridge_id, COALESCE(instance_id, ''), COALESCE(display_name, ''), COALESCE(source, ''), COALESCE(folder_name, '') FROM sessions WHERE harness_session_id=?`, harnessSessionID).Scan(&existingBridgeID, &existingInstanceID, &existingDisplayName, &existingSource, &existingFolder)
	if err == nil {
		// Already exists - update timestamp, display_name, instance_id, source,
		// and folder where the existing values are empty. Existing non-empty
		// values win (user may have moved the session manually).
		newInstanceID := existingInstanceID
		if existingInstanceID == "" && instanceID != "" {
			newInstanceID = instanceID
		}
		newDisplayName := existingDisplayName
		if displayName != "" && (existingDisplayName == "" || (strings.HasPrefix(existingDisplayName, "/") && !strings.HasPrefix(displayName, "/"))) {
			newDisplayName = displayName
		}
		newSource := existingSource
		if existingSource == "" && source != "" {
			newSource = source
		}
		newFolder := existingFolder
		if existingFolder == "" && folderName != "" {
			newFolder = folderName
		}
		s.db.Exec(`UPDATE sessions SET updated_at=?, instance_id=?, display_name=?, source=?, folder_name=? WHERE bridge_id=?`, updatedAt, newInstanceID, newDisplayName, newSource, newFolder, existingBridgeID)
		s.notifyChanged(existingBridgeID)
		return existingBridgeID, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}

	// Insert new discovered session with state "idle". session_id mirrors
	// bridge_id during the dual-write window; session_type is empty for
	// discovery-imported sessions (no caller declared one).
	bridgeID := fmt.Sprintf("br_%d", time.Now().UnixNano())
	_, err = s.db.Exec(
		`INSERT INTO sessions (bridge_id, session_id, harness_session_id, client_id, display_name, harness, instance_id, state, pid, agent_id, spawner_id, parent_id, source, session_type, folder_name, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		bridgeID, bridgeID, harnessSessionID, "", displayName, harness, instanceID, "idle", 0, "", "", "", source, "", folderName, createdAt, updatedAt,
	)
	if err != nil {
		return "", false, err
	}
	s.notifyChanged(bridgeID)
	return bridgeID, true, nil
}

// ListSourceFolders returns every runtime override row, keyed by source.
// The caller is responsible for merging these on top of env-var defaults.
func (s *Store) ListSourceFolders() (map[string]string, error) {
	rows, err := s.dbRO.Query(`SELECT source, folder_name FROM source_folders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var src, folder string
		if err := rows.Scan(&src, &folder); err != nil {
			return nil, err
		}
		out[src] = folder
	}
	return out, rows.Err()
}

// SourceFolderTimestamps returns updated_at for each row, keyed by source.
func (s *Store) SourceFolderTimestamps() (map[string]time.Time, error) {
	rows, err := s.dbRO.Query(`SELECT source, updated_at FROM source_folders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var src string
		var ts time.Time
		if err := rows.Scan(&src, &ts); err != nil {
			return nil, err
		}
		out[src] = ts
	}
	return out, rows.Err()
}

// UpsertSourceFolder writes (or replaces) a runtime override.
func (s *Store) UpsertSourceFolder(source, folderName string) error {
	_, err := s.db.Exec(
		`INSERT INTO source_folders (source, folder_name, updated_at) VALUES (?,?,?)
		 ON CONFLICT(source) DO UPDATE SET folder_name=excluded.folder_name, updated_at=excluded.updated_at`,
		source, folderName, time.Now().UTC(),
	)
	return err
}

// DeleteSourceFolder removes a runtime override; the env default (if any)
// becomes effective again for this source.
func (s *Store) DeleteSourceFolder(source string) error {
	_, err := s.db.Exec(`DELETE FROM source_folders WHERE source=?`, source)
	return err
}

// ApplySourceFolder rebuckets sessions tagged with `source` whose folder_name
// is currently empty or equal to oldFolder, setting it to newFolder. Manual
// moves to any other folder are preserved. Returns the number of rows updated.
func (s *Store) ApplySourceFolder(source, oldFolder, newFolder string) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET folder_name=?, updated_at=? WHERE source=? AND (folder_name='' OR folder_name=?)`,
		newFolder, time.Now().UTC(), source, oldFolder,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
