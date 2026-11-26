package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge/msg"
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

	// OnSessionBriefChanged fires when a session's brief or one of its loose
	// ends moves: a turn updated it, or a person answered or dismissed a
	// loose end.
	OnSessionBriefChanged(bridgeID string)
}

// Store is the bridge's own records in Postgres, in the module Module names:
// sessions and what is kept about them, sign-in, peers, settings and the
// machines' readings. Session events are not here; the session log keeps
// them (internal/sessionlog).
type Store struct {
	// db runs every write, on one connection. The store was written for
	// SQLite's single writer, and several writes read a row and then change
	// it (a renamer slot, a discovered session's fields, a delivery claim)
	// trusting that no other write lands between the two.
	db *sql.DB
	// dbRO runs reads, on a pool whose transactions are read only.
	dbRO     *sql.DB
	notifier Notifier
	// serverID is this server's own id, read from server_identity at open.
	serverID string
}

// ModuleSchema is the schema the bridge's own records live in. The session
// log names it in the statements that write a session row or a runner spawn
// in the same transaction as their event (see GrantSessionLogWrites).
const ModuleSchema = "bridge_core"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the bridge keeps its own records in,
// with the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: ModuleSchema, Role: "bridge_core_module", Migrations: migrations}, nil
}

// readerPoolSize is how many reads run at once, as the SQLite reader pool did.
const readerPoolSize = 8

// OpenReader opens the module's read-only pool on the Postgres at url.
func OpenReader(ctx context.Context, url string, module postgresstorage.Module) (*sql.DB, error) {
	readOnlyURL, err := withRuntimeParameter(url, "default_transaction_read_only", "on")
	if err != nil {
		return nil, err
	}
	return postgresstorage.Open(ctx, readOnlyURL, module, postgresstorage.ScanTimestampsInUTC)
}

// withRuntimeParameter adds a session setting to a Postgres connection
// string, in either of its two forms.
func withRuntimeParameter(connectionString, name, value string) (string, error) {
	if strings.Contains(connectionString, "://") {
		separator := "?"
		if strings.Contains(connectionString, "?") {
			separator = "&"
		}
		return connectionString + separator + name + "=" + value, nil
	}
	if strings.TrimSpace(connectionString) == "" {
		return "", fmt.Errorf("store: the Postgres connection string is empty")
	}
	return connectionString + " " + name + "=" + value, nil
}

// New builds the store over the module's pools: writer, provisioned and
// migrated and opened with postgresstorage.ScanTimestampsInUTC, and reader,
// from OpenReader. It makes this server's id the first time, and marks the
// linked subagents a previous version left unmarked. The store closes both
// pools in Close.
func New(writer, reader *sql.DB) (*Store, error) {
	writer.SetMaxOpenConns(1)
	reader.SetMaxOpenConns(readerPoolSize)
	reader.SetMaxIdleConns(4)
	s := &Store{db: writer, dbRO: reader}
	if err := s.openServerIdentity(); err != nil {
		return nil, fmt.Errorf("server identity: %w", err)
	}
	// A subagent the bridge knows the parent of runs inside that parent's
	// process, so the bridge must never resume it, send to it or offer a
	// button on it — and controlled_by='harness' is the flag every one of
	// those gates reads. Idempotent: it fills only an empty flag.
	if _, err := s.db.Exec(markLinkedSubagentsHarnessControlled); err != nil {
		return nil, fmt.Errorf("mark linked subagents controlled_by=harness: %w", err)
	}
	return s, nil
}

// SetNotifier registers a callback fired after successful session-row
// mutations. Pass nil to clear. Replays the new state on UpsertDiscovered too.
func (s *Store) SetNotifier(n Notifier) { s.notifier = n }

// NotifySessionChanged tells the notifier a session row changed. The session
// log calls it after it writes a status onto the row, in the transaction of
// the status's event.
func (s *Store) NotifySessionChanged(bridgeID string) { s.notifyChanged(bridgeID) }

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

func (s *Store) notifySessionBriefChanged(bridgeID string) {
	if s.notifier != nil && bridgeID != "" {
		s.notifier.OnSessionBriefChanged(bridgeID)
	}
}

func (s *Store) notifyDeleted(bridgeID string) {
	if s.notifier != nil && bridgeID != "" {
		s.notifier.OnSessionDeleted(bridgeID)
	}
}

func (s *Store) Close() error {
	readerErr := s.dbRO.Close()
	if err := s.db.Close(); err != nil {
		return err
	}
	return readerErr
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
			`INSERT INTO folders (name, position) VALUES ($1, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
			sess.FolderName,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO sessions (bridge_id, session_id, harness_session_id, display_name, harness, instance_id, state, pid, agent_id, principal_id, bundle_id, card_id, parent_id, forked_from_session_id, manager_session_id, root_session_id, depth, controlled_by, refreshed_from_session_id, working_dir, harness_config, purpose, type, origin, folder_name, mode, max_budget_usd, spend_usd, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)`,
		sess.SessionID, sess.SessionID, sess.HarnessSessionID, sess.DisplayName, sess.Harness, sess.InstanceID, sess.State, sess.PID, sess.AgentID, sess.PrincipalID, sess.BundleID, sess.CardID, sess.ParentID, sess.ForkedFromSessionID, sess.ManagerSessionID, sess.RootSessionID, sess.Depth, sess.ControlledBy, sess.RefreshedFromSessionID, sess.WorkingDir, harnessConfig, sess.Purpose, string(sess.Type), sess.Origin, sess.FolderName, string(sess.Mode), sess.MaxBudgetUSD, sess.SpendUSD, sess.CreatedAt, sess.UpdatedAt,
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
const sessionColumns = `COALESCE(NULLIF(session_id, ''), bridge_id), COALESCE(harness_session_id, ''), display_name, harness, COALESCE(instance_id, ''), state, pid, agent_id, COALESCE(principal_id, ''), COALESCE(bundle_id, ''), COALESCE(card_id, ''), parent_id, COALESCE(forked_from_session_id, ''), COALESCE(manager_session_id, ''), COALESCE(root_session_id, ''), COALESCE(depth, 0), COALESCE(controlled_by, ''), COALESCE(refreshed_from_session_id, ''), COALESCE(working_dir, ''), COALESCE(harness_config, ''), COALESCE(info, ''), COALESCE(folder_name, ''), COALESCE(purpose, ''), COALESCE(type, ''), COALESCE(origin, ''), COALESCE(mode, ''), COALESCE(max_budget_usd, 0), COALESCE(spend_usd, 0), COALESCE(status, ''), created_at, updated_at`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var harnessConfig string
	var info string
	var mode string
	var sessionType string
	var status string
	err := sc.Scan(&sess.SessionID, &sess.HarnessSessionID, &sess.DisplayName, &sess.Harness, &sess.InstanceID, &sess.State, &sess.PID, &sess.AgentID, &sess.PrincipalID, &sess.BundleID, &sess.CardID, &sess.ParentID, &sess.ForkedFromSessionID, &sess.ManagerSessionID, &sess.RootSessionID, &sess.Depth, &sess.ControlledBy, &sess.RefreshedFromSessionID, &sess.WorkingDir, &harnessConfig, &info, &sess.FolderName, &sess.Purpose, &sessionType, &sess.Origin, &mode, &sess.MaxBudgetUSD, &sess.SpendUSD, &status, &sess.CreatedAt, &sess.UpdatedAt)
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
	sess.Status = sessionStatusFromColumns(sess.State, status)
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
	_, err := s.db.Exec(`UPDATE sessions SET info=$1, updated_at=$2 WHERE bridge_id=$3`, payload, now, bridgeID)
	return err
}

// GetSession looks up a session by bridge_id.
func (s *Store) GetSession(bridgeID string) (*Session, error) {
	return scanSession(s.dbRO.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE bridge_id=$1`, bridgeID,
	))
}

// GetSessionByHarnessSessionID looks up a session by its harness-reported session ID.
func (s *Store) GetSessionByHarnessSessionID(harnessSessionID string) (*Session, error) {
	return scanSession(s.dbRO.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE harness_session_id=$1`, harnessSessionID,
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
	var args queryArguments
	if limit > 0 {
		query += ` LIMIT ` + args.add(limit) + ` OFFSET ` + args.add(offset)
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

// ListSessionsOwnedByPrincipalPaged is ListSessionsPaged, or
// ListSessionsByStatePaged when state is non-empty, narrowed to the sessions
// whose principal_id is principalID. A session with no principal is owned by
// nobody and never listed here. principalID must be non-empty: an empty one
// would match exactly the unowned sessions, the opposite of what a caller
// narrowing to one principal wants.
//
// throughPeerPrincipalID, when set, narrows further to the sessions a
// connected server started through that peer principal for one of its users
// (session_peer_origins): what that server's user may reach.
func (s *Store) ListSessionsOwnedByPrincipalPaged(principalID, throughPeerPrincipalID, state string, limit, offset int) ([]Session, error) {
	if principalID == "" {
		return nil, fmt.Errorf("ListSessionsOwnedByPrincipalPaged: principal id is required")
	}
	var args queryArguments
	query := `SELECT ` + sessionColumns + ` FROM sessions WHERE principal_id=` + args.add(principalID)
	if throughPeerPrincipalID != "" {
		query += ` AND COALESCE(NULLIF(session_id, ''), bridge_id) IN (SELECT session_id FROM session_peer_origins WHERE peer_principal_id = ` + args.add(throughPeerPrincipalID) + `)`
	}
	if state != "" {
		query += ` AND state=` + args.add(state)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ` + args.add(limit) + ` OFFSET ` + args.add(offset)
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

// SessionIDsOwnedByPrincipal returns the subset of sessionIDs whose session
// has principal_id principalID, in no particular order. Ids that name no
// session, or a session owned by someone else or by nobody, are dropped.
// throughPeerPrincipalID narrows as ListSessionsOwnedByPrincipalPaged does.
func (s *Store) SessionIDsOwnedByPrincipal(principalID, throughPeerPrincipalID string, sessionIDs []string) ([]string, error) {
	if principalID == "" {
		return nil, fmt.Errorf("SessionIDsOwnedByPrincipal: principal id is required")
	}
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	var args queryArguments
	query := `SELECT bridge_id FROM sessions WHERE principal_id=` + args.add(principalID) + ` AND bridge_id = ANY(` + args.add(sessionIDs) + `)`
	if throughPeerPrincipalID != "" {
		query += ` AND COALESCE(NULLIF(session_id, ''), bridge_id) IN (SELECT session_id FROM session_peer_origins WHERE peer_principal_id = ` + args.add(throughPeerPrincipalID) + `)`
	}
	rows, err := s.dbRO.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owned := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		owned = append(owned, id)
	}
	return owned, rows.Err()
}

// CountSessionsInStates counts the sessions in any of the states. No state
// counts nothing.
func (s *Store) CountSessionsInStates(states []string) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	var count int
	if err := s.dbRO.QueryRow(`SELECT COUNT(*) FROM sessions WHERE state = ANY($1)`, states).Scan(&count); err != nil {
		return 0, fmt.Errorf("count sessions in states %v: %w", states, err)
	}
	return count, nil
}

func (s *Store) ListSessionsByState(state string) ([]Session, error) {
	return s.ListSessionsByStatePaged(state, 0, 0)
}

// ListSessionsByStatePaged is ListSessionsByState with the same limit/offset
// bounding semantics as ListSessionsPaged (limit <= 0 means unbounded).
func (s *Store) ListSessionsByStatePaged(state string, limit, offset int) ([]Session, error) {
	var args queryArguments
	query := `SELECT ` + sessionColumns + ` FROM sessions WHERE state=` + args.add(state) + ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ` + args.add(limit) + ` OFFSET ` + args.add(offset)
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

// sessionStatusFromColumns is the status a session row carries. A row last
// written before the status column existed has only its state, and says so
// with AsOf 0 — the oldest a status can be, so anything a client already holds
// for the session outranks it.
func sessionStatusFromColumns(state, statusJSON string) *msg.SessionStatus {
	if statusJSON != "" {
		var status msg.SessionStatus
		if err := json.Unmarshal([]byte(statusJSON), &status); err == nil {
			return &status
		}
	}
	return &msg.SessionStatus{State: msg.SessionState(state)}
}

// SetSessionMaxBudgetUSD writes the session's spend ceiling in US dollars.
// Zero clears the ceiling (see the warning on msg.ManagedSession.MaxBudgetUSD
// — zero is "unset", never "halt now"). Callers validate the sign; a
// negative ceiling would halt every session on its first API call, so it is
// rejected at the HTTP boundary rather than silently clamped here.
func (s *Store) SetSessionMaxBudgetUSD(bridgeID string, maxBudgetUSD float64) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE sessions SET max_budget_usd=$1, updated_at=$2 WHERE bridge_id=$3`, maxBudgetUSD, now, bridgeID)
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
// The session cost total is deliberately absent: it lives in the sessions.spend_usd REAL
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

	// The two inputs of the cost estimate in spend_usd (msg.SessionCostEvent),
	// session-cumulative. APISpendUSD is what api_spend_total continues from on a
	// resume; TurnResultUSD is the sum of per-turn result costs.
	APISpendUSD   float64 `json:"api_spend_usd"`
	TurnResultUSD float64 `json:"turn_result_usd"`
}

// RecordAPISpendBreakdown stores the session's per-call spend aggregate — token
// usage, call count, per-model and per-query-source dollars and the API total
// (api_spend_usd) — from the latest api_spend_total, keeping the recorded
// turn_result_usd. It never replaces a breakdown with one describing less API
// spend: a derivation seeded from a failed read restarts at zero, and its
// breakdown would erase earlier runs' attribution.
//
// It is one statement, one round trip: it runs for every api_call event a
// harness reports, on the path that streams the event.
func (s *Store) RecordAPISpendBreakdown(bridgeID string, detail SessionSpendDetail) error {
	// The recorded turn_result_usd is kept; the statement writes it over
	// whatever the new breakdown says.
	detail.TurnResultUSD = 0
	next, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshal spend detail for %s: %w", bridgeID, err)
	}
	var sessions, updated int
	err = s.db.QueryRow(`
		WITH current AS (
			SELECT COALESCE(NULLIF(api_spend_detail, ''), '{}')::jsonb AS detail FROM sessions WHERE bridge_id = $1
		), updated AS (
			UPDATE sessions
			   SET api_spend_detail = jsonb_set($2::jsonb, '{turn_result_usd}',
			           to_jsonb(COALESCE((SELECT (detail ->> 'turn_result_usd')::double precision FROM current), 0)))::text,
			       updated_at = $3
			 WHERE bridge_id = $1
			   AND $4::double precision >= COALESCE((SELECT (detail ->> 'api_spend_usd')::double precision FROM current), 0)
			RETURNING 1
		)
		SELECT (SELECT COUNT(*) FROM current), (SELECT COUNT(*) FROM updated)`,
		bridgeID, string(next), time.Now().UTC(), detail.APISpendUSD).Scan(&sessions, &updated)
	if err != nil {
		return err
	}
	if sessions == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordSessionCostReadingCeiling raises the session's recorded cost
// (spend_usd) to totalUSD and its per-turn result total to turnResultUSD, and
// returns the cost now stored, which is never lower than what was there, and
// the session's spend ceiling (max_budget_usd, 0 for none), in one statement:
// the budget check runs on every session_cost event, on the path that streams
// it.
//
// spend_usd is the session cost estimate (msg.SessionCostEvent), the figure the
// spend ceiling compares against. The GREATEST() is a backstop, not the
// mechanism: the derivation seeds each harness process from the persisted row,
// and it covers a failed seed read, which starts the estimate at zero.
func (s *Store) RecordSessionCostReadingCeiling(bridgeID string, totalUSD, turnResultUSD float64) (spendUSD, maxBudgetUSD float64, err error) {
	now := time.Now().UTC()
	err = s.db.QueryRow(`UPDATE sessions
		SET spend_usd        = GREATEST(spend_usd, $1),
		    api_spend_detail = CASE
		        WHEN $2::double precision > COALESCE((COALESCE(NULLIF(api_spend_detail, ''), '{}')::jsonb ->> 'turn_result_usd')::double precision, 0)
		        THEN jsonb_set(COALESCE(NULLIF(api_spend_detail, ''), '{}')::jsonb, '{turn_result_usd}', to_jsonb($2::double precision))::text
		        ELSE api_spend_detail END,
		    updated_at       = $3
		WHERE bridge_id = $4
		RETURNING spend_usd, max_budget_usd`, totalUSD, turnResultUSD, now, bridgeID).Scan(&spendUSD, &maxBudgetUSD)
	if err != nil {
		return 0, 0, err
	}
	s.notifyChanged(bridgeID)
	return spendUSD, maxBudgetUSD, nil
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
		FROM sessions WHERE bridge_id=$1`, bridgeID).Scan(&totalUSD, &encoded)
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
	res, err := s.db.Exec(`UPDATE sessions SET pid=$1, updated_at=$2 WHERE bridge_id=$3`, pid, now, bridgeID)
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
	res, err := s.db.Exec(`UPDATE sessions SET mode=$1, updated_at=$2 WHERE bridge_id=$3`, string(mode), now, bridgeID)
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
	res, err := s.db.Exec(`UPDATE sessions SET harness_config=$1, updated_at=$2 WHERE bridge_id=$3`, payload, now, bridgeID)
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

// ReconciledSession is a session ReconcileSessions reset, and the idle
// session_status event it stored for it, carrying that row's id as AsOf.
// The event is returned so the caller can hand the same bytes to log-store:
// the store has no log-store client, and until 2026-09-25 this status reached
// bridge.db only, one row per stale session on every restart.
type ReconciledSession struct {
	Session
	Status msg.Event
}

// SetHarnessSessionID fills in the harness-reported session ID on a session.
func (s *Store) SetHarnessSessionID(bridgeID, harnessSessionID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE sessions SET harness_session_id=$1, updated_at=$2 WHERE bridge_id=$3`, harnessSessionID, now, bridgeID)
	return err
}

// FillOrRepairDiscoveredSessionMetadata brings what a harness wrapper reports about a
// session on disk onto its row, where the row lacks it, and returns the names
// of the columns it changed (none when the row already had both).
//
//   - working_dir is filled while it is empty. UpsertDiscoveredSession never
//     wrote it, so until 2026-10-28 a discovered session had no working folder
//     unless something later set one: 82 of one machine's 119 discovered codex
//     sessions, although every rollout names its cwd.
//   - working_dir is also replaced when it holds the path the Claude Code
//     wrapper used to misread from a project directory name
//     (isClaudeCodeProjectNameMisreading). Until llm-bridge-claudecode 5539b12
//     the wrapper decoded every '-' in that name as '/', so a session started
//     in /work/llm-bridge-server was filled with /work/llm/bridge/server, a
//     folder that does not exist, and the session could not be started again.
//   - display_name is replaced while it is empty or is text codex injected as
//     a user message (msg.IsCodexInjectedText), and never when a person chose
//     it (display_name_autogenerated = 0, written by /rename) or the renamer
//     did (display_name_named_at_turn > 0). A displayName that is itself
//     injected text is never written.
//
// updated_at is left alone: correcting a label is not activity, and bumping it
// would reorder the sidebar by the time of a discovery pass. A second call
// with the same values changes nothing.
func (s *Store) FillOrRepairDiscoveredSessionMetadata(bridgeID, displayName, workingDir string) ([]string, error) {
	var existingDisplayName, existingWorkingDir string
	var autogenerated bool
	var namedAtTurn int
	err := s.db.QueryRow(
		`SELECT display_name, working_dir, display_name_autogenerated, display_name_named_at_turn FROM sessions WHERE bridge_id=$1`,
		bridgeID,
	).Scan(&existingDisplayName, &existingWorkingDir, &autogenerated, &namedAtTurn)
	if err != nil {
		return nil, fmt.Errorf("FillOrRepairDiscoveredSessionMetadata %s: %w", bridgeID, err)
	}

	var changed []string
	newWorkingDir := existingWorkingDir
	if workingDir != "" && (existingWorkingDir == "" || isClaudeCodeProjectNameMisreading(existingWorkingDir, workingDir)) {
		newWorkingDir = workingDir
		changed = append(changed, "working_dir")
	}
	newDisplayName := existingDisplayName
	titleChosen := !autogenerated || namedAtTurn > 0
	if !titleChosen && displayName != "" && !msg.IsCodexInjectedText(displayName) &&
		(existingDisplayName == "" || msg.IsCodexInjectedText(existingDisplayName)) && displayName != existingDisplayName {
		newDisplayName = displayName
		changed = append(changed, "display_name")
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if _, err := s.db.Exec(`UPDATE sessions SET display_name=$1, working_dir=$2 WHERE bridge_id=$3`, newDisplayName, newWorkingDir, bridgeID); err != nil {
		return nil, fmt.Errorf("FillOrRepairDiscoveredSessionMetadata %s: update: %w", bridgeID, err)
	}
	s.notifyChanged(bridgeID)
	return changed, nil
}

// isClaudeCodeProjectNameMisreading reports whether stored is the path the
// Claude Code wrapper produced, before llm-bridge-claudecode 5539b12, for a
// session whose real working directory is actual. Claude Code names a
// session's project directory by writing every character of the path but
// ASCII letters and digits as '-' (a character outside the Basic Multilingual
// Plane as "--"); the old wrapper read that name back by turning each '-' into
// '/'. Only a stored path equal to that misreading, and different from actual,
// counts, so a working directory someone set on purpose is never replaced.
func isClaudeCodeProjectNameMisreading(stored, actual string) bool {
	if stored == actual {
		return false
	}
	var projectDirectoryName strings.Builder
	for _, r := range actual {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			projectDirectoryName.WriteRune(r)
		case r > 0xFFFF:
			projectDirectoryName.WriteString("--")
		default:
			projectDirectoryName.WriteByte('-')
		}
	}
	name := projectDirectoryName.String()
	misreading := "/"
	if name != "" && name != "-" {
		misreading = "/" + strings.ReplaceAll(strings.TrimPrefix(name, "-"), "-", "/")
	}
	return stored == misreading
}

// SetDisplayNameIfEmpty sets display_name only when it is currently empty,
// and marks it autogenerated. Returns true if a row was updated.
func (s *Store) SetDisplayNameIfEmpty(bridgeID, displayName string) (bool, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE sessions SET display_name=$1, display_name_autogenerated=true, updated_at=$2 WHERE bridge_id=$3 AND display_name=''`,
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
		`UPDATE sessions SET display_name=$1, display_name_autogenerated=false, updated_at=$2 WHERE bridge_id=$3`,
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

// RenamerState returns the auto-rename bookkeeping for a session: whether the
// current display name is autogenerated, the user turn count at which the
// last auto-rename ran (0 = never), and the bridge_id of any in-flight
// renamer session ("" = none).
func (s *Store) RenamerState(bridgeID string) (autogenerated bool, namedAtTurn int, renamerSessionID string, err error) {
	err = s.db.QueryRow(
		`SELECT display_name_autogenerated, display_name_named_at_turn, renamer_session_id FROM sessions WHERE bridge_id=$1`,
		bridgeID,
	).Scan(&autogenerated, &namedAtTurn, &renamerSessionID)
	if err != nil {
		return false, 0, "", err
	}
	return autogenerated, namedAtTurn, renamerSessionID, nil
}

// ReserveRenamerSlot atomically claims the renamer slot on a session. Returns
// true on success — caller now owns the slot and must clear it (via
// ApplyAutoRename or ClearRenamerSlot) when the renamer terminates. Returns
// false if another renamer is already in flight.
func (s *Store) ReserveRenamerSlot(bridgeID, renamerSessionID string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET renamer_session_id=$1 WHERE bridge_id=$2 AND renamer_session_id=''`,
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
		`UPDATE sessions SET renamer_session_id='' WHERE bridge_id=$1`,
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
		   SET display_name=$1, display_name_autogenerated=true,
		       display_name_named_at_turn=$2, renamer_session_id='',
		       updated_at=$3
		 WHERE bridge_id=$4 AND renamer_session_id=$5`,
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

// EventWithID is a raw event with its database row ID.
type EventWithID struct {
	RowID int
	Data  json.RawMessage
}

// ToolUseBinding pairs a tool_use_id with the bridge and harness message ids
// of the bubble that contained it. Used to resolve task_progress events (which
// carry tool_use_id) back to their message bubble.
type ToolUseBinding struct {
	BridgeMessageID  string
	HarnessMessageID string
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

func (s *Store) DeleteSession(bridgeID string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE bridge_id=$1`, bridgeID)
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
		`INSERT INTO folders (name, position) VALUES ($1, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
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
	if _, err := tx.Exec(`UPDATE sessions SET folder_name='' WHERE folder_name=$1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM folders WHERE name=$1`, name); err != nil {
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
	if _, err := tx.Exec(`UPDATE sessions SET folder_name=$1 WHERE folder_name=$2`, newName, oldName); err != nil {
		return err
	}
	// Check whether newName already exists (merge case).
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM folders WHERE name=$1`, newName).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		if _, err := tx.Exec(`DELETE FROM folders WHERE name=$1`, oldName); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE folders SET name=$1 WHERE name=$2`, newName, oldName); err != nil {
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
		`INSERT INTO folders (name, position) VALUES ($1, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
		folder,
	); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT bridge_id FROM sessions WHERE folder_name='' AND updated_at < $1`,
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
		`UPDATE sessions SET folder_name=$1 WHERE folder_name='' AND updated_at < $2`,
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
// Sessions in any active state (msg.ActiveSessionStates) and sessions already
// in the Archive folder are left alone.
// Returns the bridge IDs that were moved. Auto-creates the Archive folder in
// the registry if it doesn't exist.
func (s *Store) ArchiveOld(cutoff time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO folders (name, position) VALUES ($1, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
		ArchiveFolder,
	); err != nil {
		return nil, err
	}
	// Every active state is spared, not only 'running': the derivation has
	// not written 'running' since it split into model_generating, tool_running
	// and the rest, so the old test spared nothing.
	activeStates := msg.ActiveSessionStates()
	activeStateNames := make([]string, len(activeStates))
	for index, state := range activeStates {
		activeStateNames[index] = string(state)
	}
	rows, err := tx.Query(`SELECT bridge_id FROM sessions WHERE folder_name != $1 AND updated_at < $2 AND NOT state = ANY($3)`,
		ArchiveFolder, cutoff, activeStateNames)
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
		`UPDATE sessions SET folder_name=$1 WHERE folder_name != $1 AND updated_at < $2 AND NOT state = ANY($3)`,
		ArchiveFolder, cutoff, activeStateNames,
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
			`INSERT INTO folders (name, position) VALUES ($1, COALESCE((SELECT MAX(position)+1 FROM folders), 0)) ON CONFLICT(name) DO NOTHING`,
			folder,
		); err != nil {
			return err
		}
	}
	res, err := tx.Exec(
		`UPDATE sessions SET folder_name=$1, updated_at=$2 WHERE bridge_id=$3`,
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
// (docs/plans/TEAM-ORCHESTRATION.md §21.4 step 4).
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
		// A subagent acts for the same principal as the session that spawned
		// it, from the same bundle; an ownerless child would vanish from its
		// principal's view.
		PrincipalID:  parent.PrincipalID,
		BundleID:     parent.BundleID,
		CardID:       parent.CardID,
		Depth:        parent.Depth + 1,
		ControlledBy: msg.ControlledByHarness,
		Purpose:      "subagent",
		Type:         msg.SessionTypeSystem,
		Origin:       "llm-bridge-" + strings.ReplaceAll(string(parent.Harness), "_", ""),
		FolderName:   folderName,
	}
	// A subagent of a session a connected server started for one of its
	// users stays limited by that server (peer_users.go).
	if _, err := s.CopySessionPeerOrigin(parent.SessionID, sub.SessionID); err != nil {
		return "", false, err
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
		   SET manager_session_id=$1, root_session_id=$2, depth=$3
		 WHERE bridge_id=$4
		   AND COALESCE(manager_session_id, '')=''`,
		parent.SessionID, root, parent.Depth+1, bridgeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	// A linked subagent runs inside its parent's process, so the bridge does
	// not control it. Run whether or not this call wrote the link: a row
	// linked before discovery set the flag gets it on the next pass.
	marked, err := s.db.Exec(markLinkedSubagentsHarnessControlled+` AND bridge_id = $1`, bridgeID)
	if err != nil {
		return n > 0, fmt.Errorf("mark %s controlled_by=harness: %w", bridgeID, err)
	}
	markedRows, err := marked.RowsAffected()
	if err != nil {
		return n > 0, err
	}
	if n > 0 || markedRows > 0 {
		s.notifyChanged(bridgeID)
	}
	return n > 0, nil
}

// markLinkedSubagentsHarnessControlled sets controlled_by='harness' on every
// subagent row linked to a parent that does not say so already. The migration
// runs it over the table, and LinkDiscoveredSessionParent over the one row it
// links, so a discovered subagent is gated from the pass that links it.
const markLinkedSubagentsHarnessControlled = `
	UPDATE sessions SET controlled_by = 'harness'
	 WHERE purpose IN ('subagent','workflow-subagent')
	   AND COALESCE(manager_session_id, '') != ''
	   AND COALESCE(controlled_by, '') = ''`

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
	return s.UpsertDiscoveredSessionOwnedBy(harnessSessionID, bridgeSessionID, displayName, harness, instanceID, source, folderName, createdAt, updatedAt, "")
}

// UpsertDiscoveredSessionOwnedBy assigns the explicitly configured owner only
// to new or previously unowned imports, and includes it in the first SSE frame.
func (s *Store) UpsertDiscoveredSessionOwnedBy(harnessSessionID, bridgeSessionID, displayName, harness, instanceID, source, folderName string, createdAt, updatedAt time.Time, principalID string) (string, bool, error) {
	if strings.HasPrefix(harnessSessionID, "br_") {
		return "", false, fmt.Errorf("UpsertDiscoveredSession: harness_session_id %q has bridge_id prefix — harness bridge is emitting a bridge_session_id in the harness slot (contract violation)", harnessSessionID)
	}
	// Postgres keeps a time to the microsecond. A file time with nanoseconds
	// would never equal the stored one, and every pass would write the row
	// again.
	createdAt = createdAt.Truncate(time.Microsecond)
	updatedAt = updatedAt.Truncate(time.Microsecond)

	// Short-circuit: if the harness bridge reports a bridge_session_id that
	// bridge-server already owns, the session is already adopted; don't
	// re-discover it.
	if bridgeSessionID != "" {
		var bridgeID string
		err := s.db.QueryRow(`SELECT bridge_id FROM sessions WHERE bridge_id=$1`, bridgeSessionID).Scan(&bridgeID)
		if err == nil {
			return bridgeID, false, nil
		}
		if err != sql.ErrNoRows {
			return "", false, err
		}
	}

	// Check if session already exists by harness_session_id
	var existingBridgeID, existingInstanceID, existingDisplayName, existingSource, existingFolder, existingPrincipalID string
	var existingUpdatedAt time.Time
	err := s.db.QueryRow(`SELECT bridge_id, COALESCE(instance_id, ''), COALESCE(display_name, ''), COALESCE(purpose, ''), COALESCE(folder_name, ''), updated_at, COALESCE(principal_id, '') FROM sessions WHERE harness_session_id=$1`, harnessSessionID).Scan(&existingBridgeID, &existingInstanceID, &existingDisplayName, &existingSource, &existingFolder, &existingUpdatedAt, &existingPrincipalID)
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
		newPrincipalID := existingPrincipalID
		if newPrincipalID == "" {
			newPrincipalID = principalID
		}
		// Nothing to write, nothing to announce. The startup discovery pass
		// brings every session on disk through here; until 2026-09-17 each one
		// was an UPDATE and an upsert frame to every open /session-events
		// stream whether or not anything differed — tens of thousands of
		// frames per restart, over rows that had not changed in days.
		if updatedAt.Equal(existingUpdatedAt) && newInstanceID == existingInstanceID && newDisplayName == existingDisplayName && newSource == existingSource && newFolder == existingFolder && newPrincipalID == existingPrincipalID {
			return existingBridgeID, false, nil
		}
		if _, err := s.db.Exec(`UPDATE sessions SET updated_at=$1, instance_id=$2, display_name=$3, purpose=$4, folder_name=$5, principal_id=$6 WHERE bridge_id=$7`, updatedAt, newInstanceID, newDisplayName, newSource, newFolder, newPrincipalID, existingBridgeID); err != nil {
			return "", false, fmt.Errorf("UpsertDiscoveredSession: update %s: %w", existingBridgeID, err)
		}
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
	// Where the adapter or a prompt prefix did recognize a purpose — a
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
		`INSERT INTO sessions (bridge_id, session_id, harness_session_id, display_name, harness, instance_id, state, pid, agent_id, parent_id, purpose, type, origin, folder_name, created_at, updated_at, principal_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		bridgeID, bridgeID, harnessSessionID, displayName, harness, instanceID, "idle", 0, "", "", source, string(sessionType), origin, folderName, createdAt, updatedAt, principalID,
	)
	if err != nil {
		return "", false, err
	}
	s.notifyChanged(bridgeID)
	return bridgeID, true, nil
}

// ReclassifyDiscoveredSession gives an existing discovered session the purpose
// its harness adapter now reports for it.
//
// UpsertDiscoveredSession classifies a session only when it inserts the row,
// so a purpose the adapter learned to recognize after the row was imported —
// oneshot transcripts, 51,002 of 51,258 discovered rows when it was added —
// never reached the rows that already existed. This carries it to them.
//
// It writes only over the generic label: the row changes only while its
// purpose is exactly "discovered", which is what the insert path writes when
// nothing recognized the session. A purpose anything else set is never
// overwritten, and a second call is a no-op. type follows the registry entry
// for the new purpose, as it does on insert.
//
// folder_name moves to folderName only while it still holds what the insert
// path filed an unrecognized session under — the registry folder for
// "discovered", or nothing. A session someone has since moved (on 2026-09-16,
// 33,145 discovered rows sat in "Archive") stays where they put it.
//
// updated_at is left alone on purpose: reclassification is not activity, and
// bumping it would reorder the sidebar by the time of a discovery pass.
//
// Returns whether the row changed. An unregistered purpose, or "discovered"
// itself, is a caller bug and returns an error rather than writing a label
// nothing can group.
func (s *Store) ReclassifyDiscoveredSession(bridgeID, purpose, folderName string) (bool, error) {
	if purpose == msg.PurposeDiscovered {
		return false, fmt.Errorf("ReclassifyDiscoveredSession %s: purpose %q is the label being replaced, not a reclassification", bridgeID, purpose)
	}
	spec, ok := msg.LookupPurpose(purpose)
	if !ok {
		return false, fmt.Errorf("ReclassifyDiscoveredSession %s: purpose %q is not in the registry", bridgeID, purpose)
	}
	res, err := s.db.Exec(
		`UPDATE sessions
		    SET purpose = $1,
		        type = $2,
		        folder_name = CASE WHEN COALESCE(folder_name, '') IN ('', $3) THEN $4 ELSE folder_name END
		  WHERE bridge_id = $5 AND purpose = $6`,
		purpose, string(spec.Type), msg.FolderForPurpose(msg.PurposeDiscovered), folderName, bridgeID, msg.PurposeDiscovered,
	)
	if err != nil {
		return false, fmt.Errorf("ReclassifyDiscoveredSession %s: %w", bridgeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ReclassifyDiscoveredSession %s: %w", bridgeID, err)
	}
	if n > 0 {
		s.notifyChanged(bridgeID)
	}
	return n > 0, nil
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
		`INSERT INTO source_folders (source, folder_name, updated_at) VALUES ($1,$2,$3)
		 ON CONFLICT(source) DO UPDATE SET folder_name=excluded.folder_name, updated_at=excluded.updated_at`,
		source, folderName, time.Now().UTC(),
	)
	return err
}

// DeleteSourceFolder removes a runtime override; the env default (if any)
// becomes effective again for this source.
func (s *Store) DeleteSourceFolder(source string) error {
	_, err := s.db.Exec(`DELETE FROM source_folders WHERE source=$1`, source)
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
		`UPDATE sessions SET folder_name=$1, updated_at=$2 WHERE purpose=$3 AND (folder_name='' OR folder_name=$4)`,
		newFolder, time.Now().UTC(), source, oldFolder,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
