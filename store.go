// Package permstore is the canonical rule engine for tool-call gating across
// the llm-bridge harness ecosystem. SQLite-backed: rules + audit log live in
// one database. The store is paired with engine.go (the cascade evaluator)
// and internal/server (the HTTP surface).
package permstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// --- Domain types -------------------------------------------------------

// Rule is one entry in the policy table. A rule matches when:
//
//   - tool == request.tool (or tool == "*")
//   - pattern is empty, or pattern (a regex) matches the atom's Line
//
// On match, outcome decides what evaluate returns for that atom. message is
// surfaced verbatim to the caller on deny / ask outcomes.
//
// Scope encodes where the rule applies:
//
//	"global"           — applies to all sessions
//	"bridge:<id>"      — applies only to the named bridge_session_id
//	"instance:<id>"    — applies only to the named harness instance
//
// Cascade order: instance → bridge → global. Within a scope, lower priority
// wins; within the same priority, alphabetical id wins (stable tiebreak).
type Rule struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Priority  int    `json:"priority"`
	Tool      string `json:"tool"`
	Pattern   string `json:"pattern,omitempty"`
	Outcome   string `json:"outcome"`
	Message   string `json:"message,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Outcome constants — kept as strings so they round-trip through JSON
// untouched.
const (
	OutcomeAllow = "allow"
	OutcomeDeny  = "deny"
	OutcomeAsk   = "ask"
)

// Scope prefixes.
const (
	ScopeGlobal      = "global"
	ScopeBridgePfx   = "bridge:"
	ScopeInstancePfx = "instance:"
)

// AuditEntry is one row in the audit log — every evaluate call writes one
// before returning. Kept forever (per locked design).
type AuditEntry struct {
	ID            string          `json:"id"`
	TS            string          `json:"ts"`
	BridgeID      string          `json:"bridge_id,omitempty"`
	InstanceID    string          `json:"instance_id,omitempty"`
	Tool          string          `json:"tool"`
	InputJSON     json.RawMessage `json:"input,omitempty"`
	AtomsJSON     json.RawMessage `json:"atoms,omitempty"`
	Outcome       string          `json:"outcome"`
	MatchedRuleID string          `json:"matched_rule_id,omitempty"`
	ResolvedBy    string          `json:"resolved_by,omitempty"`
}

// --- Store --------------------------------------------------------------

// Store wraps the SQLite handle and exposes typed accessors. Concurrency:
// modernc.org/sqlite + WAL still leaks SQLITE_BUSY under concurrent writers,
// so the connection pool is pinned to 1.
type Store struct {
	db   *sql.DB
	path string
}

// DefaultPath returns the canonical on-disk location for the database.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "permission-store", "permission-store.db")
}

// Open opens or creates the store at path (DefaultPath when empty), running
// migrations to head.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the on-disk database path.
func (s *Store) Path() string { return s.path }

// DB returns the underlying handle for tests / advanced callers.
func (s *Store) DB() *sql.DB { return s.db }

// --- Migrations ---------------------------------------------------------

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("schema_version table: %w", err)
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	migrations := []func() error{
		s.migrateV1,
		s.migrateV2,
	}
	for i := current; i < len(migrations); i++ {
		if err := migrations[i](); err != nil {
			return fmt.Errorf("migration v%d: %w", i+1, err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			return fmt.Errorf("record schema_version v%d: %w", i+1, err)
		}
	}
	return nil
}

// migrateV2 inserts the default seed rules. Runs once per database (once
// schema_version records v2, this never fires again — user-deleted seeds
// stay deleted, edits stay edited). Use the future /seed endpoint or
// explicit DB reset to re-apply.
func (s *Store) migrateV2() error {
	now := nowTS()
	for _, r := range SeedV1Rules() {
		enabled := 0
		if r.Enabled {
			enabled = 1
		}
		_, err := s.db.Exec(
			`INSERT INTO rules (id, scope, priority, tool, pattern, outcome, message, enabled, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Scope, r.Priority, r.Tool, r.Pattern, r.Outcome, r.Message, enabled, now, now,
		)
		if err != nil {
			return fmt.Errorf("insert seed %q: %w", r.ID, err)
		}
	}
	return nil
}

func (s *Store) migrateV1() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS rules (
			id          TEXT PRIMARY KEY,
			scope       TEXT NOT NULL,
			priority    INTEGER NOT NULL DEFAULT 100,
			tool        TEXT NOT NULL,
			pattern     TEXT NOT NULL DEFAULT '',
			outcome     TEXT NOT NULL,
			message     TEXT NOT NULL DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_rules_scope_priority ON rules(scope, priority);
		CREATE INDEX IF NOT EXISTS idx_rules_tool ON rules(tool);

		CREATE TABLE IF NOT EXISTS audit (
			id              TEXT PRIMARY KEY,
			ts              TEXT NOT NULL,
			bridge_id       TEXT NOT NULL DEFAULT '',
			instance_id     TEXT NOT NULL DEFAULT '',
			tool            TEXT NOT NULL,
			input_json      TEXT NOT NULL DEFAULT '',
			atoms_json      TEXT NOT NULL DEFAULT '',
			outcome         TEXT NOT NULL,
			matched_rule_id TEXT NOT NULL DEFAULT '',
			resolved_by     TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts);
		CREATE INDEX IF NOT EXISTS idx_audit_bridge ON audit(bridge_id);
		CREATE INDEX IF NOT EXISTS idx_audit_instance ON audit(instance_id);
	`)
	return err
}

// --- Rule CRUD ----------------------------------------------------------

// ListRules returns rules in the cascade order the engine consumes:
// instance scope → bridge scope → global, each ordered by (priority asc, id asc).
// Filters: scope is one of "" (all), "global", "bridge:<id>", "instance:<id>".
func (s *Store) ListRules(scope string) ([]Rule, error) {
	q := `SELECT id, scope, priority, tool, pattern, outcome, message, enabled, created_at, updated_at FROM rules`
	args := []any{}
	if scope != "" {
		q += ` WHERE scope = ?`
		args = append(args, scope)
	}
	q += ` ORDER BY scope, priority, id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Scope, &r.Priority, &r.Tool, &r.Pattern, &r.Outcome, &r.Message, &enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule returns one rule by id, or sql.ErrNoRows if missing.
func (s *Store) GetRule(id string) (Rule, error) {
	var r Rule
	var enabled int
	err := s.db.QueryRow(
		`SELECT id, scope, priority, tool, pattern, outcome, message, enabled, created_at, updated_at FROM rules WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.Scope, &r.Priority, &r.Tool, &r.Pattern, &r.Outcome, &r.Message, &enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Rule{}, err
	}
	r.Enabled = enabled != 0
	return r, nil
}

// CreateRule inserts a rule. The caller supplies id; if empty, one is minted
// from time + random suffix. Timestamps are stamped here.
func (s *Store) CreateRule(r Rule) (Rule, error) {
	if r.ID == "" {
		r.ID = newID("rule")
	}
	now := nowTS()
	r.CreatedAt = now
	r.UpdatedAt = now
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO rules (id, scope, priority, tool, pattern, outcome, message, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Scope, r.Priority, r.Tool, r.Pattern, r.Outcome, r.Message, enabled, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return Rule{}, fmt.Errorf("insert rule: %w", err)
	}
	return r, nil
}

// UpdateRule applies a partial patch. Empty / zero fields in patch are
// preserved as-is in the row (no nil-vs-zero ambiguity at this layer — the
// HTTP handler decides what's a meaningful patch).
func (s *Store) UpdateRule(r Rule) (Rule, error) {
	r.UpdatedAt = nowTS()
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(
		`UPDATE rules SET scope=?, priority=?, tool=?, pattern=?, outcome=?, message=?, enabled=?, updated_at=? WHERE id=?`,
		r.Scope, r.Priority, r.Tool, r.Pattern, r.Outcome, r.Message, enabled, r.UpdatedAt, r.ID,
	)
	if err != nil {
		return Rule{}, fmt.Errorf("update rule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Rule{}, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return Rule{}, sql.ErrNoRows
	}
	return r, nil
}

// DeleteRule removes a rule by id. Missing id is not an error.
func (s *Store) DeleteRule(id string) error {
	_, err := s.db.Exec(`DELETE FROM rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// --- Audit --------------------------------------------------------------

// InsertAudit appends one entry to the audit log. id and ts are stamped here.
func (s *Store) InsertAudit(e AuditEntry) (AuditEntry, error) {
	if e.ID == "" {
		e.ID = newID("aud")
	}
	if e.TS == "" {
		e.TS = nowTS()
	}
	_, err := s.db.Exec(
		`INSERT INTO audit (id, ts, bridge_id, instance_id, tool, input_json, atoms_json, outcome, matched_rule_id, resolved_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TS, e.BridgeID, e.InstanceID, e.Tool, string(e.InputJSON), string(e.AtomsJSON), e.Outcome, e.MatchedRuleID, e.ResolvedBy,
	)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("insert audit: %w", err)
	}
	return e, nil
}

// ListAuditFilter narrows ListAudit results.
type ListAuditFilter struct {
	BridgeID   string
	InstanceID string
	Tool       string
	Since      string // ISO 8601 / datetimeoffset; entries with ts >= since
	Limit      int    // 0 → 100
	Offset     int
}

// ListAudit returns audit entries matching the filter, newest first.
func (s *Store) ListAudit(f ListAuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT id, ts, bridge_id, instance_id, tool, input_json, atoms_json, outcome, matched_rule_id, resolved_by FROM audit`
	args := []any{}
	where := []string{}
	if f.BridgeID != "" {
		where = append(where, "bridge_id = ?")
		args = append(args, f.BridgeID)
	}
	if f.InstanceID != "" {
		where = append(where, "instance_id = ?")
		args = append(args, f.InstanceID)
	}
	if f.Tool != "" {
		where = append(where, "tool = ?")
		args = append(args, f.Tool)
	}
	if f.Since != "" {
		where = append(where, "ts >= ?")
		args = append(args, f.Since)
	}
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	q += " ORDER BY ts DESC LIMIT ? OFFSET ?"
	args = append(args, f.Limit, f.Offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var input, atoms string
		if err := rows.Scan(&e.ID, &e.TS, &e.BridgeID, &e.InstanceID, &e.Tool, &input, &atoms, &e.Outcome, &e.MatchedRuleID, &e.ResolvedBy); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		if input != "" {
			e.InputJSON = json.RawMessage(input)
		}
		if atoms != "" {
			e.AtomsJSON = json.RawMessage(atoms)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- helpers ------------------------------------------------------------

// nowTS returns RFC3339 with offset (the "datetimeoffset" convention from the
// auto-memory: always carry an explicit timezone so comparisons are unambiguous).
func nowTS() string {
	return time.Now().Format(time.RFC3339Nano)
}

func newID(prefix string) string {
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixNano(), randHex(4))
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}
