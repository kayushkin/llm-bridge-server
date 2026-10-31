package store

// A service principal is a program on this host that calls this server as a
// principal of its own (principal-store's kind service), such as orchestratord.
//
// service_principal_tokens holds the tokens an administrator issued for one,
// each kept only as its SHA-256. A token is shown once, when it is issued, and
// a revoked one never works again.
//
// session_starters says which principal started a session that runs as
// another one: a service holding can_start_sessions_as on that principal. The
// starter may drive the session while the grant stands. A session a principal
// started as itself has no row; its owner is its starter.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const servicePrincipalTokenIDPrefix = "service_principal_token_"

// ErrServicePrincipalTokenNotFound is an id or hash with no token.
var ErrServicePrincipalTokenNotFound = errors.New("service principal token not found")

// ErrServicePrincipalTokenAlreadyRevoked is a revoke of a revoked token.
var ErrServicePrincipalTokenAlreadyRevoked = errors.New("service principal token is already revoked")

// ServicePrincipalToken is one row of service_principal_tokens, without the
// hash.
type ServicePrincipalToken struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	// Description says what holds the token, for whoever revokes it later.
	Description  string     `json:"description"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokedBy    string     `json:"revoked_by,omitempty"`
	RevokeReason string     `json:"revoke_reason,omitempty"`
}

// SessionStarter is one row of session_starters.
type SessionStarter struct {
	SessionID   string    `json:"session_id"`
	PrincipalID string    `json:"principal_id"`
	RecordedBy  string    `json:"recorded_by"`
	RecordedAt  time.Time `json:"recorded_at"`
	// Backfilled is set on a row an administrator wrote for a session created
	// before starters were recorded; the rest are written at create.
	Backfilled bool `json:"backfilled"`
}

// migrateServicePrincipals creates both tables. Called from migrate().
func (s *Store) migrateServicePrincipals() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS service_principal_tokens (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			principal_id  TEXT NOT NULL,
			description   TEXT NOT NULL,
			token_hash    TEXT NOT NULL UNIQUE,
			created_by    TEXT NOT NULL,
			created_at    DATETIME NOT NULL,
			last_used_at  DATETIME,
			revoked_at    DATETIME,
			revoked_by    TEXT NOT NULL DEFAULT '',
			revoke_reason TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS session_starters (
			session_id   TEXT PRIMARY KEY,
			principal_id TEXT NOT NULL,
			recorded_by  TEXT NOT NULL,
			recorded_at  DATETIME NOT NULL,
			backfilled   INTEGER NOT NULL DEFAULT 0
		);`)
	return err
}

const servicePrincipalTokenSelect = `SELECT id, principal_id, description, created_by, created_at, last_used_at, revoked_at, revoked_by, revoke_reason FROM service_principal_tokens`

func scanServicePrincipalToken(row rowScanner) (ServicePrincipalToken, error) {
	var token ServicePrincipalToken
	var rowID int64
	var lastUsedAt, revokedAt sql.NullTime
	if err := row.Scan(&rowID, &token.PrincipalID, &token.Description, &token.CreatedBy, &token.CreatedAt, &lastUsedAt, &revokedAt, &token.RevokedBy, &token.RevokeReason); err != nil {
		return ServicePrincipalToken{}, err
	}
	token.ID = prefixedIDOf(servicePrincipalTokenIDPrefix, rowID)
	token.LastUsedAt = timeOrNil(lastUsedAt)
	token.RevokedAt = timeOrNil(revokedAt)
	return token, nil
}

// InsertServicePrincipalToken records a token by its hash and sets the
// record's ID and CreatedAt.
func (s *Store) InsertServicePrincipalToken(token *ServicePrincipalToken, tokenHash string) error {
	if token.PrincipalID == "" || token.Description == "" || token.CreatedBy == "" || tokenHash == "" {
		return fmt.Errorf("insert service principal token: principal, description, creator and hash are all required")
	}
	token.CreatedAt = time.Now().UTC()
	result, err := s.db.Exec(`INSERT INTO service_principal_tokens (principal_id, description, token_hash, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		token.PrincipalID, token.Description, tokenHash, token.CreatedBy, token.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert service principal token: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	token.ID = prefixedIDOf(servicePrincipalTokenIDPrefix, rowID)
	return nil
}

// ServicePrincipalTokenByHash returns the token with this hash, revoked or not.
func (s *Store) ServicePrincipalTokenByHash(tokenHash string) (ServicePrincipalToken, error) {
	found, err := scanServicePrincipalToken(s.db.QueryRow(servicePrincipalTokenSelect+` WHERE token_hash = ?`, tokenHash))
	return notFoundAs(found, err, ErrServicePrincipalTokenNotFound)
}

// GetServicePrincipalToken returns one token by id.
func (s *Store) GetServicePrincipalToken(id string) (ServicePrincipalToken, error) {
	rowID, ok := rowIDOfPrefixed(servicePrincipalTokenIDPrefix, id)
	if !ok {
		return ServicePrincipalToken{}, ErrServicePrincipalTokenNotFound
	}
	found, err := scanServicePrincipalToken(s.db.QueryRow(servicePrincipalTokenSelect+` WHERE id = ?`, rowID))
	return notFoundAs(found, err, ErrServicePrincipalTokenNotFound)
}

// ListServicePrincipalTokens returns every token, newest first, revoked or not.
func (s *Store) ListServicePrincipalTokens() ([]ServicePrincipalToken, error) {
	rows, err := s.db.Query(servicePrincipalTokenSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []ServicePrincipalToken{}
	for rows.Next() {
		token, err := scanServicePrincipalToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// TouchServicePrincipalToken records that a token was used now, at most once
// a minute, so reading it on every request does not write on every request.
func (s *Store) TouchServicePrincipalToken(id string, now time.Time) error {
	rowID, ok := rowIDOfPrefixed(servicePrincipalTokenIDPrefix, id)
	if !ok {
		return ErrServicePrincipalTokenNotFound
	}
	_, err := s.db.Exec(`UPDATE service_principal_tokens SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		now.UTC(), rowID, now.UTC().Add(-time.Minute))
	return err
}

// RevokeServicePrincipalToken ends a live token.
func (s *Store) RevokeServicePrincipalToken(id, revokedBy, reason string) (ServicePrincipalToken, error) {
	if revokedBy == "" || reason == "" {
		return ServicePrincipalToken{}, fmt.Errorf("revoke service principal token: who revokes it and why are both required")
	}
	existing, err := s.GetServicePrincipalToken(id)
	if err != nil {
		return ServicePrincipalToken{}, err
	}
	if existing.RevokedAt != nil {
		return existing, ErrServicePrincipalTokenAlreadyRevoked
	}
	rowID, _ := rowIDOfPrefixed(servicePrincipalTokenIDPrefix, id)
	if _, err := s.db.Exec(`UPDATE service_principal_tokens SET revoked_at = ?, revoked_by = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), revokedBy, reason, rowID); err != nil {
		return ServicePrincipalToken{}, fmt.Errorf("revoke service principal token %s: %w", id, err)
	}
	return s.GetServicePrincipalToken(id)
}

// RecordSessionStarter says which principal started a session that runs as
// another. A session has at most one starter, set once.
func (s *Store) RecordSessionStarter(starter SessionStarter) error {
	if starter.SessionID == "" || starter.PrincipalID == "" || starter.RecordedBy == "" {
		return fmt.Errorf("record session starter: session, principal and recorder are all required")
	}
	starter.RecordedAt = time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO session_starters (session_id, principal_id, recorded_by, recorded_at, backfilled) VALUES (?, ?, ?, ?, ?)`,
		starter.SessionID, starter.PrincipalID, starter.RecordedBy, starter.RecordedAt, starter.Backfilled)
	if err != nil {
		return fmt.Errorf("record session %s's starter: %w", starter.SessionID, err)
	}
	return nil
}

// SessionStarterOf returns who started the session as another principal, and
// false when no row says so.
func (s *Store) SessionStarterOf(sessionID string) (SessionStarter, bool, error) {
	starter := SessionStarter{SessionID: sessionID}
	err := s.dbRO.QueryRow(`SELECT principal_id, recorded_by, recorded_at, backfilled FROM session_starters WHERE session_id = ?`, sessionID).
		Scan(&starter.PrincipalID, &starter.RecordedBy, &starter.RecordedAt, &starter.Backfilled)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionStarter{}, false, nil
	}
	if err != nil {
		return SessionStarter{}, false, err
	}
	return starter, true, nil
}
