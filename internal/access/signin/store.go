// Package signin owns how people sign in to the operator's sites, and
// the sessions that result. It does not own who anybody is: principal-store
// does, and every session check asks it whether the principal still exists,
// is enabled and is an administrator. One session cookie, set for the whole
// domain, reaches dash, example.test and the film player.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
//
// Design note f3d492d9-a6b0-4ea1-bd8a-46f28ee3542b.
package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	// ErrNotFound is an id or secret no row has, a principal principal-store
	// does not have, or an identity bound to nobody.
	ErrNotFound = errors.New("not found")
	// ErrLinkNotUsable is a link that was used, expired or never existed. The
	// three are one answer: a caller holding a secret learns nothing more.
	ErrLinkNotUsable = errors.New("this sign-in link has been used, has expired, or never existed; ask for a new one")
	// ErrAlreadyRevoked is a revoke of a session revoked before.
	ErrAlreadyRevoked = errors.New("already revoked")
)

const (
	// SignInLinkLifetime is how long a link may be redeemed.
	SignInLinkLifetime = 7 * 24 * time.Hour
	// SessionLifetime is how long a session lasts from sign-in. It is not
	// extended by use.
	SessionLifetime = 180 * 24 * time.Hour
	// secretRandomBytes is the size of a link or session secret.
	secretRandomBytes = 32
	// lastSeenWriteInterval is how often a session's last_seen_at is written:
	// every request of a page load would otherwise be a write.
	lastSeenWriteInterval = time.Minute

	// SignInLinkIDPrefix and SessionIDPrefix are the type parts of the public
	// ids: sign_in_link_000001, session_000001.
	SignInLinkIDPrefix = "sign_in_link"
	SessionIDPrefix    = "session"
)

// How a link reached its principal.
const (
	SentByOperator = "operator"
	SentByDiscord  = "discord"
	SentByPhone    = "phone"
	// SentByHostShell is a link made from a shell on this host: the way back
	// in when every other way is shut.
	SentByHostShell = "host_shell"
)

// How a session was signed in.
const (
	MethodSignInLink = "sign_in_link"
)

// MethodDiscord is in discord.go.

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "sign_in", Role: "sign_in_module", Migrations: migrations}, nil
}

// Store keeps links, sessions and Discord attempts in the module's schema.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New returns the store over db, the module's pool, already migrated.
func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// newSecret returns a random secret and the hash it is kept as.
func newSecret() (secret, hash string, err error) {
	raw := make([]byte, secretRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read crypto/rand: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(raw)
	return secret, hashSecret(secret), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func signInLinkID(seq int64) string { return postgresstorage.FormatRecordID(SignInLinkIDPrefix, seq) }
func sessionID(seq int64) string    { return postgresstorage.FormatRecordID(SessionIDPrefix, seq) }

// SignInLink is one row of sign_in_links. The secret is never in it.
type SignInLink struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	SentBy      string `json:"sent_by"`
	MadeBy      string `json:"made_by,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
	RedeemedAt  int64  `json:"redeemed_at,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
}

// MakeSignInLink records a new link for principalID and returns it with its
// secret, which is shown once and kept nowhere.
func (s *Store) MakeSignInLink(ctx context.Context, principalID, sentBy, madeBy string) (SignInLink, string, error) {
	secret, hash, err := newSecret()
	if err != nil {
		return SignInLink{}, "", err
	}
	now := s.now()
	link := SignInLink{PrincipalID: principalID, SentBy: sentBy, MadeBy: madeBy, CreatedAt: now.Unix(), ExpiresAt: now.Add(SignInLinkLifetime).Unix()}
	var seq int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO sign_in_links (secret_sha256, principal_id, sent_by, made_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING seq`,
		hash, link.PrincipalID, link.SentBy, link.MadeBy, link.CreatedAt, link.ExpiresAt).Scan(&seq)
	if err != nil {
		return SignInLink{}, "", fmt.Errorf("record sign-in link for %s: %w", principalID, err)
	}
	link.ID = signInLinkID(seq)
	return link, secret, nil
}

// SessionRequestFacts are what a sign-in records about the browser.
type SessionRequestFacts struct {
	UserAgent     string
	RemoteAddress string
}

// RedeemSignInLink spends the link whose secret is given and opens a session
// for its principal, in one transaction, so a link makes at most one session.
// The link's row is locked while that happens, so a second redeem of the same
// secret waits and then finds it spent. mayStillSignIn is asked after the
// link is found and before anything is written; a refusal from it is returned
// unchanged and spends nothing.
func (s *Store) RedeemSignInLink(ctx context.Context, secret string, facts SessionRequestFacts, mayStillSignIn func(principalID string) error) (Session, string, error) {
	var session Session
	var sessionSecret string
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		now := s.now()
		var linkSeq int64
		var principalID string
		err := tx.QueryRowContext(ctx, `SELECT seq, principal_id FROM sign_in_links
			WHERE secret_sha256 = $1 AND redeemed_at = 0 AND expires_at > $2 FOR UPDATE`,
			hashSecret(secret), now.Unix()).Scan(&linkSeq, &principalID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLinkNotUsable
		}
		if err != nil {
			return err
		}
		if err := mayStillSignIn(principalID); err != nil {
			return err
		}
		linkID := signInLinkID(linkSeq)
		session, sessionSecret, err = insertSession(ctx, tx, principalID, MethodSignInLink, linkID, facts, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sign_in_links SET redeemed_at = $1, session_id = $2 WHERE seq = $3`, now.Unix(), session.ID, linkSeq); err != nil {
			return fmt.Errorf("spend %s: %w", linkID, err)
		}
		return nil
	})
	if err != nil {
		return Session{}, "", err
	}
	return session, sessionSecret, nil
}

// ListSignInLinks returns links newest first, for one principal when one is named.
func (s *Store) ListSignInLinks(ctx context.Context, principalID string, limit int) ([]SignInLink, error) {
	query := `SELECT seq, principal_id, sent_by, made_by, created_at, expires_at, redeemed_at, session_id FROM sign_in_links`
	var args []any
	if principalID != "" {
		args = append(args, principalID)
		query += fmt.Sprintf(` WHERE principal_id = $%d`, len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY seq DESC LIMIT $%d`, len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []SignInLink{}
	for rows.Next() {
		var link SignInLink
		var seq int64
		if err := rows.Scan(&seq, &link.PrincipalID, &link.SentBy, &link.MadeBy, &link.CreatedAt, &link.ExpiresAt, &link.RedeemedAt, &link.SessionID); err != nil {
			return nil, err
		}
		link.ID = signInLinkID(seq)
		links = append(links, link)
	}
	return links, rows.Err()
}

// Session is one row of sessions. The secret is never in it.
type Session struct {
	ID            string `json:"id"`
	PrincipalID   string `json:"principal_id"`
	Method        string `json:"method"`
	SignInLinkID  string `json:"sign_in_link_id,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"`
	LastSeenAt    int64  `json:"last_seen_at"`
	RevokedAt     int64  `json:"revoked_at,omitempty"`
	RevokedBy     string `json:"revoked_by,omitempty"`
	RevokeReason  string `json:"revoke_reason,omitempty"`
	UserAgent     string `json:"user_agent"`
	RemoteAddress string `json:"remote_address"`
}

func insertSession(ctx context.Context, tx *sql.Tx, principalID, method, signInLinkID string, facts SessionRequestFacts, now time.Time) (Session, string, error) {
	secret, hash, err := newSecret()
	if err != nil {
		return Session{}, "", err
	}
	session := Session{PrincipalID: principalID, Method: method, SignInLinkID: signInLinkID, CreatedAt: now.Unix(),
		ExpiresAt: now.Add(SessionLifetime).Unix(), LastSeenAt: now.Unix(), UserAgent: facts.UserAgent, RemoteAddress: facts.RemoteAddress}
	var seq int64
	err = tx.QueryRowContext(ctx, `INSERT INTO sessions (secret_sha256, principal_id, method, sign_in_link_id, created_at, expires_at, last_seen_at, user_agent, remote_address)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING seq`, hash, session.PrincipalID, session.Method, session.SignInLinkID, session.CreatedAt, session.ExpiresAt,
		session.LastSeenAt, session.UserAgent, session.RemoteAddress).Scan(&seq)
	if err != nil {
		return Session{}, "", fmt.Errorf("record session for %s: %w", principalID, err)
	}
	session.ID = sessionID(seq)
	return session, secret, nil
}

const sessionColumns = `seq, principal_id, method, sign_in_link_id, created_at, expires_at, last_seen_at, revoked_at, revoked_by, revoke_reason, user_agent, remote_address`

func scanSession(scan func(...any) error) (Session, error) {
	var session Session
	var seq int64
	if err := scan(&seq, &session.PrincipalID, &session.Method, &session.SignInLinkID, &session.CreatedAt, &session.ExpiresAt, &session.LastSeenAt,
		&session.RevokedAt, &session.RevokedBy, &session.RevokeReason, &session.UserAgent, &session.RemoteAddress); err != nil {
		return Session{}, err
	}
	session.ID = sessionID(seq)
	return session, nil
}

// SessionBySecret returns the session a cookie secret names, whatever its
// state; the caller decides what expired and revoked mean.
func (s *Store) SessionBySecret(ctx context.Context, secret string) (Session, error) {
	return s.sessionBySecretHash(ctx, hashSecret(secret))
}

func (s *Store) sessionBySecretHash(ctx context.Context, hash string) (Session, error) {
	session, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE secret_sha256 = $1`, hash).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return session, err
}

// GetSession returns one session by its id. An id that is not a session id
// names no row.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	seq, err := postgresstorage.ParseRecordID(SessionIDPrefix, id)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	session, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE seq = $1`, seq).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return session, err
}

// ListSessions returns sessions newest first, for one principal when one is named.
func (s *Store) ListSessions(ctx context.Context, principalID string, limit int) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions`
	var args []any
	if principalID != "" {
		args = append(args, principalID)
		query += fmt.Sprintf(` WHERE principal_id = $%d`, len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY seq DESC LIMIT $%d`, len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []Session{}
	for rows.Next() {
		session, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// TouchSession writes last_seen_at when the stored value is older than
// lastSeenWriteInterval.
func (s *Store) TouchSession(ctx context.Context, session Session) error {
	now := s.now()
	if now.Unix()-session.LastSeenAt < int64(lastSeenWriteInterval/time.Second) {
		return nil
	}
	seq, err := postgresstorage.ParseRecordID(SessionIDPrefix, session.ID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = $1 WHERE seq = $2`, now.Unix(), seq)
	return err
}

// RevokeSession stamps a session revoked and returns it as it now is. One
// revoked before is ErrAlreadyRevoked, unchanged.
func (s *Store) RevokeSession(ctx context.Context, id, revokedBy, reason string) (Session, error) {
	seq, err := postgresstorage.ParseRecordID(SessionIDPrefix, id)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = $1, revoked_by = $2, revoke_reason = $3 WHERE seq = $4 AND revoked_at = 0`,
		s.now().Unix(), revokedBy, reason, seq)
	if err != nil {
		return Session{}, fmt.Errorf("revoke %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	session, err := s.GetSession(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if changed == 0 {
		return session, ErrAlreadyRevoked
	}
	return session, nil
}
