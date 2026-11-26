package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
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
	// BundleID is the bundle-store id the session was started with, or "".
	BundleID string
	// SpendUSD is the session's cost estimate (msg.ManagedSession.SpendUSD).
	SpendUSD  float64
	UpdatedAt time.Time
	CreatedAt time.Time
	Cursor    string

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

	// Status is the session's whole status, of which State is one field, with
	// the event row id it is current as of. Never nil. See msg.SessionStatus.
	Status *msg.SessionStatus

	// WorkingDir is the directory the session was started in: the sessions
	// table's own working_dir column, the same value the full record's
	// working_dir carries. "" when none was recorded.
	WorkingDir string

	// PrincipalID is who the session was started as; "" for no principal.
	PrincipalID string
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

// summaryColumns are the projected columns, in scan order. The cursor is made
// in Go from the row's updated_at and its projected session id.
const summaryColumns = summarySessionIDExpression + `, state, harness, instance_id, type, purpose, mode, folder_name, display_name, agent_id, bundle_id, updated_at, created_at, manager_session_id, spend_usd, status, working_dir, principal_id`

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
	// PrincipalIDs are the principals the sessions were started as; '' matches
	// a session started as no principal (the scheduler, an agent, a script).
	PrincipalIDs []string

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

	// OwnedByPrincipalID narrows to the sessions started as one principal.
	// It is not a chip axis either: it is set by the server from the caller's
	// identity request authorization verified, never from the
	// query, and a session with no principal matches no principal. Empty means
	// "don't narrow".
	OwnedByPrincipalID string
	// OwnedThroughPeerPrincipalID narrows further to the sessions a connected
	// server started through this peer principal for one of its users. Set by
	// the server beside OwnedByPrincipalID, never from the query.
	OwnedThroughPeerPrincipalID string
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
// Every column is NOT NULL with ” as its empty value, so a row with no type
// compares as the empty string.
func (f SessionSummaryFilter) axes() []summaryFilterAxis {
	return []summaryFilterAxis{
		{`harness`, f.Harnesses},
		{`state`, f.States},
		{`type`, f.Types},
		{`purpose`, f.Purposes},
		{`mode`, f.Modes},
		{`instance_id`, f.InstanceIDs},
		{`principal_id`, f.PrincipalIDs},
	}
}

// IsEmpty reports whether the filter constrains nothing, so a caller can tell an
// unfiltered listing from a filtered one without reaching into the axes.
func (f SessionSummaryFilter) IsEmpty() bool {
	if len(f.SessionIDs) > 0 || len(f.ManagerSessionIDs) > 0 || f.OwnedByPrincipalID != "" || f.OwnedThroughPeerPrincipalID != "" {
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
	// One principal's page served to another principal is a leak, not just a
	// wrong answer.
	b.WriteString("owned_by_principal_id=")
	b.WriteString(f.OwnedByPrincipalID)
	b.WriteByte(';')
	b.WriteString("owned_through_peer_principal_id=")
	b.WriteString(f.OwnedThroughPeerPrincipalID)
	b.WriteByte(';')
	return b.String()
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
// Ordering uses the indexed updated_at column directly (idx_sessions_updated_at),
// and a filter rides along as a residual test during that same index scan. The
// cost of a filter is that the scan walks further before it fills a page, which
// is the point — those are the rows the client would otherwise have fetched and
// thrown away.
//
// The cursor's timestamp half is the row's updated_at to the microsecond in UTC
// (summaryCursorTimeLayout), compared as a time.
func (s *Store) ListSessionSummaries(limit int, before string, filter SessionSummaryFilter) ([]SessionSummaryRow, error) {
	q, args, err := sessionSummariesQuery(limit, before, filter)
	if err != nil {
		return nil, err
	}
	rows, err := s.dbRO.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummaryRow
	for rows.Next() {
		var r SessionSummaryRow
		var statusJSON string
		if err := rows.Scan(
			&r.SessionID, &r.State, &r.Harness, &r.InstanceID, &r.Type, &r.Purpose,
			&r.Mode, &r.FolderName, &r.DisplayName, &r.AgentID, &r.BundleID, &r.UpdatedAt, &r.CreatedAt,
			&r.ManagerSessionID, &r.SpendUSD, &statusJSON, &r.WorkingDir, &r.PrincipalID,
		); err != nil {
			return nil, err
		}
		r.Status = sessionStatusFromColumns(r.State, statusJSON)
		r.Cursor = encodeSummaryCursor(r.UpdatedAt, r.SessionID)
		out = append(out, r)
	}
	return out, rows.Err()
}

// sessionSummariesQuery builds the statement ListSessionSummaries runs. It is
// its own function so a test can hand the exact statement to EXPLAIN: a lookup
// that stops using its index still answers correctly, only slowly, and no test
// of the rows would notice. A cursor that does not parse is an error.
func sessionSummariesQuery(limit int, before string, filter SessionSummaryFilter) (string, []any, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + summaryColumns + ` FROM sessions`
	conds := []string{}
	var args queryArguments
	if before != "" {
		// A cursor minted before the compound format existed carries no id half,
		// and needs no special case: decodeSummaryCursor yields "" for it, no id
		// sorts below the empty string, so the second disjunct is never true and
		// the comparison collapses to exactly the timestamp-only one that cursor
		// was built for. Pinned by
		// TestListSessionSummaries_LegacyTimestampOnlyCursor — the equivalence is
		// the reason there is one branch here rather than two.
		updatedAt, sessionID, err := decodeSummaryCursor(before)
		if err != nil {
			return "", nil, err
		}
		// Wrapped in its own parentheses as a whole. The cursor test is itself an
		// OR, so leaving it bare and appending ` AND <axis>` would bind as
		// `A OR (B AND C AND axis)` — every row newer than the cursor would come
		// back regardless of the filter, and only on the SECOND page onwards, so
		// page one would look perfectly correct.
		updatedAtArgument := args.add(updatedAt)
		conds = append(conds, `(updated_at < `+updatedAtArgument+
			` OR (updated_at = `+updatedAtArgument+` AND `+summarySessionIDExpression+` < `+args.add(sessionID)+`))`)
	}
	for _, axis := range filter.axes() {
		if len(axis.values) == 0 {
			continue
		}
		conds = append(conds, axis.column+` = ANY(`+args.add(axis.values)+`)`)
	}
	// Compared against the same projected expression the rows report and the
	// cursor sorts on, never the bare session_id column — a legacy row whose id
	// comes from bridge_id would otherwise be unfindable by the very id the
	// caller was handed.
	if len(filter.SessionIDs) > 0 {
		conds = append(conds, summarySessionIDExpression+` = ANY(`+args.add(filter.SessionIDs)+`)`)
	}
	// The parent pointer is stored as the parent's BRIDGE session id (see
	// internal/store/subagent_test.go, which pins exactly that), so this compares
	// the raw column against the ids the caller holds — which are themselves
	// projected session ids. No COALESCE fallback to bridge_id is wanted here:
	// the promoter writes one id into this column and it is the same id space.
	if len(filter.ManagerSessionIDs) > 0 {
		// The bare column beside the partial index's own predicate, so the
		// planner may use idx_sessions_manager. Wrapped in COALESCE this was a
		// scan of every session: 0.12s a call on 65,000 rows, from a sidebar
		// that asks several times a second.
		conds = append(conds, `manager_session_id <> '' AND manager_session_id = ANY(`+args.add(filter.ManagerSessionIDs)+`)`)
	}
	if filter.OwnedByPrincipalID != "" {
		conds = append(conds, `principal_id = `+args.add(filter.OwnedByPrincipalID))
	}
	if filter.OwnedThroughPeerPrincipalID != "" {
		conds = append(conds, summarySessionIDExpression+` IN (SELECT session_id FROM session_peer_origins WHERE peer_principal_id = `+args.add(filter.OwnedThroughPeerPrincipalID)+`)`)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	q += ` ORDER BY updated_at DESC, ` + summarySessionIDExpression + ` DESC LIMIT ` + args.add(limit)
	return q, args, nil
}

// summaryCursorSeparator joins the two halves of the compound cursor.
//
// It is safe as a separator because the timestamp half never holds a vertical
// bar. The id half is NOT constrained, so the halves are split on the FIRST
// separator and everything after it is the id. That way an id containing a bar
// round-trips intact instead of being truncated into a cursor that points at a
// session which does not exist.
const summaryCursorSeparator = "|"

// summaryCursorTimeLayout writes the cursor's timestamp half: UTC to the
// microsecond, the precision Postgres keeps, so the time read back from a
// cursor equals the row's updated_at exactly.
const summaryCursorTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// sqliteTimeLayout is how the bridge's SQLite database wrote a time (Go's
// time.String). A cursor a client holds from before the move to Postgres
// carries it.
const sqliteTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// encodeSummaryCursor builds the opaque pagination cursor from the two halves of
// the sort key. Callers treat the result as opaque and hand it back verbatim.
func encodeSummaryCursor(updatedAt time.Time, sessionID string) string {
	return formatSummaryTime(updatedAt) + summaryCursorSeparator + sessionID
}

func formatSummaryTime(updatedAt time.Time) string {
	return updatedAt.UTC().Format(summaryCursorTimeLayout)
}

// decodeSummaryCursor splits a cursor back into its two halves. A cursor with no
// separator — one minted before the compound format existed — yields the whole
// string as the timestamp and "" as the id, which is the correct bound for it
// rather than a fallback: "" is below every session id, so filtering on it
// reproduces the old timestamp-only behavior exactly. A timestamp in neither
// layout is an error.
func decodeSummaryCursor(cursor string) (time.Time, string, error) {
	updatedAtText, sessionID, _ := strings.Cut(cursor, summaryCursorSeparator)
	for _, layout := range []string{summaryCursorTimeLayout, sqliteTimeLayout} {
		if updatedAt, err := time.Parse(layout, updatedAtText); err == nil {
			return updatedAt, sessionID, nil
		}
	}
	return time.Time{}, "", fmt.Errorf("the cursor's time %q is not one this server writes", updatedAtText)
}

// MaxSessionUpdatedAt returns the newest updated_at across the sessions table,
// written as a cursor's timestamp half, used as the summary list's revision /
// ETag. Empty string when the table has no rows. Uses the same ordering as
// ListSessionSummaries so the revision is consistent with the first page.
func (s *Store) MaxSessionUpdatedAt() (string, error) {
	var newest time.Time
	err := s.dbRO.QueryRow(
		`SELECT updated_at FROM sessions ORDER BY updated_at DESC LIMIT 1`,
	).Scan(&newest)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return formatSummaryTime(newest), nil
}

// SessionCountOfPrincipal is how many sessions were started as one principal;
// PrincipalID ” counts the sessions started as no principal.
type SessionCountOfPrincipal struct {
	PrincipalID  string
	SessionCount int
}

// SessionCountsByPrincipal counts sessions by the principal they were started
// as, most sessions first. ownedByPrincipalID and throughPeerPrincipalID narrow
// it as they narrow the summary (SessionSummaryFilter); both empty counts every
// session.
func (s *Store) SessionCountsByPrincipal(ownedByPrincipalID, throughPeerPrincipalID string) ([]SessionCountOfPrincipal, error) {
	query := `SELECT principal_id, COUNT(*) FROM sessions`
	var conditions []string
	var args queryArguments
	if ownedByPrincipalID != "" {
		conditions = append(conditions, `principal_id = `+args.add(ownedByPrincipalID))
	}
	if throughPeerPrincipalID != "" {
		conditions = append(conditions, summarySessionIDExpression+` IN (SELECT session_id FROM session_peer_origins WHERE peer_principal_id = `+args.add(throughPeerPrincipalID)+`)`)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` GROUP BY 1 ORDER BY 2 DESC, 1`
	rows, err := s.dbRO.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("count sessions by principal: %w", err)
	}
	defer rows.Close()
	var counts []SessionCountOfPrincipal
	for rows.Next() {
		var count SessionCountOfPrincipal
		if err := rows.Scan(&count.PrincipalID, &count.SessionCount); err != nil {
			return nil, fmt.Errorf("read a session count: %w", err)
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}
