package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Peers are other llm-bridge-servers linked to this one. A link has two ends,
// and each server keeps only its own:
//
//   - The issuing end, where the other server is given access. An operator
//     makes a peer invite for a principal-store principal of kind peer; the
//     other server redeems the invite's one-time passphrase and gets a peer
//     token, which it presents as X-LLM-Bridge-Peer-Token on every call. The
//     token acts as that principal, so what the other server may do here is
//     that principal's grants, groups and administrator flag. peer_invites and
//     peer_tokens are this end.
//   - The calling end, where this server holds a token for another. peer_links
//     records where the other server is, which of its principals this server
//     acts as there, and the auth-store credential that holds the token.
//
// "A has access on B" is a peer token in B and a peer link in A. "B has access
// on A" is a separate pair, so the two directions never depend on each other.
//
// Only hashes of the passphrase and the token are kept: someone who reads this
// database cannot act as a peer. No row is deleted: a redeemed invite, a
// revoked token and a removed link stay as the record of who linked what.

// ErrPeerInviteNotFound is a passphrase no invite has, or an id no row has.
var ErrPeerInviteNotFound = errors.New("peer invite not found")

// ErrPeerInviteAlreadyRedeemed is a passphrase used before. Each invite makes
// one token.
var ErrPeerInviteAlreadyRedeemed = errors.New("peer invite was already redeemed")

// ErrPeerInviteExpired is a passphrase redeemed after its invite expired.
var ErrPeerInviteExpired = errors.New("peer invite has expired")

// ErrPeerTokenNotFound is a token hash or id no row has.
var ErrPeerTokenNotFound = errors.New("peer token not found")

// ErrPeerTokenAlreadyRevoked is a revoke of a token revoked before. The first
// revocation's stamp and reason are kept.
var ErrPeerTokenAlreadyRevoked = errors.New("peer token is already revoked")

// ErrPeerLinkNotFound is an id no peer link has.
var ErrPeerLinkNotFound = errors.New("peer link not found")

// ErrPeerLinkAlreadyRemoved is a removal of a link removed before.
var ErrPeerLinkAlreadyRemoved = errors.New("peer link is already removed")

const (
	peerInviteIDPrefix = "peer_invite_"
	peerTokenIDPrefix  = "peer_token_"
	peerLinkIDPrefix   = "peer_link_"
)

// PeerInvite is one row of peer_invites: the record the routes send, and the
// hex SHA-256 of the passphrase, which is shown once, in the answer that
// created the invite, and never sent again.
type PeerInvite struct {
	msg.PeerInvite
	PassphraseHash string `json:"-"`
}

// PeerToken is one row of peer_tokens: the record the routes send, and the
// hex SHA-256 of the token, which is handed once, to the server that redeemed
// the invite.
type PeerToken struct {
	msg.PeerToken
	TokenHash string `json:"-"`
}

// PeerLink is one row of peer_links: another server this one holds a peer
// token for. It holds no secret, so the record is the wire record.
type PeerLink = msg.PeerLink

// migratePeers creates peer_invites, peer_tokens and peer_links. Called from
// migrate().
func (s *Store) migratePeers() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS peer_invites (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			passphrase_hash   TEXT NOT NULL UNIQUE,
			peer_principal_id TEXT NOT NULL,
			created_by        TEXT NOT NULL,
			created_at        DATETIME NOT NULL,
			expires_at        DATETIME NOT NULL,
			redeemed_at       DATETIME,
			peer_token_id     TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS peer_tokens (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			token_hash        TEXT NOT NULL UNIQUE,
			peer_principal_id TEXT NOT NULL,
			peer_invite_id    TEXT NOT NULL,
			created_at        DATETIME NOT NULL,
			revoked_at        DATETIME,
			revoked_by        TEXT NOT NULL DEFAULT '',
			revoke_reason     TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_peer_tokens_principal ON peer_tokens(peer_principal_id, id);
		CREATE TABLE IF NOT EXISTS peer_links (
			id                       INTEGER PRIMARY KEY AUTOINCREMENT,
			base_url                 TEXT NOT NULL,
			display_name             TEXT NOT NULL,
			remote_peer_principal_id TEXT NOT NULL,
			remote_peer_token_id     TEXT NOT NULL,
			credential_id            TEXT NOT NULL,
			created_by               TEXT NOT NULL,
			created_at               DATETIME NOT NULL,
			removed_at               DATETIME,
			removed_by               TEXT NOT NULL DEFAULT '',
			remove_reason            TEXT NOT NULL DEFAULT ''
		);
	`)
	return err
}

// InsertPeerInvite records a new invite and sets its ID.
func (s *Store) InsertPeerInvite(invite *PeerInvite) error {
	if invite.PassphraseHash == "" || invite.PeerPrincipalID == "" || invite.CreatedBy == "" || invite.ExpiresAt.IsZero() {
		return fmt.Errorf("insert peer invite: passphrase hash, peer principal, creator and expiry are all required")
	}
	result, err := s.db.Exec(
		`INSERT INTO peer_invites (passphrase_hash, peer_principal_id, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		invite.PassphraseHash, invite.PeerPrincipalID, invite.CreatedBy, invite.CreatedAt.UTC(), invite.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert peer invite for %s: %w", invite.PeerPrincipalID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert peer invite for %s: read its id: %w", invite.PeerPrincipalID, err)
	}
	invite.ID = prefixedIDOf(peerInviteIDPrefix, rowID)
	return nil
}

// RedeemPeerInvite spends the invite whose passphrase hashes to
// passphraseHash and records a peer token with tokenHash for its principal, in
// one transaction, so an invite makes exactly one token. The invite is checked
// before anything is written: unknown, already redeemed and expired each have
// their own error.
func (s *Store) RedeemPeerInvite(passphraseHash, tokenHash string, redeemedAt time.Time) (PeerToken, error) {
	if passphraseHash == "" || tokenHash == "" {
		return PeerToken{}, fmt.Errorf("redeem peer invite: passphrase hash and token hash are both required")
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return PeerToken{}, err
	}
	defer transaction.Rollback()
	invite, err := scanPeerInvite(transaction.QueryRow(peerInviteSelect+` WHERE passphrase_hash = ?`, passphraseHash))
	if errors.Is(err, sql.ErrNoRows) {
		return PeerToken{}, ErrPeerInviteNotFound
	}
	if err != nil {
		return PeerToken{}, err
	}
	if invite.RedeemedAt != nil {
		return PeerToken{}, fmt.Errorf("%w: %s was redeemed at %s as %s", ErrPeerInviteAlreadyRedeemed, invite.ID, invite.RedeemedAt.UTC().Format(time.RFC3339), invite.PeerTokenID)
	}
	if !redeemedAt.Before(invite.ExpiresAt) {
		return PeerToken{}, fmt.Errorf("%w: %s expired at %s", ErrPeerInviteExpired, invite.ID, invite.ExpiresAt.UTC().Format(time.RFC3339))
	}
	result, err := transaction.Exec(
		`INSERT INTO peer_tokens (token_hash, peer_principal_id, peer_invite_id, created_at) VALUES (?, ?, ?, ?)`,
		tokenHash, invite.PeerPrincipalID, invite.ID, redeemedAt.UTC())
	if err != nil {
		return PeerToken{}, fmt.Errorf("insert peer token for %s: %w", invite.PeerPrincipalID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return PeerToken{}, err
	}
	token := PeerToken{PeerToken: msg.PeerToken{ID: prefixedIDOf(peerTokenIDPrefix, rowID), PeerPrincipalID: invite.PeerPrincipalID, PeerInviteID: invite.ID, CreatedAt: redeemedAt.UTC()}, TokenHash: tokenHash}
	inviteRowID, _ := rowIDOfPrefixed(peerInviteIDPrefix, invite.ID)
	if _, err := transaction.Exec(`UPDATE peer_invites SET redeemed_at = ?, peer_token_id = ? WHERE id = ?`, redeemedAt.UTC(), token.ID, inviteRowID); err != nil {
		return PeerToken{}, fmt.Errorf("mark %s redeemed: %w", invite.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return PeerToken{}, err
	}
	return token, nil
}

// ListPeerInvites returns every invite, newest first, redeemed or not.
func (s *Store) ListPeerInvites() ([]PeerInvite, error) {
	rows, err := s.db.Query(peerInviteSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invites := []PeerInvite{}
	for rows.Next() {
		invite, err := scanPeerInvite(rows)
		if err != nil {
			return nil, err
		}
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

// GetPeerTokenByHash returns the token whose value hashes to tokenHash,
// revoked or not; the caller decides what revoked means.
func (s *Store) GetPeerTokenByHash(tokenHash string) (PeerToken, error) {
	token, err := scanPeerToken(s.db.QueryRow(peerTokenSelect+` WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return PeerToken{}, ErrPeerTokenNotFound
	}
	return token, err
}

// GetPeerToken returns one token by its id.
func (s *Store) GetPeerToken(id string) (PeerToken, error) {
	rowID, ok := rowIDOfPrefixed(peerTokenIDPrefix, id)
	if !ok {
		return PeerToken{}, ErrPeerTokenNotFound
	}
	token, err := scanPeerToken(s.db.QueryRow(peerTokenSelect+` WHERE id = ?`, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return PeerToken{}, ErrPeerTokenNotFound
	}
	return token, err
}

// ListPeerTokens returns every token, newest first, revoked or not.
func (s *Store) ListPeerTokens() ([]PeerToken, error) {
	rows, err := s.db.Query(peerTokenSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []PeerToken{}
	for rows.Next() {
		token, err := scanPeerToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// RevokePeerToken stamps one token revoked and returns it as it now is. A
// token revoked before is ErrPeerTokenAlreadyRevoked, with the row unchanged.
func (s *Store) RevokePeerToken(id, revokedBy, reason string, revokedAt time.Time) (PeerToken, error) {
	rowID, ok := rowIDOfPrefixed(peerTokenIDPrefix, id)
	if !ok {
		return PeerToken{}, ErrPeerTokenNotFound
	}
	result, err := s.db.Exec(
		`UPDATE peer_tokens SET revoked_at = ?, revoked_by = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
		revokedAt.UTC(), revokedBy, reason, rowID)
	if err != nil {
		return PeerToken{}, fmt.Errorf("revoke peer token %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return PeerToken{}, fmt.Errorf("revoke peer token %s: %w", id, err)
	}
	token, err := s.GetPeerToken(id)
	if err != nil {
		return PeerToken{}, err
	}
	if changed == 0 {
		return token, ErrPeerTokenAlreadyRevoked
	}
	return token, nil
}

// InsertPeerLink records a link to another server and sets its ID.
func (s *Store) InsertPeerLink(link *PeerLink) error {
	if link.BaseURL == "" || link.DisplayName == "" || link.RemotePeerPrincipalID == "" || link.RemotePeerTokenID == "" || link.CredentialID == "" || link.CreatedBy == "" {
		return fmt.Errorf("insert peer link: base URL, display name, remote principal, remote token id, credential and creator are all required")
	}
	result, err := s.db.Exec(
		`INSERT INTO peer_links (base_url, display_name, remote_peer_principal_id, remote_peer_token_id, credential_id, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		link.BaseURL, link.DisplayName, link.RemotePeerPrincipalID, link.RemotePeerTokenID, link.CredentialID, link.CreatedBy, link.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert peer link to %s: %w", link.BaseURL, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert peer link to %s: read its id: %w", link.BaseURL, err)
	}
	link.ID = prefixedIDOf(peerLinkIDPrefix, rowID)
	return nil
}

// GetPeerLink returns one link by its id, removed or not.
func (s *Store) GetPeerLink(id string) (PeerLink, error) {
	rowID, ok := rowIDOfPrefixed(peerLinkIDPrefix, id)
	if !ok {
		return PeerLink{}, ErrPeerLinkNotFound
	}
	link, err := scanPeerLink(s.db.QueryRow(peerLinkSelect+` WHERE id = ?`, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return PeerLink{}, ErrPeerLinkNotFound
	}
	return link, err
}

// ListPeerLinks returns every link, newest first, removed or not.
func (s *Store) ListPeerLinks() ([]PeerLink, error) {
	rows, err := s.db.Query(peerLinkSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []PeerLink{}
	for rows.Next() {
		link, err := scanPeerLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// RemovePeerLink stamps one link removed and returns it as it now is. A link
// removed before is ErrPeerLinkAlreadyRemoved, with the row unchanged.
func (s *Store) RemovePeerLink(id, removedBy, reason string, removedAt time.Time) (PeerLink, error) {
	rowID, ok := rowIDOfPrefixed(peerLinkIDPrefix, id)
	if !ok {
		return PeerLink{}, ErrPeerLinkNotFound
	}
	result, err := s.db.Exec(
		`UPDATE peer_links SET removed_at = ?, removed_by = ?, remove_reason = ? WHERE id = ? AND removed_at IS NULL`,
		removedAt.UTC(), removedBy, reason, rowID)
	if err != nil {
		return PeerLink{}, fmt.Errorf("remove peer link %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return PeerLink{}, fmt.Errorf("remove peer link %s: %w", id, err)
	}
	link, err := s.GetPeerLink(id)
	if err != nil {
		return PeerLink{}, err
	}
	if changed == 0 {
		return link, ErrPeerLinkAlreadyRemoved
	}
	return link, nil
}

const (
	peerInviteSelect = `SELECT id, passphrase_hash, peer_principal_id, created_by, created_at, expires_at, redeemed_at, peer_token_id FROM peer_invites`
	peerTokenSelect  = `SELECT id, token_hash, peer_principal_id, peer_invite_id, created_at, revoked_at, revoked_by, revoke_reason FROM peer_tokens`
	peerLinkSelect   = `SELECT id, base_url, display_name, remote_peer_principal_id, remote_peer_token_id, credential_id, created_by, created_at,
		removed_at, removed_by, remove_reason FROM peer_links`
)

func scanPeerInvite(row interface{ Scan(...any) error }) (PeerInvite, error) {
	var invite PeerInvite
	var rowID int64
	var redeemedAt sql.NullTime
	if err := row.Scan(&rowID, &invite.PassphraseHash, &invite.PeerPrincipalID, &invite.CreatedBy, &invite.CreatedAt, &invite.ExpiresAt, &redeemedAt, &invite.PeerTokenID); err != nil {
		return PeerInvite{}, err
	}
	invite.ID = prefixedIDOf(peerInviteIDPrefix, rowID)
	if redeemedAt.Valid {
		invite.RedeemedAt = &redeemedAt.Time
	}
	return invite, nil
}

func scanPeerToken(row interface{ Scan(...any) error }) (PeerToken, error) {
	var token PeerToken
	var rowID int64
	var revokedAt sql.NullTime
	if err := row.Scan(&rowID, &token.TokenHash, &token.PeerPrincipalID, &token.PeerInviteID, &token.CreatedAt, &revokedAt, &token.RevokedBy, &token.RevokeReason); err != nil {
		return PeerToken{}, err
	}
	token.ID = prefixedIDOf(peerTokenIDPrefix, rowID)
	if revokedAt.Valid {
		token.RevokedAt = &revokedAt.Time
	}
	return token, nil
}

func scanPeerLink(row interface{ Scan(...any) error }) (PeerLink, error) {
	var link PeerLink
	var rowID int64
	var removedAt sql.NullTime
	if err := row.Scan(&rowID, &link.BaseURL, &link.DisplayName, &link.RemotePeerPrincipalID, &link.RemotePeerTokenID, &link.CredentialID,
		&link.CreatedBy, &link.CreatedAt, &removedAt, &link.RemovedBy, &link.RemoveReason); err != nil {
		return PeerLink{}, err
	}
	link.ID = prefixedIDOf(peerLinkIDPrefix, rowID)
	if removedAt.Valid {
		link.RemovedAt = &removedAt.Time
	}
	return link, nil
}

func prefixedIDOf(prefix string, rowID int64) string {
	return fmt.Sprintf("%s%06d", prefix, rowID)
}

func rowIDOfPrefixed(prefix, id string) (int64, bool) {
	digits, hasPrefix := strings.CutPrefix(id, prefix)
	if !hasPrefix {
		return 0, false
	}
	rowID, err := strconv.ParseInt(digits, 10, 64)
	return rowID, err == nil && rowID > 0
}
