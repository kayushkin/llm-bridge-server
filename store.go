// Package signinstore owns how people sign in to the operator's sites, and
// the sessions that result. It does not own who anybody is: principal-store
// does, and every session check asks it whether the principal still exists,
// is enabled and is an administrator. One session cookie, set for the whole
// domain, reaches dash, kayushkin.com and the film player.
//
// Design note f3d492d9-a6b0-4ea1-bd8a-46f28ee3542b.
package signinstore

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	// ErrNotFound is an id or secret no row has.
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

	signInLinkIDPrefix = "sign_in_link_"
	sessionIDPrefix    = "session_"
)

// How a link reached its principal.
const (
	SentByOperator = "operator"
	SentByDiscord  = "discord"
	SentByPhone    = "phone"
	// SentByHostShell is a link made with `sign-in-store make-link` by
	// someone with a shell on this host: the way back in when every other
	// way is shut.
	SentByHostShell = "host_shell"
)

// How a session was signed in.
const (
	MethodSignInLink = "sign_in_link"
)

// Store owns the database.
type Store struct {
	db      *sql.DB
	dataDir string
	now     func() time.Time
}

// DefaultDataDir is where the database lives when nothing says otherwise.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sign-in-store")
}

// Open creates the data directory if needed, opens the database and applies
// the schema.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open: data directory is empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "sign-in-store.db")+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database is in.
func (s *Store) DataDir() string { return s.dataDir }

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

func prefixedID(prefix string, seq int64) string { return fmt.Sprintf("%s%06d", prefix, seq) }

func seqOf(prefix, id string) (int64, bool) {
	digits, ok := strings.CutPrefix(id, prefix)
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseInt(digits, 10, 64)
	return seq, err == nil && seq > 0
}

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
func (s *Store) MakeSignInLink(principalID, sentBy, madeBy string) (SignInLink, string, error) {
	secret, hash, err := newSecret()
	if err != nil {
		return SignInLink{}, "", err
	}
	now := s.now()
	link := SignInLink{PrincipalID: principalID, SentBy: sentBy, MadeBy: madeBy, CreatedAt: now.Unix(), ExpiresAt: now.Add(SignInLinkLifetime).Unix()}
	result, err := s.db.Exec(`INSERT INTO sign_in_links (secret_sha256, principal_id, sent_by, made_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hash, link.PrincipalID, link.SentBy, link.MadeBy, link.CreatedAt, link.ExpiresAt)
	if err != nil {
		return SignInLink{}, "", fmt.Errorf("record sign-in link for %s: %w", principalID, err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return SignInLink{}, "", err
	}
	link.ID = prefixedID(signInLinkIDPrefix, seq)
	return link, secret, nil
}

// SessionRequestFacts are what a sign-in records about the browser.
type SessionRequestFacts struct {
	UserAgent     string
	RemoteAddress string
}

// RedeemSignInLink spends the link whose secret is given and opens a session
// for its principal, in one transaction, so a link makes at most one session.
// mayStillSignIn is asked after the link is found and before anything is
// written; a refusal from it is returned unchanged and spends nothing.
func (s *Store) RedeemSignInLink(secret string, facts SessionRequestFacts, mayStillSignIn func(principalID string) error) (Session, string, error) {
	transaction, err := s.db.Begin()
	if err != nil {
		return Session{}, "", err
	}
	defer transaction.Rollback()
	now := s.now()
	var linkSeq int64
	var principalID string
	err = transaction.QueryRow(`SELECT seq, principal_id FROM sign_in_links WHERE secret_sha256 = ? AND redeemed_at = 0 AND expires_at > ?`,
		hashSecret(secret), now.Unix()).Scan(&linkSeq, &principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, "", ErrLinkNotUsable
	}
	if err != nil {
		return Session{}, "", err
	}
	if err := mayStillSignIn(principalID); err != nil {
		return Session{}, "", err
	}
	linkID := prefixedID(signInLinkIDPrefix, linkSeq)
	session, sessionSecret, err := insertSession(transaction, principalID, MethodSignInLink, linkID, facts, now)
	if err != nil {
		return Session{}, "", err
	}
	if _, err := transaction.Exec(`UPDATE sign_in_links SET redeemed_at = ?, session_id = ? WHERE seq = ?`, now.Unix(), session.ID, linkSeq); err != nil {
		return Session{}, "", fmt.Errorf("spend %s: %w", linkID, err)
	}
	if err := transaction.Commit(); err != nil {
		return Session{}, "", err
	}
	return session, sessionSecret, nil
}

// ListSignInLinks returns links newest first, for one principal when one is named.
func (s *Store) ListSignInLinks(principalID string, limit int) ([]SignInLink, error) {
	query := `SELECT seq, principal_id, sent_by, made_by, created_at, expires_at, redeemed_at, session_id FROM sign_in_links`
	var args []any
	if principalID != "" {
		query += ` WHERE principal_id = ?`
		args = append(args, principalID)
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
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
		link.ID = prefixedID(signInLinkIDPrefix, seq)
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

func insertSession(transaction *sql.Tx, principalID, method, signInLinkID string, facts SessionRequestFacts, now time.Time) (Session, string, error) {
	secret, hash, err := newSecret()
	if err != nil {
		return Session{}, "", err
	}
	session := Session{PrincipalID: principalID, Method: method, SignInLinkID: signInLinkID, CreatedAt: now.Unix(),
		ExpiresAt: now.Add(SessionLifetime).Unix(), LastSeenAt: now.Unix(), UserAgent: facts.UserAgent, RemoteAddress: facts.RemoteAddress}
	result, err := transaction.Exec(`INSERT INTO sessions (secret_sha256, principal_id, method, sign_in_link_id, created_at, expires_at, last_seen_at, user_agent, remote_address)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, hash, session.PrincipalID, session.Method, session.SignInLinkID, session.CreatedAt, session.ExpiresAt,
		session.LastSeenAt, session.UserAgent, session.RemoteAddress)
	if err != nil {
		return Session{}, "", fmt.Errorf("record session for %s: %w", principalID, err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return Session{}, "", err
	}
	session.ID = prefixedID(sessionIDPrefix, seq)
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
	session.ID = prefixedID(sessionIDPrefix, seq)
	return session, nil
}

// SessionBySecret returns the session a cookie secret names, whatever its
// state; the caller decides what expired and revoked mean.
func (s *Store) SessionBySecret(secret string) (Session, error) {
	session, err := scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE secret_sha256 = ?`, hashSecret(secret)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return session, err
}

// GetSession returns one session by its id.
func (s *Store) GetSession(id string) (Session, error) {
	seq, ok := seqOf(sessionIDPrefix, id)
	if !ok {
		return Session{}, ErrNotFound
	}
	session, err := scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE seq = ?`, seq).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return session, err
}

// ListSessions returns sessions newest first, for one principal when one is named.
func (s *Store) ListSessions(principalID string, limit int) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions`
	var args []any
	if principalID != "" {
		query += ` WHERE principal_id = ?`
		args = append(args, principalID)
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
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
func (s *Store) TouchSession(session Session) error {
	now := s.now()
	if now.Unix()-session.LastSeenAt < int64(lastSeenWriteInterval/time.Second) {
		return nil
	}
	seq, _ := seqOf(sessionIDPrefix, session.ID)
	_, err := s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE seq = ?`, now.Unix(), seq)
	return err
}

// RevokeSession stamps a session revoked and returns it as it now is. One
// revoked before is ErrAlreadyRevoked, unchanged.
func (s *Store) RevokeSession(id, revokedBy, reason string) (Session, error) {
	seq, ok := seqOf(sessionIDPrefix, id)
	if !ok {
		return Session{}, ErrNotFound
	}
	result, err := s.db.Exec(`UPDATE sessions SET revoked_at = ?, revoked_by = ?, revoke_reason = ? WHERE seq = ? AND revoked_at = 0`,
		s.now().Unix(), revokedBy, reason, seq)
	if err != nil {
		return Session{}, fmt.Errorf("revoke %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	session, err := s.GetSession(id)
	if err != nil {
		return Session{}, err
	}
	if changed == 0 {
		return session, ErrAlreadyRevoked
	}
	return session, nil
}
