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

	// OnSignalsChanged fires when a session's signals move: raised, answered,
	// dismissed or superseded. Fired from the store's write paths rather than
	// from the handlers, so a caller cannot forget — a question can be closed
	// by a turn ending, a message, a resolve verb, a supersede or a park
	// draining, and every one of those has to reach the surfaces showing it.
	OnSignalsChanged(bridgeID string)
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

func (s *Store) notifySignalsChanged(bridgeID string) {
	if s.notifier != nil && bridgeID != "" {
		s.notifier.OnSignalsChanged(bridgeID)
	}
}

func (s *Store) notifyDeleted(bridgeID string) {
	if s.notifier != nil && bridgeID != "" {
		s.notifier.OnSessionDeleted(bridgeID)
	}
}

// busyTimeoutMillisecondsWanted is how long a connection waits for a lock
// before giving up. Named so the DSN that sets it and the check that proves it
// took effect cannot drift apart.
const busyTimeoutMillisecondsWanted = 5000

// dataSourceName builds the sqlite DSN for a database path.
//
// busy_timeout MUST be set here rather than with a PRAGMA statement after
// Open: it is a per-connection setting, and *sql.DB is a pool. A one-shot
// db.Exec("PRAGMA busy_timeout=5000") reaches only whichever connection the
// pool happened to hand out, so every other connection keeps SQLite's default
// of 0 and fails instantly on a lock instead of waiting.
//
// _pragma=... is modernc's syntax and it is the only one that works here. The
// reader pool used to be opened with the mattn/go-sqlite3 spelling,
// "?_busy_timeout=5000", directly above a comment saying each connection got
// the setting on open. This driver ignores an unrecognised DSN key without
// complaint, so that DSN opened cleanly and configured nothing: measured,
// 0 of 8 pooled reader connections had a busy_timeout above 0. That is why
// both pools now verify the value after opening rather than trusting the
// connection string.
func dataSourceName(dbPath string) string {
	return fmt.Sprintf("%s?_pragma=busy_timeout(%d)", dbPath, busyTimeoutMillisecondsWanted)
}

// verifyBusyTimeoutTookEffect proves the DSN was understood by the driver.
func verifyBusyTimeoutTookEffect(db *sql.DB) error {
	var busyTimeoutMilliseconds int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMilliseconds); err != nil {
		return fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if busyTimeoutMilliseconds != busyTimeoutMillisecondsWanted {
		return fmt.Errorf("busy_timeout is %d, want %d: the DSN did not take effect",
			busyTimeoutMilliseconds, busyTimeoutMillisecondsWanted)
	}
	return nil
}

func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	d, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		return nil, err
	}

	// journal_mode is the one pragma that belongs here rather than in the DSN:
	// it is a property of the database file, so setting it once is permanent.
	// busy_timeout is per-connection and lives in dataSourceName.
	if _, err := d.Exec("PRAGMA journal_mode=WAL"); err != nil {
		d.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if err := verifyBusyTimeoutTookEffect(d); err != nil {
		d.Close()
		return nil, fmt.Errorf("writer pool: %w", err)
	}

	// Single connection serializes writes through Go's sql pool. Without this,
	// modernc.org/sqlite still hits SQLITE_BUSY (5) under concurrent writers
	// from multiple harness streams + /send handlers, despite WAL+busy_timeout.
	d.SetMaxOpenConns(1)

	// Reader pool — separate sql.DB so reads don't queue behind the single
	// writer connection. WAL allows many concurrent readers alongside one
	// writer; the SetMaxOpenConns(1) limit applies only to the writer.
	// busy_timeout via DSN so each connection in the pool gets it on open.
	dbRO, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("sqlite open ro: %w", err)
	}
	dbRO.SetMaxOpenConns(8)
	dbRO.SetMaxIdleConns(4)
	if err := verifyBusyTimeoutTookEffect(dbRO); err != nil {
		d.Close()
		dbRO.Close()
		return nil, fmt.Errorf("reader pool: %w", err)
	}

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
			display_name       TEXT NOT NULL DEFAULT '',
			harness            TEXT NOT NULL,
			state              TEXT NOT NULL,
			pid                INTEGER NOT NULL DEFAULT 0,
			agent_id           TEXT NOT NULL DEFAULT '',
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
	// Session lineage (TEAM-ORCHESTRATION.md §21). Idempotent ADD COLUMNs.
	// forked_from_session_id is the honest replacement for parent_id: it holds the
	// FORK PARENT'S bridge_id, where parent_id holds the parent's harness UUID (fed
	// to --fork). parent_id stays until the fork plumbing resolves the harness id
	// from the parent row; then it is dropped.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN forked_from_session_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN manager_session_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN root_session_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN depth INTEGER NOT NULL DEFAULT 0")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN controlled_by TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN refreshed_from_session_id TEXT NOT NULL DEFAULT ''")
	// Session-level working directory — the top of the working-directory
	// cascade (session > instance > machine default). Empty means "inherit
	// the instance", which is what every existing row means.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN working_dir TEXT NOT NULL DEFAULT ''")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_manager ON sessions(manager_session_id) WHERE manager_session_id != ''")
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
	// Phase I sub-step 6 of the session-identity migration: client_id is
	// retired. DROP runs AFTER the legacy renames above so any DB whose
	// migration ever resurrects client_id (via the client_request_id rename)
	// still ends up without it. Idempotent: errors swallowed on already-
	// migrated DBs that no longer have the column.
	// See llm-bridge MIGRATION-session-identity.md.
	s.db.Exec("ALTER TABLE sessions DROP COLUMN client_id")
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
	// Legacy: older DBs added a `source` column under that name; the
	// rename to `purpose` happens later in this migrate() block. Keep
	// the additive ADD here so DBs that pre-date `source` still gain
	// the column before the rename.
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
	// Source/Purpose/Origin reshape (2026-05-09): rename the overloaded
	// `source` column to `purpose` (the function/category of the session),
	// rename `session_type` to `type` (drop the redundant session_ prefix —
	// the field already lives on the sessions row), add `origin` for the
	// service that POST'd /sessions. Backfill origin from purpose since
	// for most sessions the originating service IS the function (autoworker
	// spawns autoworker work). Index renames track the column rename.
	// See llm-bridge MIGRATION-session-identity.md.
	s.db.Exec("ALTER TABLE sessions RENAME COLUMN source TO purpose")
	s.db.Exec("ALTER TABLE sessions RENAME COLUMN session_type TO type")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN origin TEXT NOT NULL DEFAULT ''")
	s.db.Exec("UPDATE sessions SET origin = purpose WHERE origin = '' AND purpose != ''")
	s.db.Exec("DROP INDEX IF EXISTS idx_sessions_source")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_purpose ON sessions(purpose)")
	// Backfill type for legacy rows pre-dating sub-step 3 (when the column
	// was added). Idempotent: WHERE type = '' guards against overwriting
	// caller-declared values. The mapping mirrors the well-known purpose
	// values documented on msg.SessionType. Unknown/new purposes left
	// empty — graceful failure beats silent misclassification.
	s.db.Exec(`UPDATE sessions SET type = 'autonomous'
		WHERE type = '' AND purpose IN
		('autoworker','dispatcher','kanban-dispatcher','scheduler','scheduled-task','harness-watch')`)
	s.db.Exec(`UPDATE sessions SET type = 'system'
		WHERE type = '' AND purpose IN
		('renamer','conformance','subagent','workflow-subagent','classifier','scoper')`)
	// 'healthcheck' used to be in the list above, which typed its remediation
	// agents as system. They are not: healthcheck spawns them to go clean up a
	// full disk with nobody watching, which is what autonomous means. The
	// caller was sending no type at all when that guess was made, and now
	// sends 'autonomous' explicitly, so the guess loses to the declaration.
	s.db.Exec(`UPDATE sessions SET type = 'autonomous'
		WHERE purpose = 'healthcheck' AND type = 'system'`)
	s.db.Exec(`UPDATE sessions SET type = 'interactive'
		WHERE type = '' AND purpose = 'chat'`)
	// The rule above used to also match purpose = '', which made it a standing
	// mislabeller rather than a backfill.
	//
	// migrate() runs on every open, not once, so every session that arrived
	// with no type got relabelled "interactive" at the next restart. Discovery
	// imports arrive exactly that way — a `claude -p` one-shot from a shell
	// became a human chat on the next deploy, indistinguishable in the UI from
	// a session someone actually opened. The same rule had already done this to
	// 226 workflow subagents, which is what the repair below cleans up; that
	// repair fixed the rows and left the rule that produced them.
	//
	// Empty purpose is now what it looks like: unclassified. Nothing infers a
	// type from it, discovery declares its own (external), and
	// session-taxonomy-guard reports whatever is left rather than papering
	// over it.

	// Repair the workflow subagents this backfill itself mislabelled.
	//
	// 'workflow-subagent' was missing from the system list above, so every
	// Workflow-tool subagent fell through to the purpose='' rule and was typed
	// INTERACTIVE — a fire-and-forget agent recorded as a human chat, and shown
	// as one. 226 sessions on this host, against 734 correctly-typed Task()
	// subagents; the only difference between them is which tool spawned them.
	//
	// Unlike the backfills above, this one overwrites a NON-empty type, which
	// they deliberately refuse to do. That is safe here precisely because it is
	// keyed on purpose: a workflow subagent is never interactive by
	// construction — nobody is at a keyboard — so 'interactive' on such a row is
	// not a caller's declaration to be preserved, it is this bug's fingerprint.
	s.db.Exec(`UPDATE sessions SET type = 'system'
		WHERE purpose = 'workflow-subagent' AND type = 'interactive'`)
	// Backfill origin for legacy rows where it was lazily defaulted to the
	// purpose value. Origin is meant to be the SERVICE that hosts the
	// spawning code, not the binary or the kind of session. WHERE origin =
	// purpose AND purpose IN (...) limits the rewrite to the lazy-defaulted
	// rows; explicit caller-supplied origins (e.g. healthcheck-set
	// origin=healthcheck) are left alone.
	s.db.Exec(`UPDATE sessions SET origin = 'scheduler' WHERE origin = purpose AND purpose IN
		('autoworker','dispatcher','kanban-dispatcher','scheduled-task','classifier','scoper')`)
	s.db.Exec(`UPDATE sessions SET origin = 'llm-bridge-server' WHERE origin = purpose AND purpose IN
		('renamer','conformance')`)
	s.db.Exec(`UPDATE sessions SET origin = 'inber' WHERE origin = purpose AND purpose = 'harness-watch'`)
	s.db.Exec(`UPDATE sessions SET origin = 'llm-bridge-claudecode' WHERE origin = purpose AND purpose = 'subagent'`)
	// Legacy interactive sessions: bridge-ui defaults origin='frontend' for
	// new ones, but pre-rename rows have empty origin. Backfill so the
	// frontend's chat sessions all attribute to "frontend" (dash/llmux split
	// is a separate followup if we want it).
	//
	// Keyed on purpose='chat' only. It used to also match purpose='', which
	// credited every unclassified session to the frontend on the same faulty
	// reasoning as the type rule above: absence of a declaration is not a
	// declaration of chat.
	s.db.Exec(`UPDATE sessions SET origin = 'frontend'
		WHERE origin = '' AND purpose = 'chat'`)

	// Repair the discovery imports that the purpose='' rules above mislabelled.
	//
	// Discovery wrote type='' and origin='llm-bridge-<harness>' for sessions it
	// found on disk; the type rule then read the empty type as interactive. The
	// result is a session that reports it was a human chat originating from a
	// harness adapter — two false statements about a `claude -p` one-shot that
	// the bridge never ran.
	//
	// Keyed on origin: only discovery ever wrote an origin of llm-bridge-<x>,
	// and a genuine subagent from the same adapter carries purpose='subagent',
	// so restricting to purpose='' selects exactly the cold imports. Like the
	// workflow-subagent repair above, this overwrites a non-empty type, and for
	// the same reason — 'interactive' here is the bug's fingerprint, not a
	// caller's declaration, because no caller ever spoke for these rows.
	s.db.Exec(`UPDATE sessions SET type = 'external', purpose = 'discovered', origin = 'discovery'
		WHERE purpose = '' AND origin LIKE 'llm-bridge-%' AND type IN ('', 'interactive')`)
	// Discovered subagents that predate the live promotion path: purpose was
	// recognised from the on-disk layout but type was left empty, and no rule
	// above covers purpose='subagent' with an empty type.
	s.db.Exec(`UPDATE sessions SET type = 'system'
		WHERE type = '' AND purpose IN ('subagent','workflow-subagent')`)

	// Legacy frontend chats created before the frontends sent a purpose at
	// all. The rows already say type='interactive' and origin='frontend', and
	// the frontends create nothing but chats, so 'chat' is the only purpose
	// these can have — this names what the row already says rather than
	// inferring something new from silence, which is the mistake the rules
	// above made.
	s.db.Exec(`UPDATE sessions SET purpose = 'chat'
		WHERE purpose = '' AND type = 'interactive' AND origin = 'frontend'`)
	// Spend ceiling and spend high-water mark. Both REAL, both defaulting to
	// 0, and 0 on max_budget_usd means NO CEILING — see the warning on
	// msg.ManagedSession.MaxBudgetUSD. A default of 0 is therefore also the
	// correct value for every pre-existing row: no session created before
	// this column existed had a ceiling, and the backfill must not invent
	// one for them.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN max_budget_usd REAL NOT NULL DEFAULT 0")
	s.db.Exec("ALTER TABLE sessions ADD COLUMN spend_usd REAL NOT NULL DEFAULT 0")
	// The rest of the cumulative api_spend_total aggregate — token usage,
	// call count, per-model and per-query-source dollars — as JSON. See
	// SessionSpendDetail for why the dollar total is not in here. Empty is
	// the correct value for every pre-existing row: the breakdown for spend
	// that predates this column was never persisted anywhere, and inventing
	// one would attribute dollars to a model nobody measured.
	s.db.Exec("ALTER TABLE sessions ADD COLUMN api_spend_detail TEXT NOT NULL DEFAULT ''")
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
	// idx_sessions_source removed: the source column was renamed to purpose;
	// the corresponding idx_sessions_purpose index is created in the
	// purpose/origin block above.
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
	if err := s.migrateSignals(); err != nil {
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
		`INSERT INTO sessions (bridge_id, session_id, harness_session_id, display_name, harness, instance_id, state, pid, agent_id, parent_id, forked_from_session_id, manager_session_id, root_session_id, depth, controlled_by, refreshed_from_session_id, working_dir, harness_config, purpose, type, origin, folder_name, mode, max_budget_usd, spend_usd, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.SessionID, sess.SessionID, sess.HarnessSessionID, sess.DisplayName, sess.Harness, sess.InstanceID, sess.State, sess.PID, sess.AgentID, sess.ParentID, sess.ForkedFromSessionID, sess.ManagerSessionID, sess.RootSessionID, sess.Depth, sess.ControlledBy, sess.RefreshedFromSessionID, sess.WorkingDir, harnessConfig, sess.Purpose, string(sess.Type), sess.Origin, sess.FolderName, string(sess.Mode), sess.MaxBudgetUSD, sess.SpendUSD, sess.CreatedAt, sess.UpdatedAt,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyChanged(sess.SessionID)
	return nil
}

// sessionColumns selects the fields scanSession reads, in the order it
// expects them. session_id is the canonical id (COALESCE falls back to
// bridge_id for legacy rows whose session_id may be unbackfilled).
const sessionColumns = `COALESCE(NULLIF(session_id, ''), bridge_id), COALESCE(harness_session_id, ''), display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, parent_id, COALESCE(forked_from_session_id, ''), COALESCE(manager_session_id, ''), COALESCE(root_session_id, ''), COALESCE(depth, 0), COALESCE(controlled_by, ''), COALESCE(refreshed_from_session_id, ''), COALESCE(working_dir, ''), COALESCE(harness_config, ''), COALESCE(info, ''), COALESCE(folder_name, ''), COALESCE(purpose, ''), COALESCE(type, ''), COALESCE(origin, ''), COALESCE(mode, ''), COALESCE(max_budget_usd, 0), COALESCE(spend_usd, 0), created_at, updated_at`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var harnessConfig string
	var info string
	var mode string
	var sessionType string
	err := sc.Scan(&sess.SessionID, &sess.HarnessSessionID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.ParentID, &sess.ForkedFromSessionID, &sess.ManagerSessionID, &sess.RootSessionID, &sess.Depth, &sess.ControlledBy, &sess.RefreshedFromSessionID, &sess.WorkingDir, &harnessConfig, &info, &sess.FolderName, &sess.Purpose, &sessionType, &sess.Origin, &mode, &sess.MaxBudgetUSD, &sess.SpendUSD, &sess.CreatedAt, &sess.UpdatedAt)
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
		sess.Type = msg.SessionType(sessionType)
	}
	// Backfill Origin = Purpose for legacy rows that pre-date the origin
	// column (UPDATE in migrate() handles new rows but lookups against
	// rows from a brief window may still see empty origin).
	if sess.Origin == "" && sess.Purpose != "" {
		sess.Origin = sess.Purpose
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
	return s.ListSessionsPaged(0, 0)
}

// ListSessionsPaged returns sessions ordered by created_at DESC. A limit <= 0
// means no bound — the full table, matching the historical unbounded behavior
// callers like the health snapshot and instance rollup still rely on. offset is
// applied only when limit > 0 (a bare offset with no limit is meaningless).
func (s *Store) ListSessionsPaged(limit, offset int) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions ORDER BY created_at DESC`
	var args []any
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.dbRO.Query(query, args...)
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
	return s.ListSessionsByStatePaged(state, 0, 0)
}

// ListSessionsByStatePaged is ListSessionsByState with the same limit/offset
// bounding semantics as ListSessionsPaged (limit <= 0 means unbounded).
func (s *Store) ListSessionsByStatePaged(state string, limit, offset int) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions WHERE state=? ORDER BY created_at DESC`
	args := []any{state}
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.dbRO.Query(query, args...)
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

// SetSessionMaxBudgetUSD writes the session's spend ceiling in US dollars.
// Zero clears the ceiling (see the warning on msg.ManagedSession.MaxBudgetUSD
// — zero is "unset", never "halt now"). Callers validate the sign; a
// negative ceiling would halt every session on its first API call, so it is
// rejected at the HTTP boundary rather than silently clamped here.
func (s *Store) SetSessionMaxBudgetUSD(bridgeID string, maxBudgetUSD float64) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET max_budget_usd=?, updated_at=? WHERE bridge_id=?`, maxBudgetUSD, now, bridgeID)
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

// SessionSpendDetail is every field of the cumulative api_spend_total
// aggregate except TotalUSD.
//
// TotalUSD is deliberately absent: it lives in the sessions.spend_usd REAL
// column because the spend gate compares it in SQL and a figure inside a
// JSON blob cannot be compared there. Splitting the aggregate this way
// keeps exactly one home for the dollar total instead of two that can
// disagree. Both halves are written by the same statement, so they cannot
// drift apart.
type SessionSpendDetail struct {
	Usage         msg.TokenUsage     `json:"usage"`
	Calls         int                `json:"calls"`
	ByModel       map[string]float64 `json:"by_model,omitempty"`
	ByQuerySource map[string]float64 `json:"by_query_source,omitempty"`
}

// RecordSessionSpend raises the session's recorded spend to spendUSD, stores
// the matching breakdown, and returns the dollar value now stored, which is
// never lower than what was there.
//
// The MAX() is a backstop, not the mechanism. The running total this is fed
// from is derived in bridge-server's memory, and the derivation that holds it
// is discarded when the harness process exits — so without the seeding in
// harness.persistedSpend the total restarts at zero on every resume and a
// plain assignment would walk a session's spend backwards, handing a budget
// that was already exhausted a fresh full allowance. Cumulative spend does not
// go down. The MAX() survives here for the case seeding cannot cover: a store
// read that fails at derivation-creation time starts the total at zero, and
// the recorded figure must not follow it down.
//
// The breakdown is written only when the total advances, for the same reason:
// a per-run breakdown from a derivation that lost its history describes less
// spending than the row already knows about, and overwriting with it would
// lose the earlier runs' attribution.
func (s *Store) RecordSessionSpend(bridgeID string, spendUSD float64, detail SessionSpendDetail) (float64, error) {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return 0, fmt.Errorf("marshal spend detail for %s: %w", bridgeID, err)
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions
		SET api_spend_detail = CASE WHEN ? > spend_usd THEN ? ELSE api_spend_detail END,
		    spend_usd        = MAX(spend_usd, ?),
		    updated_at       = ?
		WHERE bridge_id = ?`, spendUSD, string(encoded), spendUSD, now, bridgeID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	var stored float64
	if err := s.dbRO.QueryRow(`SELECT COALESCE(spend_usd, 0) FROM sessions WHERE bridge_id=?`, bridgeID).Scan(&stored); err != nil {
		return 0, err
	}
	s.notifyChanged(bridgeID)
	return stored, nil
}

// SessionSpend reads back the cumulative spend a session has already been
// recorded as making: the dollar total and the breakdown that produced it.
//
// This is what a freshly created derivation is seeded from, so that the
// api_spend_total it goes on to emit continues the session's history instead
// of starting a second one. A session with no row, or one that has never
// spent, reads as zero — the correct starting point for both.
func (s *Store) SessionSpend(bridgeID string) (float64, SessionSpendDetail, error) {
	var (
		totalUSD float64
		encoded  string
		detail   SessionSpendDetail
	)
	err := s.dbRO.QueryRow(`SELECT COALESCE(spend_usd, 0), COALESCE(api_spend_detail, '')
		FROM sessions WHERE bridge_id=?`, bridgeID).Scan(&totalUSD, &encoded)
	if err != nil {
		return 0, detail, err
	}
	if encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &detail); err != nil {
			return 0, SessionSpendDetail{}, fmt.Errorf("unmarshal spend detail for %s: %w", bridgeID, err)
		}
	}
	return totalUSD, detail, nil
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

// UpdateSessionMode flips the I/O mode of a session. Used by the mode-
// switch endpoint to flip an existing session between events and pty
// modes; the caller is responsible for stopping the current process and
// starting a fresh one in the new mode (this method only changes the
// stored bit).
func (s *Store) UpdateSessionMode(bridgeID string, mode msg.SessionMode) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET mode=?, updated_at=? WHERE bridge_id=?`, string(mode), now, bridgeID)
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

// InterruptedTurn describes a turn whose user_message never got a terminator:
// the harness process died before the turn finished. Auto-resume picks such a
// turn back up, and how it does that depends on how far the turn had got, so
// the count of work already done is part of the answer rather than something
// the caller has to go and derive.
type InterruptedTurn struct {
	// UserMessageText is the instruction the turn was given.
	UserMessageText string

	// ToolCallsAlreadyRun counts the tool_call events logged after that
	// user_message. Zero means the turn died before it touched anything, so
	// re-sending the instruction repeats nothing. Above zero the turn has
	// already changed state outside this process — files written, services
	// restarted, requests sent — and re-sending the instruction asks for that
	// work a second time. The two cases are indistinguishable without this
	// count, which is why the replay used to hand a half-finished deploy back
	// to the model as if the user had just asked for it.
	ToolCallsAlreadyRun int
}

// InterruptedTurn returns the most recent turn when no 'result' or 'error'
// event follows its user_message — the signal of a turn killed mid-flight.
// Returns nil when the last turn is balanced or when no user_message exists.
func (s *Store) InterruptedTurn(bridgeID string) (*InterruptedTurn, error) {
	var userID int
	var userData string
	err := s.db.QueryRow(
		`SELECT id, data FROM events WHERE session_id=? AND type='user_message' ORDER BY id DESC LIMIT 1`,
		bridgeID,
	).Scan(&userID, &userData)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resultID int
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id=? AND type IN ('result','error')`,
		bridgeID,
	).Scan(&resultID); err != nil {
		return nil, err
	}
	if resultID >= userID {
		return nil, nil
	}
	var ev msg.Event
	if err := json.Unmarshal([]byte(userData), &ev); err != nil {
		return nil, err
	}
	if ev.Result == nil {
		return nil, nil
	}
	var toolCalls int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE session_id=? AND type='tool_call' AND id > ?`,
		bridgeID, userID,
	).Scan(&toolCalls); err != nil {
		return nil, err
	}
	return &InterruptedTurn{
		UserMessageText:     ev.Result.Text,
		ToolCallsAlreadyRun: toolCalls,
	}, nil
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

// MaxEventRowID reports the highest event row id stored for a session, or 0 when it has
// none. This is the SSE stream's resume point: a client holding it can reconnect with
// `Last-Event-ID` and be sent only what arrived after, instead of having the whole
// current turn replayed at it.
//
// ⚠️ Read it BEFORE flushing pending writes to log-store, never after. The guarantee a
// caller needs is "every event at or below this id is already in the materialized page",
// and only that order provides it: the flush drains everything queued at the moment it
// runs, which necessarily includes every event that existed when the id was read. Read
// the id after the flush and an event written in between is at or below it while never
// having reached log-store — present in neither the page nor the resumed stream, and
// nothing anywhere reports the loss.
func (s *Store) MaxEventRowID(sessionID string) (int64, error) {
	var maxID int64
	err := s.dbRO.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id=?`,
		sessionID,
	).Scan(&maxID)
	return maxID, err
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

// EnsureSubagentSession returns the bridge session id for a harness-internal
// subagent, creating the row the first time that subagent is seen. It is the
// live counterpart of UpsertDiscoveredSession: the adapter demuxes a subagent's
// frames out of the parent process's stream while the run is still in flight,
// so the session exists before any on-disk rollout scan finds it
// (TEAM-ORCHESTRATION.md §21.4 step 4).
//
// harnessSessionID must be the SAME key the discovery scanner would derive for
// this subagent — for Claude Code, "agent-<task_id>", matching the
// `subagents/agent-<task_id>.jsonl` rollout filename. That is what makes the
// two paths converge on one row instead of minting a duplicate when discovery
// later walks the same subagent. It is required, not optional: an empty
// harness_session_id would collapse every subagent onto the same dedupe key
// (§21.6).
//
// The new row is linked to its parent through manager_session_id — the
// management tree — and marked controlled_by="harness", which is the flag every
// resume / message / kill caller gates on. That marking matters: the subagent's
// harness_session_id is an internal agent id, NOT a resumable Claude UUID, so
// anything that fed it to `claude --resume` would fail (§21.6).
func (s *Store) EnsureSubagentSession(parent *Session, harnessSessionID, displayName, folderName string) (string, bool, error) {
	if parent == nil || parent.SessionID == "" {
		return "", false, fmt.Errorf("EnsureSubagentSession: parent session is required")
	}
	if harnessSessionID == "" {
		return "", false, fmt.Errorf("EnsureSubagentSession: harness_session_id is required as the dedupe key")
	}
	if strings.HasPrefix(harnessSessionID, "br_") {
		return "", false, fmt.Errorf("EnsureSubagentSession: harness_session_id %q has bridge_id prefix — harness bridge is emitting a bridge_session_id in the harness slot (contract violation)", harnessSessionID)
	}

	existing, err := s.GetSessionByHarnessSessionID(harnessSessionID)
	if err == nil && existing != nil {
		return existing.SessionID, false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", false, err
	}

	// root_session_id denormalizes the top of the tree so "show me this whole
	// team" is one query instead of a walk up the manager chain. A parent that
	// is itself top-level has no root of its own and becomes the root here.
	root := parent.RootSessionID
	if root == "" {
		root = parent.SessionID
	}

	sub := &Session{
		SessionID:        fmt.Sprintf("br_%d", time.Now().UnixNano()),
		HarnessSessionID: harnessSessionID,
		DisplayName:      displayName,
		Harness:          parent.Harness,
		InstanceID:       parent.InstanceID,
		State:            string(msg.SessionRunning),
		ManagerSessionID: parent.SessionID,
		RootSessionID:    root,
		Depth:            parent.Depth + 1,
		ControlledBy:     msg.ControlledByHarness,
		Purpose:          "subagent",
		Type:             msg.SessionTypeSystem,
		Origin:           "llm-bridge-" + strings.ReplaceAll(string(parent.Harness), "_", ""),
		FolderName:       folderName,
	}
	if err := s.CreateSession(sub); err != nil {
		return "", false, err
	}
	return sub.SessionID, true, nil
}

// LinkDiscoveredSessionParent records the lineage of a session that discovery
// imported, resolving the harness-native parent id the adapter reported to the
// bridge session that actually holds it.
//
// This is the cold-import counterpart to the link EnsureSubagentSession writes
// live. It exists because the live path only ever sees subagents of processes
// bridge-server is currently hosting: a subagent whose run predates the demux,
// or whose parent ran outside the bridge, reaches the store only through
// discovery — and arrived with no parent at all. On this host that was every
// discovered subagent, 1,259 rows.
//
// parentHarnessSessionID is a HARNESS id (for Claude Code, the parent's session
// UUID, which it writes into the subagent's rollout path). It is resolved to a
// bridge_session_id here and stored as one. The harness id is never written
// into a lineage column: those hold bridge ids, and mixing the two would make
// the tree unwalkable. Nothing is matched by display name.
//
// Three things it deliberately does not do:
//
//   - It does not invent a parent. An unresolvable parent id — the parent has
//     not been imported yet, or never will be — leaves the row unlinked, and a
//     later discovery pass links it once the parent exists. Empty stays empty.
//   - It does not overwrite an existing manager_session_id. A row promoted live
//     already has the authoritative link; discovery converges onto that row and
//     must not disturb it.
//   - It does not link a session to itself, which a malformed or truncated path
//     could otherwise ask for.
//
// Returns whether it wrote a link, so a caller can report how many rows a
// discovery pass repaired.
func (s *Store) LinkDiscoveredSessionParent(bridgeID, parentHarnessSessionID string) (bool, error) {
	if bridgeID == "" || parentHarnessSessionID == "" {
		return false, nil
	}
	if strings.HasPrefix(parentHarnessSessionID, "br_") {
		return false, fmt.Errorf("LinkDiscoveredSessionParent: parent_harness_session_id %q has bridge_id prefix — the adapter is reporting a bridge id in the harness slot (contract violation)", parentHarnessSessionID)
	}

	parent, err := s.GetSessionByHarnessSessionID(parentHarnessSessionID)
	if err == sql.ErrNoRows || (err == nil && parent == nil) {
		// The parent is not (yet) known. Not an error: cold import has no
		// ordering guarantee, and the next pass will find it.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if parent.SessionID == bridgeID {
		return false, nil
	}

	root := parent.RootSessionID
	if root == "" {
		root = parent.SessionID
	}

	// The WHERE clause is what makes this idempotent and safe to re-run on
	// every discovery pass: it only fills a link that is absent.
	res, err := s.db.Exec(`
		UPDATE sessions
		   SET manager_session_id=?, root_session_id=?, depth=?
		 WHERE bridge_id=?
		   AND COALESCE(manager_session_id, '')=''`,
		parent.SessionID, root, parent.Depth+1, bridgeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		s.notifyChanged(bridgeID)
	}
	return n > 0, nil
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
	err := s.db.QueryRow(`SELECT bridge_id, COALESCE(instance_id, ''), COALESCE(display_name, ''), COALESCE(purpose, ''), COALESCE(folder_name, '') FROM sessions WHERE harness_session_id=?`, harnessSessionID).Scan(&existingBridgeID, &existingInstanceID, &existingDisplayName, &existingSource, &existingFolder)
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
		s.db.Exec(`UPDATE sessions SET updated_at=?, instance_id=?, display_name=?, purpose=?, folder_name=? WHERE bridge_id=?`, updatedAt, newInstanceID, newDisplayName, newSource, newFolder, existingBridgeID)
		s.notifyChanged(existingBridgeID)
		return existingBridgeID, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}

	// Insert new discovered session with state "idle". session_id mirrors
	// bridge_id during the dual-write window.
	//
	// parent_id is left empty because it is FORK plumbing — it holds a fork
	// source's harness UUID, and a discovered session was not forked. It is not
	// the "who spawned me" link; that is manager_session_id.
	//
	// This path writes no manager_session_id either, and for a cold-imported
	// top-level session there is genuinely nothing to write. For a harness
	// SUBAGENT there is: EnsureSubagentSession promotes it live, while the run
	// is in flight, and links it to its parent then. Discovery converges onto
	// that row through the harness_session_id dedupe key above, so a subagent
	// that was promoted live keeps its lineage. A subagent whose run predates
	// the live demux — or whose parent process bridge-server never hosted —
	// still lands here unlinked; for Claude Code the parent's UUID is
	// recoverable from the rollout path (<parent-uuid>/subagents/agent-*.jsonl),
	// which is what the adapter now reports as ParentHarnessSessionID.
	bridgeID := fmt.Sprintf("br_%d", time.Now().UnixNano())

	// Classify the import honestly.
	//
	// This used to write type="" and origin="llm-bridge-<harness>". Both were
	// wrong, and they compounded: the empty type was filled in later by a
	// startup backfill that read "no type" as "interactive", so a `claude -p`
	// smoke test run from a shell was recorded — and displayed — as a human
	// chat. The origin named the adapter that *found* the session, but origin
	// means the service that *created* it, and the adapter created nothing.
	//
	// A discovered session is external: it ran outside the bridge, no caller
	// declared anything about it, and the honest origin is "discovery".
	// Where the adapter or a prompt prefix did recognise a purpose — a
	// conformance probe, a subagent, a scheduled job — that is a real signal
	// rather than a guess, so its registered type wins instead.
	//
	// The type is adopted; the origin never is. Origin answers who created
	// the row, and that is discovery no matter what the session turned out
	// to be — which is the whole correction described above.
	//
	// This used to also adopt spec.Origins[0] whenever the registry listed
	// exactly one origin. That is the replaced bug, narrowed rather than
	// removed: a discovered workflow-subagent would have been stamped
	// "llm-bridge-claudecode" and a discovered autoworker "scheduler",
	// naming services that created nothing. Narrowing a wrong rule to the
	// case where its guess is unambiguous does not make the guess true, it
	// makes it rare and hard to see. A registry entry discovery can assign
	// lists OriginDiscovery among its origins instead.
	sessionType := msg.SessionTypeExternal
	origin := msg.OriginDiscovery
	if source == "" {
		source = msg.PurposeDiscovered
		if folderName == "" {
			folderName = msg.FolderForPurpose(msg.PurposeDiscovered)
		}
	} else if spec, ok := msg.LookupPurpose(source); ok {
		sessionType = spec.Type
	}

	_, err = s.db.Exec(
		`INSERT INTO sessions (bridge_id, session_id, harness_session_id, display_name, harness, instance_id, state, pid, agent_id, parent_id, purpose, type, origin, folder_name, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		bridgeID, bridgeID, harnessSessionID, displayName, harness, instanceID, "idle", 0, "", "", source, string(sessionType), origin, folderName, createdAt, updatedAt,
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
// The `source` parameter matches against the sessions.purpose column — the
// source_folders table and this API still use "source" naming (vestigial,
// see migrate()), but the session column was renamed to purpose in f03b058.
func (s *Store) ApplySourceFolder(source, oldFolder, newFolder string) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET folder_name=?, updated_at=? WHERE purpose=? AND (folder_name='' OR folder_name=?)`,
		newFolder, time.Now().UTC(), source, oldFolder,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
