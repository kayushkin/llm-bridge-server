// Package permissions is the rule engine for tool-call gating across the
// llm-bridge harnesses: rules, the verdict on one tool call, and an audit row
// for every verdict. The store keeps both in Postgres, in the schema of
// Module, through postgresstorage; engine.go is the cascade evaluator and
// handlers.go the HTTP surface the server mounts.
package permissions

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
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

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its rules and audit
// in, with the migrations that build them and insert the default rules.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "permissions", Role: "permissions_module", Migrations: migrations}, nil
}

// Store keeps the rules and the audit in the module's schema.
type Store struct {
	db *sql.DB
}

// New returns the store on db, a pool Module's schema has been migrated on.
func New(db *sql.DB) *Store { return &Store{db: db} }

const ruleColumns = `id, scope, priority, tool, pattern, outcome, message, enabled, created_at, updated_at`

func scanRule(scan func(...any) error) (Rule, error) {
	var r Rule
	err := scan(&r.ID, &r.Scope, &r.Priority, &r.Tool, &r.Pattern, &r.Outcome, &r.Message, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// --- Rule CRUD ----------------------------------------------------------

// ListRules returns rules in the cascade order the engine consumes:
// instance scope → bridge scope → global, each ordered by (priority asc, id asc).
// Filters: scope is one of "" (all), "global", "bridge:<id>", "instance:<id>".
func (s *Store) ListRules(ctx context.Context, scope string) ([]Rule, error) {
	q := `SELECT ` + ruleColumns + ` FROM rules`
	args := []any{}
	if scope != "" {
		q += ` WHERE scope = $1`
		args = append(args, scope)
	}
	// COLLATE "C" orders by bytes, as SQLite's BINARY did.
	q += ` ORDER BY scope COLLATE "C", priority, id COLLATE "C"`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule returns one rule by id, or sql.ErrNoRows if missing.
func (s *Store) GetRule(ctx context.Context, id string) (Rule, error) {
	return scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id = $1`, id).Scan)
}

// CreateRule inserts a rule. The caller supplies id; if empty, one is minted
// from time + random suffix. Timestamps are stamped here.
func (s *Store) CreateRule(ctx context.Context, r Rule) (Rule, error) {
	if r.ID == "" {
		r.ID = newID("rule")
	}
	now := nowTS()
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rules (`+ruleColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.ID, r.Scope, r.Priority, r.Tool, r.Pattern, r.Outcome, r.Message, r.Enabled, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return Rule{}, fmt.Errorf("insert rule: %w", err)
	}
	return r, nil
}

// UpdateRule writes every field of r but created_at over the row with r's id.
// The HTTP handler merges a partial patch into the stored rule first, so r
// carries the stored created_at.
func (s *Store) UpdateRule(ctx context.Context, r Rule) (Rule, error) {
	r.UpdatedAt = nowTS()
	res, err := s.db.ExecContext(ctx,
		`UPDATE rules SET scope = $1, priority = $2, tool = $3, pattern = $4, outcome = $5, message = $6, enabled = $7, updated_at = $8 WHERE id = $9`,
		r.Scope, r.Priority, r.Tool, r.Pattern, r.Outcome, r.Message, r.Enabled, r.UpdatedAt, r.ID,
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
func (s *Store) DeleteRule(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// --- Audit --------------------------------------------------------------

// InsertAudit appends one entry to the audit log. id and ts are stamped here.
func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) (AuditEntry, error) {
	if e.ID == "" {
		e.ID = newID("aud")
	}
	if e.TS == "" {
		e.TS = nowTS()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (id, ts, bridge_id, instance_id, tool, input_json, atoms_json, outcome, matched_rule_id, resolved_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
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

// ListAudit returns audit entries matching the filter, newest first. ts is
// compared and ordered as text, byte by byte, as the standalone store did;
// rows with the same ts come newest written first.
func (s *Store) ListAudit(ctx context.Context, f ListAuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT id, ts, bridge_id, instance_id, tool, input_json, atoms_json, outcome, matched_rule_id, resolved_by FROM audit`
	args := []any{}
	where := []string{}
	placeholder := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.BridgeID != "" {
		where = append(where, "bridge_id = "+placeholder(f.BridgeID))
	}
	if f.InstanceID != "" {
		where = append(where, "instance_id = "+placeholder(f.InstanceID))
	}
	if f.Tool != "" {
		where = append(where, "tool = "+placeholder(f.Tool))
	}
	if f.Since != "" {
		where = append(where, `ts COLLATE "C" >= `+placeholder(f.Since))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += ` ORDER BY ts COLLATE "C" DESC, number DESC LIMIT ` + placeholder(f.Limit) + ` OFFSET ` + placeholder(f.Offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
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
