package store

// Phase 2 of linked servers (design note d107154a): a connected server names
// which of its users a request is for, and this server acts for that user as
// one of its own principals, mapped by its administrator.
//
// peer_user_mappings says which local principal a (peer principal, remote
// principal) pair acts as. It joins on ids only: the peer principal this
// server made for the other server, and the other server's own id for its
// user, which is compared and never looked up here.
//
// session_peer_origins says a session was started through a peer for one of
// its users. Its session belongs to the mapped local principal, and the row
// keeps the peer, so the session's grants are the intersection of both and
// only that peer's requests for that principal reach it.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const peerUserMappingIDPrefix = "peer_user_mapping_"

// ErrPeerUserMappingNotFound is a pair or id with no live mapping.
var ErrPeerUserMappingNotFound = errors.New("peer user mapping not found")

// ErrPeerUserMappingExists is a second live mapping for one pair.
var ErrPeerUserMappingExists = errors.New("a live mapping for this peer and remote principal already exists")

// PeerUserMapping is one row of peer_user_mappings.
type PeerUserMapping = msg.PeerUserMapping

// SessionPeerOrigin is one row of session_peer_origins.
type SessionPeerOrigin struct {
	SessionID         string
	PeerPrincipalID   string
	RemotePrincipalID string
}

const peerUserMappingSelect = `SELECT id, peer_principal_id, remote_principal_id, local_principal_id, created_by, created_at, removed_at, removed_by, remove_reason FROM peer_user_mappings`

func scanPeerUserMapping(row rowScanner) (PeerUserMapping, error) {
	var mapping PeerUserMapping
	var rowID int64
	var removedAt sql.NullTime
	if err := row.Scan(&rowID, &mapping.PeerPrincipalID, &mapping.RemotePrincipalID, &mapping.LocalPrincipalID, &mapping.CreatedBy, &mapping.CreatedAt,
		&removedAt, &mapping.RemovedBy, &mapping.RemoveReason); err != nil {
		return PeerUserMapping{}, err
	}
	mapping.ID = prefixedIDOf(peerUserMappingIDPrefix, rowID)
	mapping.RemovedAt = timeOrNil(removedAt)
	return mapping, nil
}

// InsertPeerUserMapping records a mapping and sets its ID and CreatedAt.
func (s *Store) InsertPeerUserMapping(mapping *PeerUserMapping) error {
	if mapping.PeerPrincipalID == "" || mapping.RemotePrincipalID == "" || mapping.LocalPrincipalID == "" || mapping.CreatedBy == "" {
		return fmt.Errorf("insert peer user mapping: peer principal, remote principal, local principal and creator are all required")
	}
	mapping.CreatedAt = time.Now().UTC()
	result, err := s.db.Exec(`INSERT INTO peer_user_mappings (peer_principal_id, remote_principal_id, local_principal_id, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		mapping.PeerPrincipalID, mapping.RemotePrincipalID, mapping.LocalPrincipalID, mapping.CreatedBy, mapping.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrPeerUserMappingExists
		}
		return fmt.Errorf("insert peer user mapping: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	mapping.ID = prefixedIDOf(peerUserMappingIDPrefix, rowID)
	return nil
}

// LivePeerUserMapping returns the live mapping for a peer principal and the
// other server's id for its user.
func (s *Store) LivePeerUserMapping(peerPrincipalID, remotePrincipalID string) (PeerUserMapping, error) {
	found, err := scanPeerUserMapping(s.db.QueryRow(peerUserMappingSelect+` WHERE peer_principal_id = ? AND remote_principal_id = ? AND removed_at IS NULL`,
		peerPrincipalID, remotePrincipalID))
	return notFoundAs(found, err, ErrPeerUserMappingNotFound)
}

// ListPeerUserMappings returns every mapping, newest first, removed or not,
// narrowed to one peer principal when peerPrincipalID is set.
func (s *Store) ListPeerUserMappings(peerPrincipalID string) ([]PeerUserMapping, error) {
	query := peerUserMappingSelect
	var args []any
	if peerPrincipalID != "" {
		query += ` WHERE peer_principal_id = ?`
		args = append(args, peerPrincipalID)
	}
	rows, err := s.db.Query(query+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mappings := []PeerUserMapping{}
	for rows.Next() {
		mapping, err := scanPeerUserMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, rows.Err()
}

// RemovePeerUserMapping ends a live mapping.
func (s *Store) RemovePeerUserMapping(id, removedBy, reason string) (PeerUserMapping, error) {
	rowID, ok := rowIDOfPrefixed(peerUserMappingIDPrefix, id)
	if !ok {
		return PeerUserMapping{}, ErrPeerUserMappingNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_user_mappings SET removed_at = ?, removed_by = ?, remove_reason = ? WHERE id = ? AND removed_at IS NULL`,
		time.Now().UTC(), removedBy, reason, rowID)
	if err != nil {
		return PeerUserMapping{}, fmt.Errorf("remove peer user mapping %s: %w", id, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return PeerUserMapping{}, err
	} else if changed == 0 {
		return PeerUserMapping{}, ErrPeerUserMappingNotFound
	}
	found, err := scanPeerUserMapping(s.db.QueryRow(peerUserMappingSelect+` WHERE id = ?`, rowID))
	return notFoundAs(found, err, ErrPeerUserMappingNotFound)
}

// RecordSessionPeerOrigin says a session was started through a peer for one
// of its users. A session has at most one origin, set once.
func (s *Store) RecordSessionPeerOrigin(origin SessionPeerOrigin) error {
	if origin.SessionID == "" || origin.PeerPrincipalID == "" || origin.RemotePrincipalID == "" {
		return fmt.Errorf("record session peer origin: session, peer principal and remote principal are all required")
	}
	_, err := s.db.Exec(`INSERT INTO session_peer_origins (session_id, peer_principal_id, remote_principal_id) VALUES (?, ?, ?)`,
		origin.SessionID, origin.PeerPrincipalID, origin.RemotePrincipalID)
	if err != nil {
		return fmt.Errorf("record session %s's peer origin: %w", origin.SessionID, err)
	}
	return nil
}

// SessionPeerOriginOf returns the session's peer origin, and false when it
// was not started through a peer for one of its users.
func (s *Store) SessionPeerOriginOf(sessionID string) (SessionPeerOrigin, bool, error) {
	origin := SessionPeerOrigin{SessionID: sessionID}
	err := s.dbRO.QueryRow(`SELECT peer_principal_id, remote_principal_id FROM session_peer_origins WHERE session_id = ?`, sessionID).
		Scan(&origin.PeerPrincipalID, &origin.RemotePrincipalID)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionPeerOrigin{}, false, nil
	}
	if err != nil {
		return SessionPeerOrigin{}, false, err
	}
	return origin, true, nil
}

// CopySessionPeerOrigin gives toSessionID the peer origin fromSessionID's
// lineage has, if any, so a session made from one started through a peer — a
// fork, a subagent, a delegate, a button's new session — stays limited by that
// peer and stays visible to the same user. It reports whether there was one.
func (s *Store) CopySessionPeerOrigin(fromSessionID, toSessionID string) (bool, error) {
	origin, found, err := s.SessionPeerOriginOfLineage(fromSessionID)
	if err != nil || !found {
		return false, err
	}
	origin.SessionID = toSessionID
	return true, s.RecordSessionPeerOrigin(origin)
}

// sessionLineageLimit bounds the walk up a session's lineage.
const sessionLineageLimit = 64

// SessionPeerOriginOfLineage is SessionPeerOriginOf for the session or, when
// it has none, the nearest session it was made from — its manager, the session
// it was forked from or the one it continues after a refresh — that has the
// same principal. Grant decisions read this rather than the session's own row,
// so a session some path made without copying the origin still cannot shed
// the peer's limits.
func (s *Store) SessionPeerOriginOfLineage(sessionID string) (SessionPeerOrigin, bool, error) {
	current := sessionID
	principalID := ""
	for step := 0; step < sessionLineageLimit && current != ""; step++ {
		origin, found, err := s.SessionPeerOriginOf(current)
		if err != nil || found {
			return origin, found, err
		}
		session, err := s.GetSession(current)
		if errors.Is(err, sql.ErrNoRows) {
			return SessionPeerOrigin{}, false, nil
		}
		if err != nil {
			return SessionPeerOrigin{}, false, fmt.Errorf("read session %s on the way up from %s: %w", current, sessionID, err)
		}
		if step == 0 {
			principalID = session.PrincipalID
		} else if session.PrincipalID != principalID {
			return SessionPeerOrigin{}, false, nil
		}
		switch {
		case session.ManagerSessionID != "":
			current = session.ManagerSessionID
		case session.ForkedFromSessionID != "":
			current = session.ForkedFromSessionID
		case session.RefreshedFromSessionID != "":
			current = session.RefreshedFromSessionID
		default:
			current = ""
		}
	}
	return SessionPeerOrigin{}, false, nil
}
