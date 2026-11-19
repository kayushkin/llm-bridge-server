package hookstore

import (
	"database/sql"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// CreateHook inserts a new hook row. Caller supplies ID (ULID recommended).
// CreatedAt/UpdatedAt are overwritten with the current UTC time.
func (s *Store) CreateHook(h *msg.Hook) error {
	now := time.Now().UTC()
	h.CreatedAt = now
	h.UpdatedAt = now
	_, err := s.db.Exec(`
		INSERT INTO hooks (id, harness, event, matcher, command, scope_kind, scope_id, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.Harness, h.Event, h.Matcher, h.Command,
		h.ScopeKind, h.ScopeID, h.Enabled, h.CreatedAt, h.UpdatedAt,
	)
	return err
}

// GetHook retrieves a hook by ID.
func (s *Store) GetHook(id string) (*msg.Hook, error) {
	row := s.db.QueryRow(`
		SELECT id, harness, event, matcher, command, scope_kind, scope_id, enabled, created_at, updated_at
		FROM hooks WHERE id = ?`, id)
	return scanHook(row)
}

// UpdateHook overwrites a hook's mutable fields. ID, CreatedAt are not
// touched; UpdatedAt is refreshed.
func (s *Store) UpdateHook(h *msg.Hook) error {
	h.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(`
		UPDATE hooks SET harness=?, event=?, matcher=?, command=?, scope_kind=?, scope_id=?, enabled=?, updated_at=?
		WHERE id=?`,
		h.Harness, h.Event, h.Matcher, h.Command,
		h.ScopeKind, h.ScopeID, h.Enabled, h.UpdatedAt, h.ID,
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

// SetHookEnabled toggles the enabled flag without rewriting other fields.
func (s *Store) SetHookEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE hooks SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteHook removes a hook row.
func (s *Store) DeleteHook(id string) error {
	res, err := s.db.Exec(`DELETE FROM hooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListFilter narrows ListHooks. Zero-valued fields are ignored.
type ListFilter struct {
	Harness    msg.Harness
	Event      string
	ScopeKind  msg.HookScope
	ScopeID    string
	EnabledSet bool // if true, filter on Enabled; if false, return both
	Enabled    bool
}

// ListHooks returns hooks matching filter, ordered by creation time.
func (s *Store) ListHooks(filter ListFilter) ([]msg.Hook, error) {
	query := `SELECT id, harness, event, matcher, command, scope_kind, scope_id, enabled, created_at, updated_at FROM hooks WHERE 1=1`
	var args []any
	if filter.Harness != "" {
		query += ` AND harness = ?`
		args = append(args, filter.Harness)
	}
	if filter.Event != "" {
		query += ` AND event = ?`
		args = append(args, filter.Event)
	}
	if filter.ScopeKind != "" {
		query += ` AND scope_kind = ?`
		args = append(args, filter.ScopeKind)
	}
	if filter.ScopeID != "" {
		query += ` AND scope_id = ?`
		args = append(args, filter.ScopeID)
	}
	if filter.EnabledSet {
		query += ` AND enabled = ?`
		args = append(args, filter.Enabled)
	}
	query += ` ORDER BY created_at`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []msg.Hook
	for rows.Next() {
		h, err := scanHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// ListApplicable returns the enabled hooks that apply to a session spawn,
// with scope collisions resolved: when two hooks share (harness, event,
// matcher), the most specific scope wins (session > instance > global).
// InstanceID and SessionID may be empty (pre-instance or pre-session
// queries); those scope layers are simply skipped.
func (s *Store) ListApplicable(harness msg.Harness, instanceID, sessionID string) ([]msg.Hook, error) {
	query := `
		SELECT id, harness, event, matcher, command, scope_kind, scope_id, enabled, created_at, updated_at
		FROM hooks
		WHERE enabled = 1
		  AND harness = ?
		  AND (
		    scope_kind = 'global'
		    OR (scope_kind = 'instance' AND scope_id = ?)
		    OR (scope_kind = 'session'  AND scope_id = ?)
		  )`
	rows, err := s.db.Query(query, harness, instanceID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect, then dedupe by (event, matcher) preferring the most specific scope.
	type key struct {
		event, matcher string
	}
	byKey := map[key]msg.Hook{}
	priority := map[msg.HookScope]int{
		msg.HookScopeGlobal:   0,
		msg.HookScopeInstance: 1,
		msg.HookScopeSession:  2,
	}
	for rows.Next() {
		h, err := scanHook(rows)
		if err != nil {
			return nil, err
		}
		k := key{h.Event, h.Matcher}
		if existing, ok := byKey[k]; ok {
			if priority[h.ScopeKind] <= priority[existing.ScopeKind] {
				continue
			}
		}
		byKey[k] = *h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]msg.Hook, 0, len(byKey))
	for _, h := range byKey {
		out = append(out, h)
	}
	return out, nil
}

// CountHooks returns the total number of hook rows.
func (s *Store) CountHooks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM hooks`).Scan(&n)
	return n, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanHook(r rowScanner) (*msg.Hook, error) {
	var h msg.Hook
	var enabled int
	if err := r.Scan(&h.ID, &h.Harness, &h.Event, &h.Matcher, &h.Command,
		&h.ScopeKind, &h.ScopeID, &enabled, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.Enabled = enabled != 0
	return &h, nil
}
