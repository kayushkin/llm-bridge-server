package store

import (
	"database/sql"
	"strings"
	"time"
)

// SessionSummaryRow is the projected sidebar row for the dashv2 list. It carries
// ONLY the light columns the sidebar needs — deliberately no info /
// harness_config blobs, which are the expensive part of a full Session and are
// fetched lazily per session when actually opened.
//
// UpdatedAt / CreatedAt are parsed times for wire formatting; Cursor is the
// compound pagination cursor (updated_at plus session id), used opaquely by the
// caller so it never has to reason about the on-disk timestamp format.
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

// summarySessionIDExpression is the projected session id: session_id falling
// back to bridge_id for legacy rows (mirrors sessionColumns' COALESCE).
//
// It is a named constant because ORDER BY and the cursor filter MUST sort and
// compare the very same expression the row reports as its SessionID. Ordering by
// the bare session_id column while the cursor carries the COALESCEd value would
// put a legacy row (empty session_id, id supplied by bridge_id) in one place in
// the sort and look for it in another, which is the same skipped-row failure
// this compound cursor exists to close.
const summarySessionIDExpression = `COALESCE(NULLIF(session_id, ''), bridge_id)`

// summaryColumns are the projected columns, in scan order. The raw updated_at is
// selected a second time (as text) as the timestamp half of the cursor; the id
// half is the already-projected session id, assembled in Go.
const summaryColumns = summarySessionIDExpression + `, state, harness, COALESCE(instance_id, ''), COALESCE(type, ''), COALESCE(purpose, ''), COALESCE(mode, ''), COALESCE(folder_name, ''), display_name, COALESCE(agent_id, ''), updated_at, created_at, CAST(updated_at AS TEXT)`

// ListSessionSummaries returns projected session rows newest-first, paginated by
// (updated_at, session id). `before` is the opaque cursor returned as a prior
// page's last row's Cursor ("" for the first page). limit <= 0 defaults to 100.
//
// The sort key is compound because updated_at alone is not unique: 30 groups of
// rows in the live table share a timestamp to the nanosecond. A page boundary
// landing inside such a group used to skip the rest of it — the strict `<` on
// the timestamp excluded the tied rows from the next page, and the page size had
// already excluded them from this one, so they were returned by NO page and were
// unreachable by paging entirely. Tie-breaking on the session id, which is
// unique, gives every row a distinct position and closes the gap.
//
// Ordering still uses the already-indexed updated_at column directly
// (idx_sessions_updated_at), so no full temp b-tree is built; SQLite satisfies
// the leading term from the index and block-sorts only the right part of the
// ORDER BY, i.e. within a tie group. Those groups hold at most 3 rows.
//
// The cursor filter compares updated_at as TEXT: every row's timestamp is
// written by the store as Go's uniform t.String() format, which is lexically
// monotonic with time, so a TEXT comparison paginates correctly without
// depending on the column's numeric affinity.
func (s *Store) ListSessionSummaries(limit int, before string) ([]SessionSummaryRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + summaryColumns + ` FROM sessions`
	args := []any{}
	if before != "" {
		// A cursor minted before the compound format existed carries no id half,
		// and needs no special case: decodeSummaryCursor yields "" for it, no id
		// sorts below the empty string, so the second disjunct is never true and
		// the filter collapses to exactly the timestamp-only comparison that
		// cursor was built for. Pinned by
		// TestListSessionSummaries_LegacyTimestampOnlyCursor — the equivalence is
		// the reason there is one branch here rather than two.
		updatedAtText, sessionID := decodeSummaryCursor(before)
		q += ` WHERE CAST(updated_at AS TEXT) < ?` +
			` OR (CAST(updated_at AS TEXT) = ? AND ` + summarySessionIDExpression + ` < ?)`
		args = append(args, updatedAtText, updatedAtText, sessionID)
	}
	q += ` ORDER BY updated_at DESC, ` + summarySessionIDExpression + ` DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.dbRO.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummaryRow
	for rows.Next() {
		var r SessionSummaryRow
		var updatedAtText string
		if err := rows.Scan(
			&r.SessionID, &r.State, &r.Harness, &r.InstanceID, &r.Type, &r.Purpose,
			&r.Mode, &r.FolderName, &r.DisplayName, &r.AgentID, &r.UpdatedAt, &r.CreatedAt, &updatedAtText,
		); err != nil {
			return nil, err
		}
		r.Cursor = encodeSummaryCursor(updatedAtText, r.SessionID)
		out = append(out, r)
	}
	return out, rows.Err()
}

// summaryCursorSeparator joins the two halves of the compound cursor.
//
// It is safe as a separator because the timestamp half is always Go's
// t.String() format, whose alphabet is digits, '-', ':', '.', '+' and spaces —
// a vertical bar cannot occur in it. The id half is NOT constrained, so the
// halves are split on the FIRST separator and everything after it is the id.
// That way an id containing a bar round-trips intact instead of being truncated
// into a cursor that points at a session which does not exist.
const summaryCursorSeparator = "|"

// encodeSummaryCursor builds the opaque pagination cursor from the two halves of
// the sort key. Callers treat the result as opaque and hand it back verbatim.
func encodeSummaryCursor(updatedAtText, sessionID string) string {
	return updatedAtText + summaryCursorSeparator + sessionID
}

// decodeSummaryCursor splits a cursor back into its two halves. A cursor with no
// separator — one minted before the compound format existed — yields the whole
// string as the timestamp and "" as the id, which is the correct bound for it
// rather than a fallback: "" is below every session id, so filtering on it
// reproduces the old timestamp-only behaviour exactly.
func decodeSummaryCursor(cursor string) (updatedAtText, sessionID string) {
	updatedAtText, sessionID, _ = strings.Cut(cursor, summaryCursorSeparator)
	return updatedAtText, sessionID
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
