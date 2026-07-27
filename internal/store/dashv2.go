package store

import (
	"database/sql"
	"time"
)

// SessionSummaryRow is the projected sidebar row for the dashv2 list. It carries
// ONLY the light columns the sidebar needs — deliberately no info /
// harness_config blobs, which are the expensive part of a full Session and are
// fetched lazily per session when actually opened.
//
// UpdatedAt / CreatedAt are parsed times for wire formatting; Cursor is the raw
// stored updated_at string, used opaquely as the pagination cursor so the
// caller never has to reason about the on-disk timestamp format.
type SessionSummaryRow struct {
	SessionID   string
	State       string
	Harness     string
	InstanceID  string
	Type        string
	Purpose     string
	Mode        string
	FolderName  string
	DisplayName string
	AgentID     string
	UpdatedAt   time.Time
	CreatedAt   time.Time
	Cursor      string
}

// summaryColumns are the projected columns, in scan order. session_id falls
// back to bridge_id for legacy rows (mirrors sessionColumns' COALESCE). The raw
// updated_at is selected a second time (as text) for the opaque cursor.
const summaryColumns = `COALESCE(NULLIF(session_id, ''), bridge_id), state, harness, COALESCE(instance_id, ''), COALESCE(type, ''), COALESCE(purpose, ''), COALESCE(mode, ''), COALESCE(folder_name, ''), display_name, COALESCE(agent_id, ''), updated_at, created_at, CAST(updated_at AS TEXT)`

// ListSessionSummaries returns projected session rows newest-first, paginated by
// updated_at. `before` is the opaque cursor returned as a prior page's last
// row's Cursor ("" for the first page). limit <= 0 defaults to 100.
//
// Ordering uses the already-indexed updated_at column directly (idx_sessions_
// updated_at), so no created_at temp b-tree is built. The cursor filter compares
// updated_at as TEXT: every row's timestamp is written by the store as Go's
// uniform t.String() format, which is lexically monotonic with time, so a TEXT
// comparison paginates correctly without depending on the column's numeric
// affinity.
func (s *Store) ListSessionSummaries(limit int, before string) ([]SessionSummaryRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + summaryColumns + ` FROM sessions`
	args := []any{}
	if before != "" {
		q += ` WHERE CAST(updated_at AS TEXT) < ?`
		args = append(args, before)
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.dbRO.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummaryRow
	for rows.Next() {
		var r SessionSummaryRow
		if err := rows.Scan(
			&r.SessionID, &r.State, &r.Harness, &r.InstanceID, &r.Type, &r.Purpose,
			&r.Mode, &r.FolderName, &r.DisplayName, &r.AgentID, &r.UpdatedAt, &r.CreatedAt, &r.Cursor,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MaxSessionUpdatedAt returns the newest updated_at across the sessions table as
// its raw stored string, used as the summary list's revision / ETag. Empty
// string when the table has no rows. Uses the same ordering as
// ListSessionSummaries so the revision is consistent with the first page.
func (s *Store) MaxSessionUpdatedAt() (string, error) {
	var v sql.NullString
	err := s.dbRO.QueryRow(
		`SELECT CAST(updated_at AS TEXT) FROM sessions ORDER BY updated_at DESC LIMIT 1`,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !v.Valid {
		return "", nil
	}
	return v.String, nil
}
