package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A login attempt is one sign-in through an outside identity provider (Entra
// ID today): written when the browser is sent to the provider, consumed once
// when it comes back, and then stamped with how it ended. The table is the
// audit of sign-in attempts, refused ones included — an administrator reads
// the tenant and object id of an unprovisioned identity here in order to link
// it.
//
// The state is kept only as a hash, since it is what the callback presents to
// find the row. The nonce is kept as a hash too, because the callback only has
// to compare it. The PKCE code verifier has to be sent to the token endpoint,
// so it is kept as it is until the attempt is consumed and then blanked.

// ErrLoginAttemptNotFound is a state hash or id no row has.
var ErrLoginAttemptNotFound = errors.New("login attempt not found")

// ErrLoginAttemptAlreadyConsumed is a second callback for one attempt: a
// replayed or double-submitted state.
var ErrLoginAttemptAlreadyConsumed = errors.New("login attempt was already used")

const loginAttemptIDPrefix = "login_attempt_"

// LoginAttemptOutcomeSignedIn is the outcome of an attempt that created a
// login session. Every other outcome is a refusal, named by the server.
const LoginAttemptOutcomeSignedIn = "signed_in"

// LoginAttempt is one row of login_attempts. The state hash, nonce hash and
// verifier never leave the server.
type LoginAttempt struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	StateHash    string `json:"-"`
	NonceHash    string `json:"-"`
	CodeVerifier string `json:"-"`
	// BrowserBindingHash is the hash of the value put in a cookie on the
	// browser that started the attempt; the callback must come from that
	// browser, so a link to someone else's callback cannot sign a victim in.
	BrowserBindingHash string     `json:"-"`
	ReturnTo           string     `json:"return_to"`
	CreatedAt          time.Time  `json:"created_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
	// Outcome is empty until the attempt ends, then signed_in or the code of
	// the refusal.
	Outcome       string `json:"outcome"`
	OutcomeDetail string `json:"outcome_detail,omitempty"`
	// TenantID and Subject are the tid and oid claims of a verified ID token;
	// PreferredUsername and DisplayName are for an administrator to recognise
	// the person by, and are never matched on.
	TenantID          string `json:"tenant_id,omitempty"`
	Subject           string `json:"subject,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	IdentityBindingID string `json:"identity_binding_id,omitempty"`
	PrincipalID       string `json:"principal_id,omitempty"`
	LoginSessionID    string `json:"login_session_id,omitempty"`
	UserAgent         string `json:"user_agent"`
	RemoteAddress     string `json:"remote_address"`
}

// LoginAttemptEnd is what is written when an attempt ends.
type LoginAttemptEnd struct {
	Outcome           string
	OutcomeDetail     string
	TenantID          string
	Subject           string
	PreferredUsername string
	DisplayName       string
	IdentityBindingID string
	PrincipalID       string
	LoginSessionID    string
}

// migrateLoginAttempts creates login_attempts. Called from migrate().
func (s *Store) migrateLoginAttempts() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS login_attempts (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			provider             TEXT NOT NULL,
			state_hash           TEXT NOT NULL UNIQUE,
			nonce_hash           TEXT NOT NULL,
			code_verifier        TEXT NOT NULL,
			browser_binding_hash TEXT NOT NULL,
			return_to            TEXT NOT NULL,
			created_at           DATETIME NOT NULL,
			expires_at           DATETIME NOT NULL,
			consumed_at          DATETIME,
			outcome              TEXT NOT NULL DEFAULT '',
			outcome_detail       TEXT NOT NULL DEFAULT '',
			tenant_id            TEXT NOT NULL DEFAULT '',
			subject              TEXT NOT NULL DEFAULT '',
			preferred_username   TEXT NOT NULL DEFAULT '',
			display_name         TEXT NOT NULL DEFAULT '',
			identity_binding_id  TEXT NOT NULL DEFAULT '',
			principal_id         TEXT NOT NULL DEFAULT '',
			login_session_id     TEXT NOT NULL DEFAULT '',
			user_agent           TEXT NOT NULL DEFAULT '',
			remote_address       TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_login_attempts_outcome ON login_attempts(outcome, id);
	`)
	return err
}

// InsertLoginAttempt records a started attempt and sets its ID.
func (s *Store) InsertLoginAttempt(attempt *LoginAttempt) error {
	if attempt.Provider == "" || attempt.StateHash == "" || attempt.NonceHash == "" || attempt.CodeVerifier == "" || attempt.BrowserBindingHash == "" {
		return fmt.Errorf("insert login attempt: provider, state hash, nonce hash, code verifier and browser binding are all required")
	}
	result, err := s.db.Exec(
		`INSERT INTO login_attempts (provider, state_hash, nonce_hash, code_verifier, browser_binding_hash, return_to, created_at, expires_at, user_agent, remote_address)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.Provider, attempt.StateHash, attempt.NonceHash, attempt.CodeVerifier, attempt.BrowserBindingHash, attempt.ReturnTo,
		attempt.CreatedAt.UTC(), attempt.ExpiresAt.UTC(), attempt.UserAgent, attempt.RemoteAddress,
	)
	if err != nil {
		return fmt.Errorf("insert login attempt: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert login attempt: read its id: %w", err)
	}
	attempt.ID = loginAttemptIDOf(rowID)
	return nil
}

// ConsumeLoginAttemptByStateHash marks the attempt with this state hash used
// and returns it as it was before, verifier included. Only the first caller
// wins: a second is ErrLoginAttemptAlreadyConsumed, with the attempt as it
// now is. The verifier is blanked in the row, since nothing may use it again.
// Expiry is the caller's to judge from ExpiresAt.
func (s *Store) ConsumeLoginAttemptByStateHash(stateHash string, consumedAt time.Time) (LoginAttempt, error) {
	attempt, err := scanLoginAttempt(s.db.QueryRow(loginAttemptSelect+` WHERE state_hash = ?`, stateHash))
	if errors.Is(err, sql.ErrNoRows) {
		return LoginAttempt{}, ErrLoginAttemptNotFound
	}
	if err != nil {
		return LoginAttempt{}, err
	}
	rowID, _ := loginAttemptRowIDOf(attempt.ID)
	result, err := s.db.Exec(`UPDATE login_attempts SET consumed_at = ?, code_verifier = '' WHERE id = ? AND consumed_at IS NULL`,
		consumedAt.UTC(), rowID)
	if err != nil {
		return LoginAttempt{}, fmt.Errorf("consume login attempt %s: %w", attempt.ID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LoginAttempt{}, fmt.Errorf("consume login attempt %s: %w", attempt.ID, err)
	}
	if changed == 0 {
		current, err := s.GetLoginAttempt(attempt.ID)
		if err != nil {
			return LoginAttempt{}, err
		}
		return current, ErrLoginAttemptAlreadyConsumed
	}
	consumed := consumedAt.UTC()
	attempt.ConsumedAt = &consumed
	return attempt, nil
}

// FinishLoginAttempt writes how an attempt ended. It is written once: an
// attempt that already has an outcome is left as it is and the call fails, so
// a later code path cannot overwrite the audit of the first.
func (s *Store) FinishLoginAttempt(id string, end LoginAttemptEnd) (LoginAttempt, error) {
	rowID, ok := loginAttemptRowIDOf(id)
	if !ok {
		return LoginAttempt{}, ErrLoginAttemptNotFound
	}
	if end.Outcome == "" {
		return LoginAttempt{}, fmt.Errorf("finish login attempt %s: the outcome is empty", id)
	}
	result, err := s.db.Exec(
		`UPDATE login_attempts SET outcome = ?, outcome_detail = ?, tenant_id = ?, subject = ?, preferred_username = ?, display_name = ?,
		 identity_binding_id = ?, principal_id = ?, login_session_id = ? WHERE id = ? AND outcome = ''`,
		end.Outcome, end.OutcomeDetail, end.TenantID, end.Subject, end.PreferredUsername, end.DisplayName,
		end.IdentityBindingID, end.PrincipalID, end.LoginSessionID, rowID)
	if err != nil {
		return LoginAttempt{}, fmt.Errorf("finish login attempt %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LoginAttempt{}, fmt.Errorf("finish login attempt %s: %w", id, err)
	}
	if changed == 0 {
		return LoginAttempt{}, fmt.Errorf("finish login attempt %s as %s: it is missing or already has an outcome", id, end.Outcome)
	}
	return s.GetLoginAttempt(id)
}

// GetLoginAttempt returns one row by its id.
func (s *Store) GetLoginAttempt(id string) (LoginAttempt, error) {
	rowID, ok := loginAttemptRowIDOf(id)
	if !ok {
		return LoginAttempt{}, ErrLoginAttemptNotFound
	}
	attempt, err := scanLoginAttempt(s.db.QueryRow(loginAttemptSelect+` WHERE id = ?`, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return LoginAttempt{}, ErrLoginAttemptNotFound
	}
	return attempt, err
}

// ListLoginAttempts returns attempts newest first, only those with the given
// outcome when outcome is not empty, at most limit of them.
func (s *Store) ListLoginAttempts(outcome string, limit int) ([]LoginAttempt, error) {
	query := loginAttemptSelect
	var arguments []any
	if outcome != "" {
		query += ` WHERE outcome = ?`
		arguments = append(arguments, outcome)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	arguments = append(arguments, limit)
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []LoginAttempt{}
	for rows.Next() {
		attempt, err := scanLoginAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

const loginAttemptSelect = `SELECT id, provider, state_hash, nonce_hash, code_verifier, browser_binding_hash, return_to, created_at, expires_at,
	consumed_at, outcome, outcome_detail, tenant_id, subject, preferred_username, display_name, identity_binding_id, principal_id,
	login_session_id, user_agent, remote_address FROM login_attempts`

func loginAttemptIDOf(rowID int64) string {
	return fmt.Sprintf("%s%06d", loginAttemptIDPrefix, rowID)
}

func loginAttemptRowIDOf(id string) (int64, bool) {
	digits, hasPrefix := strings.CutPrefix(id, loginAttemptIDPrefix)
	if !hasPrefix {
		return 0, false
	}
	rowID, err := strconv.ParseInt(digits, 10, 64)
	return rowID, err == nil && rowID > 0
}

func scanLoginAttempt(scanner rowScanner) (LoginAttempt, error) {
	var attempt LoginAttempt
	var rowID int64
	var consumedAt sql.NullTime
	if err := scanner.Scan(&rowID, &attempt.Provider, &attempt.StateHash, &attempt.NonceHash, &attempt.CodeVerifier, &attempt.BrowserBindingHash,
		&attempt.ReturnTo, &attempt.CreatedAt, &attempt.ExpiresAt, &consumedAt, &attempt.Outcome, &attempt.OutcomeDetail,
		&attempt.TenantID, &attempt.Subject, &attempt.PreferredUsername, &attempt.DisplayName, &attempt.IdentityBindingID,
		&attempt.PrincipalID, &attempt.LoginSessionID, &attempt.UserAgent, &attempt.RemoteAddress); err != nil {
		return LoginAttempt{}, err
	}
	attempt.ID = loginAttemptIDOf(rowID)
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.ExpiresAt = attempt.ExpiresAt.UTC()
	if consumedAt.Valid {
		consumed := consumedAt.Time.UTC()
		attempt.ConsumedAt = &consumed
	}
	return attempt, nil
}
