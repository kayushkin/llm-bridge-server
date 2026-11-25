package store

// This server's own id: msg.ServerIDPrefix and a ULID, made at the first open
// that finds none and never changed. Connected servers record it to tell this
// server from every other, so a new id would read to them as a different
// server. The import of a bridge database copies the id it held.

import (
	"fmt"
	"time"

	serverids "github.com/kayushkin/llm-bridge-server/internal/ids"
)

func (s *Store) openServerIdentity() error {
	if _, err := s.db.Exec(`INSERT INTO server_identity (only_row, server_id, created_at) VALUES (1, $1, $2) ON CONFLICT (only_row) DO NOTHING`,
		serverids.NewServerID(), time.Now().UTC()); err != nil {
		return err
	}
	if err := s.db.QueryRow(`SELECT server_id FROM server_identity WHERE only_row = 1`).Scan(&s.serverID); err != nil {
		return err
	}
	if !serverids.IsServerID(s.serverID) {
		return fmt.Errorf("server_identity holds %q, which is not a server id", s.serverID)
	}
	return nil
}

// ThisServerID is this server's own id.
func (s *Store) ThisServerID() string { return s.serverID }
