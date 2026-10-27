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

// Peers are other llm-bridge-servers connected to this one (design note
// d107154a-49ac-471b-a1b7-fc7975de37f3). A connection and a grant of access are
// separate, and each server keeps only its own rows:
//
//   - peer_connection_invites: one-time passphrases this server minted so that
//     another server can connect to it. Only a server with an address mints
//     one.
//   - peer_connections: every server this one is connected to, with the hash
//     of the credential the other presents on connection calls and the
//     auth-store credential that holds the one this server presents. A
//     connection gives no access.
//   - peer_grants: access this server gave another. The row holds the hash of
//     the peer token; only the other server holds the token.
//   - peer_access: access another server gave this one. The token is in
//     auth-store; the row names the credential.
//   - peer_access_levels: the principal-store group this server provisioned
//     for each level that needs one, so the group is found by the level's key
//     and never by a name.
//
// Only hashes of passphrases, credentials and tokens are kept: someone who
// reads this database cannot act as a peer. No row is deleted: a redeemed
// invite, a removed connection, a revoked grant and ended access stay as the
// record of who connected what.

// ErrPeerConnectionInviteNotFound is a passphrase no invite has.
var ErrPeerConnectionInviteNotFound = errors.New("peer connection invite not found")

// ErrPeerConnectionInviteAlreadyRedeemed is a passphrase used before.
var ErrPeerConnectionInviteAlreadyRedeemed = errors.New("peer connection invite was already redeemed")

// ErrPeerConnectionInviteExpired is a passphrase redeemed after its invite
// expired.
var ErrPeerConnectionInviteExpired = errors.New("peer connection invite has expired")

// ErrPeerConnectionNotFound is an id or credential no connection has.
var ErrPeerConnectionNotFound = errors.New("peer connection not found")

// ErrPeerConnectionAlreadyRemoved is a removal of a connection removed before.
var ErrPeerConnectionAlreadyRemoved = errors.New("peer connection is already removed")

// ErrPeerGrantNotFound is a token hash or id no grant has.
var ErrPeerGrantNotFound = errors.New("peer grant not found")

// ErrPeerGrantAlreadyRevoked is a revoke of a grant revoked before.
var ErrPeerGrantAlreadyRevoked = errors.New("peer grant is already revoked")

// ErrPeerAccessNotFound is a connection holding no live access.
var ErrPeerAccessNotFound = errors.New("peer access not found")

// ErrPeerAccessLevelNotProvisioned is a level whose group this server has not
// made.
var ErrPeerAccessLevelNotProvisioned = errors.New("peer access level is not provisioned")

const (
	peerConnectionInviteIDPrefix = "peer_connection_invite_"
	peerConnectionIDPrefix       = "peer_connection_"
	peerGrantIDPrefix            = "peer_grant_"
	peerAccessIDPrefix           = "peer_access_"
)

// PeerConnectionInvite is one row of peer_connection_invites, with the hash of
// the passphrase, which is shown once and never sent again.
type PeerConnectionInvite struct {
	msg.PeerConnectionInvite
	PassphraseHash string `json:"-"`
}

// PeerConnection is one row of peer_connections: the record the routes send,
// the hash of the credential the other server presents here, and the
// auth-store credential holding the one this server presents there.
type PeerConnection struct {
	msg.PeerConnection
	InboundCredentialHash string `json:"-"`
	OutboundCredentialID  string `json:"-"`
	// OtherServerName is the name the other server last reported for itself,
	// and OtherServerNameReportedAt when; the view carries both.
	OtherServerName           string     `json:"-"`
	OtherServerNameReportedAt *time.Time `json:"-"`
}

// PeerGrant is one row of peer_grants, with the hash of the token.
type PeerGrant struct {
	msg.PeerGrant
	TokenHash string `json:"-"`
}

// PeerAccess is one row of peer_access. It holds no secret.
type PeerAccess = msg.PeerAccess

// migratePeers creates the peer tables, and drops the invite, token and link
// tables they replaced on 2026-10-27. Those are dropped only while no token
// was ever issued and no link ever made: a row in either would be a live way
// in or out that a migration must not erase, so the start stops and says so.
// Called from migrate().
func (s *Store) migratePeers() error {
	for _, table := range []string{"peer_tokens", "peer_links"} {
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		var rows int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&rows); err != nil {
			return err
		}
		if rows > 0 {
			return fmt.Errorf("%s holds %d rows: peer connections replaced invites, tokens and links on 2026-10-27, and a migration will not drop a live peer credential; revoke and remove them by hand, then delete the rows", table, rows)
		}
	}
	_, err := s.db.Exec(`
		DROP TABLE IF EXISTS peer_invites;
		DROP TABLE IF EXISTS peer_tokens;
		DROP TABLE IF EXISTS peer_links;
		CREATE TABLE IF NOT EXISTS peer_connection_invites (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			passphrase_hash TEXT NOT NULL UNIQUE,
			created_by      TEXT NOT NULL,
			created_at      DATETIME NOT NULL,
			expires_at      DATETIME NOT NULL,
			redeemed_at     DATETIME,
			connection_id   TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS peer_connections (
			id                      INTEGER PRIMARY KEY AUTOINCREMENT,
			display_name            TEXT NOT NULL,
			transport               TEXT NOT NULL,
			role                    TEXT NOT NULL,
			remote_base_url         TEXT NOT NULL DEFAULT '',
			remote_connection_id    TEXT NOT NULL DEFAULT '',
			inbound_credential_hash TEXT NOT NULL UNIQUE,
			outbound_credential_id  TEXT NOT NULL,
			peer_principal_id       TEXT NOT NULL DEFAULT '',
			created_by              TEXT NOT NULL,
			created_at              DATETIME NOT NULL,
			removed_at              DATETIME,
			removed_by              TEXT NOT NULL DEFAULT '',
			remove_reason           TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS peer_grants (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_id      TEXT NOT NULL,
			token_hash         TEXT NOT NULL UNIQUE,
			peer_principal_id  TEXT NOT NULL,
			level              TEXT NOT NULL,
			group_principal_id TEXT NOT NULL DEFAULT '',
			created_by         TEXT NOT NULL,
			created_at         DATETIME NOT NULL,
			delivered_at       DATETIME,
			revoked_at         DATETIME,
			revoked_by         TEXT NOT NULL DEFAULT '',
			revoke_reason      TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_peer_grants_connection ON peer_grants(connection_id, id);
		CREATE TABLE IF NOT EXISTS peer_access (
			id                       INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_id            TEXT NOT NULL,
			remote_grant_id          TEXT NOT NULL,
			remote_peer_principal_id TEXT NOT NULL,
			level                    TEXT NOT NULL,
			credential_id            TEXT NOT NULL,
			received_at              DATETIME NOT NULL,
			ended_at                 DATETIME,
			end_reason               TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_peer_access_connection ON peer_access(connection_id, id);
		CREATE TABLE IF NOT EXISTS peer_access_levels (
			level_key          TEXT PRIMARY KEY,
			group_principal_id TEXT NOT NULL,
			provisioned_by     TEXT NOT NULL,
			provisioned_at     DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS peer_user_mappings (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			peer_principal_id   TEXT NOT NULL,
			remote_principal_id TEXT NOT NULL,
			local_principal_id  TEXT NOT NULL,
			created_by          TEXT NOT NULL,
			created_at          DATETIME NOT NULL,
			removed_at          DATETIME,
			removed_by          TEXT NOT NULL DEFAULT '',
			remove_reason       TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_user_mappings_live
			ON peer_user_mappings(peer_principal_id, remote_principal_id) WHERE removed_at IS NULL;
		CREATE TABLE IF NOT EXISTS session_peer_origins (
			session_id          TEXT PRIMARY KEY,
			peer_principal_id   TEXT NOT NULL,
			remote_principal_id TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_session_peer_origins_peer ON session_peer_origins(peer_principal_id);
	`)
	if err != nil {
		return err
	}
	for _, column := range []string{
		"ALTER TABLE peer_connections ADD COLUMN removed_by_other_server INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE peer_connections ADD COLUMN removal_notice TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE peer_connections ADD COLUMN other_server_name TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE peer_connections ADD COLUMN other_server_name_reported_at DATETIME",
	} {
		if _, err := s.db.Exec(column); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// InsertPeerConnectionInvite records a new invite and sets its ID.
func (s *Store) InsertPeerConnectionInvite(invite *PeerConnectionInvite) error {
	if invite.PassphraseHash == "" || invite.CreatedBy == "" || invite.ExpiresAt.IsZero() {
		return fmt.Errorf("insert peer connection invite: passphrase hash, creator and expiry are all required")
	}
	result, err := s.db.Exec(`INSERT INTO peer_connection_invites (passphrase_hash, created_by, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		invite.PassphraseHash, invite.CreatedBy, invite.CreatedAt.UTC(), invite.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert peer connection invite: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	invite.ID = prefixedIDOf(peerConnectionInviteIDPrefix, rowID)
	return nil
}

// CheckPeerConnectionInvite says whether the passphrase names an invite that
// may still be redeemed, without spending it: unknown, already redeemed and
// expired each have their own error.
func (s *Store) CheckPeerConnectionInvite(passphraseHash string, at time.Time) (PeerConnectionInvite, error) {
	return checkPeerConnectionInvite(s.db.QueryRow(peerConnectionInviteSelect+` WHERE passphrase_hash = ?`, passphraseHash), at)
}

func checkPeerConnectionInvite(row *sql.Row, at time.Time) (PeerConnectionInvite, error) {
	invite, err := scanPeerConnectionInvite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PeerConnectionInvite{}, ErrPeerConnectionInviteNotFound
	}
	if err != nil {
		return PeerConnectionInvite{}, err
	}
	if invite.RedeemedAt != nil {
		return invite, fmt.Errorf("%w: %s was redeemed at %s as %s", ErrPeerConnectionInviteAlreadyRedeemed, invite.ID, invite.RedeemedAt.UTC().Format(time.RFC3339), invite.ConnectionID)
	}
	if !at.Before(invite.ExpiresAt) {
		return invite, fmt.Errorf("%w: %s expired at %s", ErrPeerConnectionInviteExpired, invite.ID, invite.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return invite, nil
}

// RedeemPeerConnectionInvite spends the invite and records the connection it
// makes, in one transaction, so an invite makes exactly one connection. It
// sets the connection's ID.
func (s *Store) RedeemPeerConnectionInvite(passphraseHash string, connection *PeerConnection, at time.Time) error {
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	invite, err := checkPeerConnectionInvite(transaction.QueryRow(peerConnectionInviteSelect+` WHERE passphrase_hash = ?`, passphraseHash), at)
	if err != nil {
		return err
	}
	if err := insertPeerConnection(transaction, connection); err != nil {
		return err
	}
	inviteRowID, _ := rowIDOfPrefixed(peerConnectionInviteIDPrefix, invite.ID)
	if _, err := transaction.Exec(`UPDATE peer_connection_invites SET redeemed_at = ?, connection_id = ? WHERE id = ?`, at.UTC(), connection.ID, inviteRowID); err != nil {
		return fmt.Errorf("mark %s redeemed: %w", invite.ID, err)
	}
	return transaction.Commit()
}

// ListPeerConnectionInvites returns every invite, newest first.
func (s *Store) ListPeerConnectionInvites() ([]PeerConnectionInvite, error) {
	rows, err := s.db.Query(peerConnectionInviteSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invites := []PeerConnectionInvite{}
	for rows.Next() {
		invite, err := scanPeerConnectionInvite(rows)
		if err != nil {
			return nil, err
		}
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

// InsertPeerConnection records a connection and sets its ID.
func (s *Store) InsertPeerConnection(connection *PeerConnection) error {
	return insertPeerConnection(s.db, connection)
}

func insertPeerConnection(executor interface {
	Exec(string, ...any) (sql.Result, error)
}, connection *PeerConnection) error {
	if connection.Transport == "" || connection.Role == "" ||
		connection.InboundCredentialHash == "" || connection.OutboundCredentialID == "" || connection.CreatedBy == "" {
		return fmt.Errorf("insert peer connection: transport, role, both credentials and creator are all required")
	}
	// The initiating side learns the accepting side's id from its answer; the
	// accepting side is never told the initiating side's, which it does not
	// need.
	if connection.Role == msg.PeerConnectionRoleInitiating && connection.RemoteConnectionID == "" {
		return fmt.Errorf("insert peer connection: the initiating side records the accepting side's connection id")
	}
	var otherServerNameReportedAt any
	if connection.OtherServerNameReportedAt != nil {
		otherServerNameReportedAt = connection.OtherServerNameReportedAt.UTC()
	}
	result, err := executor.Exec(`INSERT INTO peer_connections (display_name, transport, role, remote_base_url, remote_connection_id,
		inbound_credential_hash, outbound_credential_id, created_by, created_at, other_server_name, other_server_name_reported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		connection.DisplayName, string(connection.Transport), string(connection.Role), connection.RemoteBaseURL, connection.RemoteConnectionID,
		connection.InboundCredentialHash, connection.OutboundCredentialID, connection.CreatedBy, connection.CreatedAt.UTC(),
		connection.OtherServerName, otherServerNameReportedAt)
	if err != nil {
		return fmt.Errorf("insert peer connection %q: %w", connection.DisplayName, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	connection.ID = prefixedIDOf(peerConnectionIDPrefix, rowID)
	return nil
}

// GetPeerConnection returns one connection by its id, removed or not.
func (s *Store) GetPeerConnection(id string) (PeerConnection, error) {
	rowID, ok := rowIDOfPrefixed(peerConnectionIDPrefix, id)
	if !ok {
		return PeerConnection{}, ErrPeerConnectionNotFound
	}
	found, err := scanPeerConnection(s.db.QueryRow(peerConnectionSelect+` WHERE id = ?`, rowID))
	return notFoundAs(found, err, ErrPeerConnectionNotFound)
}

// GetPeerConnectionByInboundCredentialHash returns the connection whose other
// server presents the credential that hashes to credentialHash, removed or
// not; the caller decides what removed means.
func (s *Store) GetPeerConnectionByInboundCredentialHash(credentialHash string) (PeerConnection, error) {
	found, err := scanPeerConnection(s.db.QueryRow(peerConnectionSelect+` WHERE inbound_credential_hash = ?`, credentialHash))
	return notFoundAs(found, err, ErrPeerConnectionNotFound)
}

// ListPeerConnections returns every connection, newest first, removed or not.
func (s *Store) ListPeerConnections() ([]PeerConnection, error) {
	rows, err := s.db.Query(peerConnectionSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	connections := []PeerConnection{}
	for rows.Next() {
		connection, err := scanPeerConnection(rows)
		if err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	return connections, rows.Err()
}

// RecordPeerConnectionOtherServerName records the name the other server
// reported for itself on a connection. It writes only when the name differs
// from the one recorded, or none was ever recorded, and says whether it wrote.
func (s *Store) RecordPeerConnectionOtherServerName(id, name string, at time.Time) (bool, error) {
	rowID, ok := rowIDOfPrefixed(peerConnectionIDPrefix, id)
	if !ok {
		return false, ErrPeerConnectionNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_connections SET other_server_name = ?, other_server_name_reported_at = ?
		WHERE id = ? AND (other_server_name != ? OR other_server_name_reported_at IS NULL)`, name, at.UTC(), rowID, name)
	if err != nil {
		return false, fmt.Errorf("record %s's other server name: %w", id, err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return written > 0, nil
}

// SetPeerConnectionPeerPrincipal records the principal this server made for
// the other server. It is set once: a connection whose principal is already
// set keeps it.
func (s *Store) SetPeerConnectionPeerPrincipal(id, principalID string) error {
	rowID, ok := rowIDOfPrefixed(peerConnectionIDPrefix, id)
	if !ok {
		return ErrPeerConnectionNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_connections SET peer_principal_id = ? WHERE id = ? AND peer_principal_id = ''`, principalID, rowID)
	if err != nil {
		return fmt.Errorf("set %s's peer principal: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%s already has a peer principal, or does not exist", id)
	}
	return nil
}

// RemovePeerConnection stamps one connection removed and returns it as it now
// is. A connection removed before is ErrPeerConnectionAlreadyRemoved.
func (s *Store) RemovePeerConnection(id, removedBy, reason string, removedAt time.Time) (PeerConnection, error) {
	rowID, ok := rowIDOfPrefixed(peerConnectionIDPrefix, id)
	if !ok {
		return PeerConnection{}, ErrPeerConnectionNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_connections SET removed_at = ?, removed_by = ?, remove_reason = ? WHERE id = ? AND removed_at IS NULL`,
		removedAt.UTC(), removedBy, reason, rowID)
	if err != nil {
		return PeerConnection{}, fmt.Errorf("remove peer connection %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return PeerConnection{}, err
	}
	connection, err := s.GetPeerConnection(id)
	if err != nil {
		return PeerConnection{}, err
	}
	if changed == 0 {
		return connection, ErrPeerConnectionAlreadyRemoved
	}
	return connection, nil
}

// RecordPeerConnectionRemovalNotice keeps what happened when this server told
// the other one it removed the connection.
func (s *Store) RecordPeerConnectionRemovalNotice(id, notice string) error {
	rowID, ok := rowIDOfPrefixed(peerConnectionIDPrefix, id)
	if !ok {
		return ErrPeerConnectionNotFound
	}
	_, err := s.db.Exec(`UPDATE peer_connections SET removal_notice = ? WHERE id = ?`, notice, rowID)
	return err
}

// RemovePeerConnectionByOtherServer marks a connection removed because the
// other server removed it and said so over it.
func (s *Store) RemovePeerConnectionByOtherServer(id, reason string, removedAt time.Time) (PeerConnection, error) {
	rowID, ok := rowIDOfPrefixed(peerConnectionIDPrefix, id)
	if !ok {
		return PeerConnection{}, ErrPeerConnectionNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_connections SET removed_at = ?, removed_by = '', remove_reason = ?, removed_by_other_server = 1 WHERE id = ? AND removed_at IS NULL`,
		removedAt.UTC(), reason, rowID)
	if err != nil {
		return PeerConnection{}, fmt.Errorf("remove peer connection %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return PeerConnection{}, err
	}
	connection, err := s.GetPeerConnection(id)
	if err != nil {
		return PeerConnection{}, err
	}
	if changed == 0 {
		return connection, ErrPeerConnectionAlreadyRemoved
	}
	return connection, nil
}

// InsertPeerGrant records a grant and sets its ID. It refuses while the
// connection has another live grant: the caller revokes that one first, so a
// connection never gives two levels at once.
func (s *Store) InsertPeerGrant(grant *PeerGrant) error {
	if grant.ConnectionID == "" || grant.TokenHash == "" || grant.PeerPrincipalID == "" || grant.Level == "" || grant.CreatedBy == "" {
		return fmt.Errorf("insert peer grant: connection, token hash, peer principal, level and creator are all required")
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var live int
	if err := transaction.QueryRow(`SELECT COUNT(*) FROM peer_grants WHERE connection_id = ? AND revoked_at IS NULL`, grant.ConnectionID).Scan(&live); err != nil {
		return err
	}
	if live > 0 {
		return fmt.Errorf("%s already has a live grant; revoke it before granting another level", grant.ConnectionID)
	}
	result, err := transaction.Exec(`INSERT INTO peer_grants (connection_id, token_hash, peer_principal_id, level, group_principal_id, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, grant.ConnectionID, grant.TokenHash, grant.PeerPrincipalID, grant.Level, grant.GroupPrincipalID, grant.CreatedBy, grant.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert peer grant on %s: %w", grant.ConnectionID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	grant.ID = prefixedIDOf(peerGrantIDPrefix, rowID)
	return nil
}

// GetPeerGrant returns one grant by its id.
func (s *Store) GetPeerGrant(id string) (PeerGrant, error) {
	rowID, ok := rowIDOfPrefixed(peerGrantIDPrefix, id)
	if !ok {
		return PeerGrant{}, ErrPeerGrantNotFound
	}
	found, err := scanPeerGrant(s.db.QueryRow(peerGrantSelect+` WHERE id = ?`, rowID))
	return notFoundAs(found, err, ErrPeerGrantNotFound)
}

// GetPeerGrantByTokenHash returns the grant whose token hashes to tokenHash,
// revoked or not; the caller decides what revoked means.
func (s *Store) GetPeerGrantByTokenHash(tokenHash string) (PeerGrant, error) {
	found, err := scanPeerGrant(s.db.QueryRow(peerGrantSelect+` WHERE token_hash = ?`, tokenHash))
	return notFoundAs(found, err, ErrPeerGrantNotFound)
}

// LivePeerGrantOfConnection returns the connection's grant that is not
// revoked, or ErrPeerGrantNotFound.
func (s *Store) LivePeerGrantOfConnection(connectionID string) (PeerGrant, error) {
	found, err := scanPeerGrant(s.db.QueryRow(peerGrantSelect+` WHERE connection_id = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1`, connectionID))
	return notFoundAs(found, err, ErrPeerGrantNotFound)
}

// MarkPeerGrantDelivered stamps when the other server confirmed it holds the
// token.
func (s *Store) MarkPeerGrantDelivered(id string, at time.Time) error {
	rowID, ok := rowIDOfPrefixed(peerGrantIDPrefix, id)
	if !ok {
		return ErrPeerGrantNotFound
	}
	_, err := s.db.Exec(`UPDATE peer_grants SET delivered_at = ? WHERE id = ?`, at.UTC(), rowID)
	return err
}

// RevokePeerGrant stamps one grant revoked and returns it as it now is. A
// grant revoked before is ErrPeerGrantAlreadyRevoked.
func (s *Store) RevokePeerGrant(id, revokedBy, reason string, revokedAt time.Time) (PeerGrant, error) {
	rowID, ok := rowIDOfPrefixed(peerGrantIDPrefix, id)
	if !ok {
		return PeerGrant{}, ErrPeerGrantNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_grants SET revoked_at = ?, revoked_by = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
		revokedAt.UTC(), revokedBy, reason, rowID)
	if err != nil {
		return PeerGrant{}, fmt.Errorf("revoke peer grant %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return PeerGrant{}, err
	}
	grant, err := s.GetPeerGrant(id)
	if err != nil {
		return PeerGrant{}, err
	}
	if changed == 0 {
		return grant, ErrPeerGrantAlreadyRevoked
	}
	return grant, nil
}

// ListPeerGrants returns every grant, newest first, revoked or not.
func (s *Store) ListPeerGrants() ([]PeerGrant, error) {
	rows, err := s.db.Query(peerGrantSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []PeerGrant{}
	for rows.Next() {
		grant, err := scanPeerGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// InsertPeerAccess records access another server delivered and sets its ID.
// Access the connection held before is ended in the same transaction, with
// the reason that it was replaced, and returned so the caller can delete its
// credential.
func (s *Store) InsertPeerAccess(access *PeerAccess) (replaced []PeerAccess, err error) {
	if access.ConnectionID == "" || access.RemoteGrantID == "" || access.RemotePeerPrincipalID == "" || access.Level == "" || access.CredentialID == "" {
		return nil, fmt.Errorf("insert peer access: connection, remote grant, remote principal, level and credential are all required")
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	rows, err := transaction.Query(peerAccessSelect+` WHERE connection_id = ? AND ended_at IS NULL`, access.ConnectionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		previous, err := scanPeerAccess(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		replaced = append(replaced, previous)
	}
	rows.Close()
	reason := "replaced by " + access.RemoteGrantID
	if _, err := transaction.Exec(`UPDATE peer_access SET ended_at = ?, end_reason = ? WHERE connection_id = ? AND ended_at IS NULL`,
		access.ReceivedAt.UTC(), reason, access.ConnectionID); err != nil {
		return nil, err
	}
	result, err := transaction.Exec(`INSERT INTO peer_access (connection_id, remote_grant_id, remote_peer_principal_id, level, credential_id, received_at)
		VALUES (?, ?, ?, ?, ?, ?)`, access.ConnectionID, access.RemoteGrantID, access.RemotePeerPrincipalID, access.Level, access.CredentialID, access.ReceivedAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("insert peer access on %s: %w", access.ConnectionID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	access.ID = prefixedIDOf(peerAccessIDPrefix, rowID)
	return replaced, nil
}

// LivePeerAccessOfConnection returns the access the connection holds now, or
// ErrPeerAccessNotFound.
func (s *Store) LivePeerAccessOfConnection(connectionID string) (PeerAccess, error) {
	found, err := scanPeerAccess(s.db.QueryRow(peerAccessSelect+` WHERE connection_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, connectionID))
	return notFoundAs(found, err, ErrPeerAccessNotFound)
}

// EndPeerAccess stamps the connection's live access ended, when it is the
// access remoteGrantID made (or any live access, when remoteGrantID is
// empty), and returns what it ended. Nothing live is ErrPeerAccessNotFound.
func (s *Store) EndPeerAccess(connectionID, remoteGrantID, reason string, at time.Time) (PeerAccess, error) {
	access, err := s.LivePeerAccessOfConnection(connectionID)
	if err != nil {
		return PeerAccess{}, err
	}
	if remoteGrantID != "" && access.RemoteGrantID != remoteGrantID {
		return PeerAccess{}, fmt.Errorf("%w: %s holds %s, not %s", ErrPeerAccessNotFound, connectionID, access.RemoteGrantID, remoteGrantID)
	}
	rowID, _ := rowIDOfPrefixed(peerAccessIDPrefix, access.ID)
	if _, err := s.db.Exec(`UPDATE peer_access SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL`, at.UTC(), reason, rowID); err != nil {
		return PeerAccess{}, err
	}
	ended := at.UTC()
	access.EndedAt = &ended
	access.EndReason = reason
	return access, nil
}

// PeerAccessLevelGroup returns the group this server provisioned for a level,
// or ErrPeerAccessLevelNotProvisioned.
func (s *Store) PeerAccessLevelGroup(levelKey string) (string, error) {
	var groupID string
	err := s.db.QueryRow(`SELECT group_principal_id FROM peer_access_levels WHERE level_key = ?`, levelKey).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPeerAccessLevelNotProvisioned
	}
	return groupID, err
}

// RecordPeerAccessLevelGroup records the group a level's provisioning made.
// A level provisioned before keeps its group: the second call is an error.
func (s *Store) RecordPeerAccessLevelGroup(levelKey, groupPrincipalID, provisionedBy string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO peer_access_levels (level_key, group_principal_id, provisioned_by, provisioned_at) VALUES (?, ?, ?, ?)`,
		levelKey, groupPrincipalID, provisionedBy, at.UTC())
	if err != nil {
		return fmt.Errorf("record the group of peer access level %s: %w", levelKey, err)
	}
	return nil
}

const (
	peerConnectionInviteSelect = `SELECT id, passphrase_hash, created_by, created_at, expires_at, redeemed_at, connection_id FROM peer_connection_invites`
	peerConnectionSelect       = `SELECT id, display_name, transport, role, remote_base_url, remote_connection_id, inbound_credential_hash,
		outbound_credential_id, peer_principal_id, created_by, created_at, removed_at, removed_by, remove_reason, removed_by_other_server, removal_notice,
		other_server_name, other_server_name_reported_at FROM peer_connections`
	peerGrantSelect = `SELECT id, connection_id, token_hash, peer_principal_id, level, group_principal_id, created_by, created_at,
		delivered_at, revoked_at, revoked_by, revoke_reason FROM peer_grants`
	peerAccessSelect = `SELECT id, connection_id, remote_grant_id, remote_peer_principal_id, level, credential_id, received_at, ended_at, end_reason FROM peer_access`
)

// notFoundAs turns sql.ErrNoRows into the store's own not-found error.
func notFoundAs[T any](value T, err error, notFound error) (T, error) {
	if errors.Is(err, sql.ErrNoRows) {
		var zero T
		return zero, notFound
	}
	return value, err
}

func scanPeerConnectionInvite(row rowScanner) (PeerConnectionInvite, error) {
	var invite PeerConnectionInvite
	var rowID int64
	var redeemedAt sql.NullTime
	if err := row.Scan(&rowID, &invite.PassphraseHash, &invite.CreatedBy, &invite.CreatedAt, &invite.ExpiresAt, &redeemedAt, &invite.ConnectionID); err != nil {
		return PeerConnectionInvite{}, err
	}
	invite.ID = prefixedIDOf(peerConnectionInviteIDPrefix, rowID)
	invite.RedeemedAt = timeOrNil(redeemedAt)
	return invite, nil
}

func scanPeerConnection(row rowScanner) (PeerConnection, error) {
	var connection PeerConnection
	var rowID int64
	var transport, role string
	var removedAt, otherServerNameReportedAt sql.NullTime
	if err := row.Scan(&rowID, &connection.DisplayName, &transport, &role, &connection.RemoteBaseURL, &connection.RemoteConnectionID,
		&connection.InboundCredentialHash, &connection.OutboundCredentialID, &connection.PeerPrincipalID, &connection.CreatedBy, &connection.CreatedAt,
		&removedAt, &connection.RemovedBy, &connection.RemoveReason, &connection.RemovedByOtherServer, &connection.RemovalNotice,
		&connection.OtherServerName, &otherServerNameReportedAt); err != nil {
		return PeerConnection{}, err
	}
	connection.OtherServerNameReportedAt = timeOrNil(otherServerNameReportedAt)
	connection.ID = prefixedIDOf(peerConnectionIDPrefix, rowID)
	connection.Transport = msg.PeerTransport(transport)
	connection.Role = msg.PeerConnectionRole(role)
	connection.RemovedAt = timeOrNil(removedAt)
	return connection, nil
}

func scanPeerGrant(row rowScanner) (PeerGrant, error) {
	var grant PeerGrant
	var rowID int64
	var deliveredAt, revokedAt sql.NullTime
	if err := row.Scan(&rowID, &grant.ConnectionID, &grant.TokenHash, &grant.PeerPrincipalID, &grant.Level, &grant.GroupPrincipalID, &grant.CreatedBy,
		&grant.CreatedAt, &deliveredAt, &revokedAt, &grant.RevokedBy, &grant.RevokeReason); err != nil {
		return PeerGrant{}, err
	}
	grant.ID = prefixedIDOf(peerGrantIDPrefix, rowID)
	grant.DeliveredAt = timeOrNil(deliveredAt)
	grant.RevokedAt = timeOrNil(revokedAt)
	return grant, nil
}

func scanPeerAccess(row rowScanner) (PeerAccess, error) {
	var access PeerAccess
	var rowID int64
	var endedAt sql.NullTime
	if err := row.Scan(&rowID, &access.ConnectionID, &access.RemoteGrantID, &access.RemotePeerPrincipalID, &access.Level, &access.CredentialID,
		&access.ReceivedAt, &endedAt, &access.EndReason); err != nil {
		return PeerAccess{}, err
	}
	access.ID = prefixedIDOf(peerAccessIDPrefix, rowID)
	access.EndedAt = timeOrNil(endedAt)
	return access, nil
}

func timeOrNil(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	moment := value.Time
	return &moment
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
