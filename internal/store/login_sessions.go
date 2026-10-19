package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A login session is a person signed in to this server through a browser or
// a CLI: the record behind the llm_bridge_principal_session cookie. It is not
// a bridge session (a harness conversation), and it is not a session agent
// token or a session posting token, which are signed and hold no row.
//
// The cookie carries an opaque random token. Only its SHA-256 is kept here,
// so someone who reads this database cannot sign in as anyone. A row is never
// deleted: a revoked or expired one stays as the record of who signed in, how
// and from where.

// LoginSessionMethod says how a login session was signed in.
type LoginSessionMethod string

const (
	// LoginSessionMethodDemo is POST /auth/demo-login: a principal id, no
	// password.
	LoginSessionMethodDemo LoginSessionMethod = "demo"
	// LoginSessionMethodEntra is Microsoft Entra ID sign-in, matched to a
	// principal through a principal-store identity binding.
	LoginSessionMethodEntra LoginSessionMethod = "entra"
)

// ErrLoginSessionNotFound is a token hash or id no row has.
var ErrLoginSessionNotFound = errors.New("login session not found")

// ErrLoginSessionAlreadyRevoked is a revoke of a session that was revoked
// before. The first revocation's stamp and reason are kept.
var ErrLoginSessionAlreadyRevoked = errors.New("login session is already revoked")

const loginSessionIDPrefix = "login_session_"

// LoginSession is one row of login_sessions. The two hashes and the CSRF
// token never leave the server in a list: TokenHash and CSRFToken are tagged
// out of JSON, and a caller learns its own CSRF token only from the login
// response and GET /auth/principal.
type LoginSession struct {
	ID string `json:"id"`
	// TokenHash is the hex SHA-256 of the cookie's token.
	TokenHash string `json:"-"`
	// CSRFToken is what an unsafe request authenticated by this session must
	// carry in X-CSRF-Token. It is kept as it is, not hashed, because GET
	// /auth/principal hands it back to a page that reloaded and lost it; it
	// is worth nothing without the cookie, and the cookie's token is kept
	// only as a hash.
	CSRFToken         string             `json:"-"`
	PrincipalID       string             `json:"principal_id"`
	Method            LoginSessionMethod `json:"method"`
	IdentityBindingID string             `json:"identity_binding_id,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	ExpiresAt         time.Time          `json:"expires_at"`
	LastSeenAt        time.Time          `json:"last_seen_at"`
	RevokedAt         *time.Time         `json:"revoked_at,omitempty"`
	RevokedBy         string             `json:"revoked_by,omitempty"`
	RevokeReason      string             `json:"revoke_reason,omitempty"`
	UserAgent         string             `json:"user_agent"`
	RemoteAddress     string             `json:"remote_address"`
}

// migrateLoginSessions creates login_sessions. Called from migrate().
func (s *Store) migrateLoginSessions() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS login_sessions (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			token_hash          TEXT NOT NULL UNIQUE,
			csrf_token          TEXT NOT NULL,
			principal_id        TEXT NOT NULL,
			method              TEXT NOT NULL,
			identity_binding_id TEXT NOT NULL DEFAULT '',
			created_at          DATETIME NOT NULL,
			expires_at          DATETIME NOT NULL,
			last_seen_at        DATETIME NOT NULL,
			revoked_at          DATETIME,
			revoked_by          TEXT NOT NULL DEFAULT '',
			revoke_reason       TEXT NOT NULL DEFAULT '',
			user_agent          TEXT NOT NULL DEFAULT '',
			remote_address      TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_login_sessions_principal ON login_sessions(principal_id, id);
		CREATE INDEX IF NOT EXISTS idx_login_sessions_identity_binding ON login_sessions(identity_binding_id) WHERE identity_binding_id != '';
	`)
	return err
}

// InsertLoginSession records a new login session and sets its ID.
func (s *Store) InsertLoginSession(session *LoginSession) error {
	if session.TokenHash == "" || session.CSRFToken == "" || session.PrincipalID == "" || session.Method == "" {
		return fmt.Errorf("insert login session: token hash, CSRF token, principal and method are all required")
	}
	result, err := s.db.Exec(
		`INSERT INTO login_sessions (token_hash, csrf_token, principal_id, method, identity_binding_id, created_at, expires_at, last_seen_at, user_agent, remote_address)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.TokenHash, session.CSRFToken, session.PrincipalID, string(session.Method), session.IdentityBindingID,
		session.CreatedAt.UTC(), session.ExpiresAt.UTC(), session.LastSeenAt.UTC(), session.UserAgent, session.RemoteAddress,
	)
	if err != nil {
		return fmt.Errorf("insert login session for %s: %w", session.PrincipalID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert login session for %s: read its id: %w", session.PrincipalID, err)
	}
	session.ID = loginSessionIDOf(rowID)
	return nil
}

// GetLoginSessionByTokenHash returns the row whose cookie token hashes to
// tokenHash, whatever its state; the caller decides what expired and revoked
// mean.
func (s *Store) GetLoginSessionByTokenHash(tokenHash string) (LoginSession, error) {
	session, err := scanLoginSession(s.db.QueryRow(loginSessionSelect+` WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return LoginSession{}, ErrLoginSessionNotFound
	}
	return session, err
}

// GetLoginSession returns one row by its id.
func (s *Store) GetLoginSession(id string) (LoginSession, error) {
	rowID, ok := loginSessionRowIDOf(id)
	if !ok {
		return LoginSession{}, ErrLoginSessionNotFound
	}
	session, err := scanLoginSession(s.db.QueryRow(loginSessionSelect+` WHERE id = ?`, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return LoginSession{}, ErrLoginSessionNotFound
	}
	return session, err
}

// ListLoginSessionsOfPrincipal returns every login session of one principal,
// newest first, live or not.
func (s *Store) ListLoginSessionsOfPrincipal(principalID string) ([]LoginSession, error) {
	rows, err := s.db.Query(loginSessionSelect+` WHERE principal_id = ? ORDER BY id DESC`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []LoginSession{}
	for rows.Next() {
		session, err := scanLoginSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// TouchLoginSession sets last_seen_at. The caller decides how often: every
// request of a page load would otherwise be a write.
func (s *Store) TouchLoginSession(id string, seenAt time.Time) error {
	rowID, ok := loginSessionRowIDOf(id)
	if !ok {
		return ErrLoginSessionNotFound
	}
	_, err := s.db.Exec(`UPDATE login_sessions SET last_seen_at = ? WHERE id = ?`, seenAt.UTC(), rowID)
	return err
}

// RevokeLoginSession stamps one session revoked and returns it as it now is.
// A session revoked before is ErrLoginSessionAlreadyRevoked, with the row
// unchanged. An expired session can still be revoked, so the record says
// someone ended it on purpose.
func (s *Store) RevokeLoginSession(id, revokedBy, reason string, revokedAt time.Time) (LoginSession, error) {
	rowID, ok := loginSessionRowIDOf(id)
	if !ok {
		return LoginSession{}, ErrLoginSessionNotFound
	}
	result, err := s.db.Exec(
		`UPDATE login_sessions SET revoked_at = ?, revoked_by = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
		revokedAt.UTC(), revokedBy, reason, rowID)
	if err != nil {
		return LoginSession{}, fmt.Errorf("revoke login session %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LoginSession{}, fmt.Errorf("revoke login session %s: %w", id, err)
	}
	session, err := s.GetLoginSession(id)
	if err != nil {
		return LoginSession{}, err
	}
	if changed == 0 {
		return session, ErrLoginSessionAlreadyRevoked
	}
	return session, nil
}

// RevokeLiveLoginSessionsOfIdentityBinding revokes every session signed in
// through one identity binding that is neither revoked nor expired at
// revokedAt, and returns the ids it revoked.
func (s *Store) RevokeLiveLoginSessionsOfIdentityBinding(identityBindingID, revokedBy, reason string, revokedAt time.Time) ([]string, error) {
	if identityBindingID == "" {
		return nil, fmt.Errorf("revoke login sessions of an identity binding: the binding id is empty")
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	rows, err := transaction.Query(
		`SELECT id FROM login_sessions WHERE identity_binding_id = ? AND revoked_at IS NULL AND expires_at > ?`,
		identityBindingID, revokedAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("find login sessions of identity binding %s: %w", identityBindingID, err)
	}
	var rowIDs []int64
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			rows.Close()
			return nil, err
		}
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	revokedIDs := []string{}
	for _, rowID := range rowIDs {
		if _, err := transaction.Exec(
			`UPDATE login_sessions SET revoked_at = ?, revoked_by = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
			revokedAt.UTC(), revokedBy, reason, rowID); err != nil {
			return nil, fmt.Errorf("revoke login session %s: %w", loginSessionIDOf(rowID), err)
		}
		revokedIDs = append(revokedIDs, loginSessionIDOf(rowID))
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return revokedIDs, nil
}

const loginSessionSelect = `SELECT id, token_hash, csrf_token, principal_id, method, identity_binding_id, created_at, expires_at,
	last_seen_at, revoked_at, revoked_by, revoke_reason, user_agent, remote_address FROM login_sessions`

func loginSessionIDOf(rowID int64) string {
	return fmt.Sprintf("%s%06d", loginSessionIDPrefix, rowID)
}

func loginSessionRowIDOf(id string) (int64, bool) {
	digits, hasPrefix := strings.CutPrefix(id, loginSessionIDPrefix)
	if !hasPrefix {
		return 0, false
	}
	rowID, err := strconv.ParseInt(digits, 10, 64)
	return rowID, err == nil && rowID > 0
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLoginSession(scanner rowScanner) (LoginSession, error) {
	var session LoginSession
	var rowID int64
	var method string
	var revokedAt sql.NullTime
	if err := scanner.Scan(&rowID, &session.TokenHash, &session.CSRFToken, &session.PrincipalID, &method, &session.IdentityBindingID,
		&session.CreatedAt, &session.ExpiresAt, &session.LastSeenAt, &revokedAt, &session.RevokedBy, &session.RevokeReason,
		&session.UserAgent, &session.RemoteAddress); err != nil {
		return LoginSession{}, err
	}
	session.ID = loginSessionIDOf(rowID)
	session.Method = LoginSessionMethod(method)
	session.CreatedAt = session.CreatedAt.UTC()
	session.ExpiresAt = session.ExpiresAt.UTC()
	session.LastSeenAt = session.LastSeenAt.UTC()
	if revokedAt.Valid {
		revoked := revokedAt.Time.UTC()
		session.RevokedAt = &revoked
	}
	return session, nil
}
