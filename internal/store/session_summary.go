package store

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// SessionSummaryRow is the projected sidebar row for the chat sidebar. It carries
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

	// ManagerSessionID is the BRIDGE session id of the session that spawned this
	// one, empty for a top-level session. It is the store's own parent pointer —
	// set by the subagent promoter (internal/harness/subagent.go) — and nothing
	// derives or invents it here.
	//
	// It was absent from this projection while being present on the full session
	// record, so `GET /sessions/{id}` and every SSE upsert carried it and the
	// paged LIST did not. The sidebar reads only the list, so it could not tell a
	// parent from a child at all: 1,325 subagent sessions across 435 parents sat
	// in it as unrelated rows.
	ManagerSessionID string
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
const summaryColumns = summarySessionIDExpression + `, state, harness, COALESCE(instance_id, ''), COALESCE(type, ''), COALESCE(purpose, ''), COALESCE(mode, ''), COALESCE(folder_name, ''), display_name, COALESCE(agent_id, ''), updated_at, created_at, COALESCE(manager_session_id, ''), CAST(updated_at AS TEXT)`

// SessionSummaryFilter narrows ListSessionSummaries to the rows matching every
// non-empty axis. An empty axis constrains nothing, and the values within one
// axis are OR'd together — the same inclusion semantics chat-core's sidebar
// chips already use, so the server and the client cannot mean two different
// things by the same filter.
//
// The axes mirror the sidebar's six. `InstanceIDs` is the one whose name differs
// from its chip label: the sidebar calls that axis "machine" and resolves an
// instance to a machine name for DISPLAY only, while the value it actually
// filters on is always the instance id. This field is named for the value it
// matches, not for the label above it.
type SessionSummaryFilter struct {
	Harnesses   []string
	States      []string
	Types       []string
	Purposes    []string
	Modes       []string
	InstanceIDs []string

	// SessionIDs names the exact rows wanted, and is NOT one of the sidebar's
	// six chip axes — it is a LOOKUP. A caller that already holds ids and needs
	// what they are called ("these 17 sessions raised a signal; what are their
	// names?") cannot express that as a chip, and cannot page to them either:
	// the row it wants may be thousands deep in a listing ordered by recency.
	//
	// Kept off axes() for that reason. axes() is documented as mirroring the
	// sidebar, it feeds the chip-oriented IsEmpty, and folding a lookup into it
	// would make "the filter constrains nothing" false for a request that
	// constrains it to one row.
	SessionIDs []string

	// ManagerSessionIDs names the PARENTS whose children are wanted, and like
	// SessionIDs above it is a LOOKUP rather than a chip axis: "what did these
	// sessions spawn?".
	//
	// It cannot be answered by paging. A parent's children are ordered by their
	// own recency, not the parent's, so a session that spawned 106 subagents last
	// week has them scattered thousands of rows deep in a listing the sidebar
	// reads one page of. Grouping only the children that happen to be on the
	// loaded page would show four of the 106 and say nothing about the rest.
	//
	// Several parents at once, deliberately: the sidebar asks about a whole page
	// of rows in ONE request rather than one request per row, which is also what
	// lets it know WHICH rows have children without a count column existing.
	ManagerSessionIDs []string
}

// summaryFilterAxis pairs one filterable column expression with the values a row
// must match on it.
type summaryFilterAxis struct {
	column string
	values []string
}

// axes reports the filter's columns in a FIXED order. Fixed because that order
// decides both the SQL argument order and the response-cache key, and ranging a
// map would let two identical requests build two different keys.
//
// Every column is COALESCEd to ” to match the projection in summaryColumns: a
// row whose type is NULL has to compare as the empty string, rather than drop
// out of the comparison the silent way SQL NULL does.
func (f SessionSummaryFilter) axes() []summaryFilterAxis {
	return []summaryFilterAxis{
		{`COALESCE(harness, '')`, f.Harnesses},
		{`COALESCE(state, '')`, f.States},
		{`COALESCE(type, '')`, f.Types},
		{`COALESCE(purpose, '')`, f.Purposes},
		{`COALESCE(mode, '')`, f.Modes},
		{`COALESCE(instance_id, '')`, f.InstanceIDs},
	}
}

// IsEmpty reports whether the filter constrains nothing, so a caller can tell an
// unfiltered listing from a filtered one without reaching into the axes.
func (f SessionSummaryFilter) IsEmpty() bool {
	if len(f.SessionIDs) > 0 || len(f.ManagerSessionIDs) > 0 {
		return false
	}
	for _, axis := range f.axes() {
		if len(axis.values) > 0 {
			return false
		}
	}
	return true
}

// CacheKey renders the filter as a deterministic string for the response cache.
//
// Values are sorted so two requests that select the same set in a different chip
// order share one entry. This belongs in the key rather than beside it: a page
// served from an entry cached under a DIFFERENT filter is a wrong answer, not a
// stale one, and it would look exactly like a correct one.
func (f SessionSummaryFilter) CacheKey() string {
	var b strings.Builder
	for _, axis := range f.axes() {
		b.WriteString(axis.column)
		b.WriteByte('=')
		sorted := append([]string(nil), axis.values...)
		sort.Strings(sorted)
		b.WriteString(strings.Join(sorted, ","))
		b.WriteByte(';')
	}
	// The lookup rides the key too, and for the same reason every axis does: a
	// page cached for "these 17 ids" served to a request for a different 17 is
	// a wrong answer that looks exactly like a right one.
	b.WriteString("session_ids=")
	ids := append([]string(nil), f.SessionIDs...)
	sort.Strings(ids)
	b.WriteString(strings.Join(ids, ","))
	b.WriteByte(';')
	// Same reasoning as the lookup above: a page of one parent's children served
	// to a request about a different parent is a wrong answer that looks right.
	b.WriteString("manager_session_ids=")
	managers := append([]string(nil), f.ManagerSessionIDs...)
	sort.Strings(managers)
	b.WriteString(strings.Join(managers, ","))
	b.WriteByte(';')
	return b.String()
}

// sqlPlaceholders renders n bound-parameter placeholders as "?, ?, ?".
//
// The values themselves are always bound, never interpolated: they arrive
// straight from a URL query string, and an IN list built by string concatenation
// is the textbook injection.
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, '?')
	}
	return string(b)
}

// ListSessionSummaries returns projected session rows newest-first, paginated by
// (updated_at, session id) and narrowed by `filter`. `before` is the opaque
// cursor returned as a prior page's last row's Cursor ("" for the first page).
// limit <= 0 defaults to 100. A zero-value filter lists everything.
//
// Filtering happens HERE rather than in the client because the two compose
// badly: the client can only sieve the page it was given, so a page of the
// newest 100 sessions spent on machine traffic leaves it with a handful of rows
// and no way to ask for more of the kind it wants. Measured on the live box,
// interactive sessions are ~8% of any recent window, so a client-side sieve
// discards 92 rows to keep 8.
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
// A filter rides along as a residual test during that same index scan — it needs
// no index of its own and does not change the plan. Measured against the live
// 3.1 GB bridge.db (8,845 sessions): the newest filtered 100 in 23ms, and 42ms a
// thousand rows deep. The cost of a filter is that the scan walks further before
// it fills a page, which is the point — those are the rows the client would
// otherwise have fetched and thrown away.
//
// The cursor filter compares updated_at as TEXT: every row's timestamp is
// written by the store as Go's uniform t.String() format, which is lexically
// monotonic with time, so a TEXT comparison paginates correctly without
// depending on the column's numeric affinity.
func (s *Store) ListSessionSummaries(limit int, before string, filter SessionSummaryFilter) ([]SessionSummaryRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + summaryColumns + ` FROM sessions`
	conds := []string{}
	args := []any{}
	if before != "" {
		// A cursor minted before the compound format existed carries no id half,
		// and needs no special case: decodeSummaryCursor yields "" for it, no id
		// sorts below the empty string, so the second disjunct is never true and
		// the comparison collapses to exactly the timestamp-only one that cursor
		// was built for. Pinned by
		// TestListSessionSummaries_LegacyTimestampOnlyCursor — the equivalence is
		// the reason there is one branch here rather than two.
		updatedAtText, sessionID := decodeSummaryCursor(before)
		// Wrapped in its own parentheses as a whole. The cursor test is itself an
		// OR, so leaving it bare and appending ` AND <axis>` would bind as
		// `A OR (B AND C AND axis)` — every row newer than the cursor would come
		// back regardless of the filter, and only on the SECOND page onwards, so
		// page one would look perfectly correct.
		conds = append(conds, `(CAST(updated_at AS TEXT) < ?`+
			` OR (CAST(updated_at AS TEXT) = ? AND `+summarySessionIDExpression+` < ?))`)
		args = append(args, updatedAtText, updatedAtText, sessionID)
	}
	for _, axis := range filter.axes() {
		if len(axis.values) == 0 {
			continue
		}
		conds = append(conds, axis.column+` IN (`+sqlPlaceholders(len(axis.values))+`)`)
		for _, v := range axis.values {
			args = append(args, v)
		}
	}
	// Compared against the same projected expression the rows report and the
	// cursor sorts on, never the bare session_id column — a legacy row whose id
	// comes from bridge_id would otherwise be unfindable by the very id the
	// caller was handed.
	if len(filter.SessionIDs) > 0 {
		conds = append(conds, summarySessionIDExpression+` IN (`+sqlPlaceholders(len(filter.SessionIDs))+`)`)
		for _, v := range filter.SessionIDs {
			args = append(args, v)
		}
	}
	// The parent pointer is stored as the parent's BRIDGE session id (see
	// internal/store/subagent_test.go, which pins exactly that), so this compares
	// the raw column against the ids the caller holds — which are themselves
	// projected session ids. No COALESCE fallback to bridge_id is wanted here:
	// the promoter writes one id into this column and it is the same id space.
	if len(filter.ManagerSessionIDs) > 0 {
		conds = append(conds, `COALESCE(manager_session_id, '') IN (`+sqlPlaceholders(len(filter.ManagerSessionIDs))+`)`)
		for _, v := range filter.ManagerSessionIDs {
			args = append(args, v)
		}
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
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
			&r.Mode, &r.FolderName, &r.DisplayName, &r.AgentID, &r.UpdatedAt, &r.CreatedAt,
			&r.ManagerSessionID, &updatedAtText,
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
