package harnessstore

import (
	"database/sql"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const machineRowCols = `id, name, emoji, hostname, os, arch, transport, ssh_user, ssh_key_path, ssh_port, default_working_dir, user, notes, computer_use_allowed, last_seen_at, created_at, updated_at`

func scanMachine(row interface{ Scan(dest ...any) error }) (*msg.Machine, error) {
	var m msg.Machine
	var lastSeen sql.NullTime
	if err := row.Scan(&m.ID, &m.Name, &m.Emoji, &m.Hostname, &m.OS, &m.Arch, &m.Transport,
		&m.SSHUser, &m.SSHKeyPath, &m.SSHPort, &m.DefaultWorkingDir, &m.User, &m.Notes,
		&m.ComputerUseAllowed, &lastSeen, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	if lastSeen.Valid {
		m.LastSeenAt = lastSeen.Time
	}
	return &m, nil
}

// CreateMachine inserts a machine row. The runner_token_hash is set
// separately via SetMachineRunnerTokenHash (only meaningful for
// transport=runner machines).
func (s *Store) CreateMachine(m *msg.Machine) error {
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	if m.SSHPort == 0 {
		m.SSHPort = 22
	}
	if m.Transport == "" {
		m.Transport = msg.TransportLocal
	}
	_, err := s.db.Exec(`
		INSERT INTO machines (id, name, emoji, hostname, os, arch, transport, ssh_user, ssh_key_path, ssh_port, default_working_dir, user, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Name, m.Emoji, m.Hostname, m.OS, m.Arch, m.Transport,
		m.SSHUser, m.SSHKeyPath, m.SSHPort, m.DefaultWorkingDir, m.User, m.Notes,
		m.CreatedAt, m.UpdatedAt,
	)
	return err
}

// GetMachine returns a machine by ID.
func (s *Store) GetMachine(id string) (*msg.Machine, error) {
	row := s.db.QueryRow(`SELECT `+machineRowCols+` FROM machines WHERE id = ?`, id)
	return scanMachine(row)
}

// GetMachineByName returns a machine by its unique Name.
func (s *Store) GetMachineByName(name string) (*msg.Machine, error) {
	row := s.db.QueryRow(`SELECT `+machineRowCols+` FROM machines WHERE name = ?`, name)
	return scanMachine(row)
}

// ListMachines returns every machine, ordered by name.
func (s *Store) ListMachines() ([]msg.Machine, error) {
	rows, err := s.db.Query(`SELECT ` + machineRowCols + ` FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []msg.Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// UpdateMachine updates the mutable fields of a machine. The
// runner_token_hash is updated separately via SetMachineRunnerTokenHash.
func (s *Store) UpdateMachine(m *msg.Machine) error {
	m.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(`
		UPDATE machines SET name=?, emoji=?, hostname=?, os=?, arch=?, transport=?, ssh_user=?, ssh_key_path=?, ssh_port=?, default_working_dir=?, user=?, notes=?, updated_at=?
		WHERE id=?`,
		m.Name, m.Emoji, m.Hostname, m.OS, m.Arch, m.Transport,
		m.SSHUser, m.SSHKeyPath, m.SSHPort, m.DefaultWorkingDir, m.User, m.Notes,
		m.UpdatedAt, m.ID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetMachineComputerUseAllowed sets the owner's computer-use switch. It has
// its own setter, outside UpdateMachine, so an edit of a machine's other
// fields can never turn computer use on or off by carrying a stale value.
func (s *Store) SetMachineComputerUseAllowed(id string, allowed bool) error {
	res, err := s.db.Exec(`UPDATE machines SET computer_use_allowed=?, updated_at=? WHERE id=?`,
		allowed, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteMachine removes a machine. The instances FK cascades, so all
// instances bound to this machine are removed as well.
func (s *Store) DeleteMachine(id string) error {
	res, err := s.db.Exec(`DELETE FROM machines WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetMachineRunnerTokenHash records the durable runner token's hash for
// this machine. Future runner WS connects validate against this column.
func (s *Store) SetMachineRunnerTokenHash(id, hash string) error {
	res, err := s.db.Exec(`UPDATE machines SET runner_token_hash=?, updated_at=? WHERE id=?`,
		hash, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetMachineByRunnerTokenHash returns the machine whose runner_token_hash
// matches the given value, or sql.ErrNoRows. Used by the WS auth path.
func (s *Store) GetMachineByRunnerTokenHash(hash string) (*msg.Machine, error) {
	if hash == "" {
		return nil, sql.ErrNoRows
	}
	row := s.db.QueryRow(`SELECT `+machineRowCols+` FROM machines WHERE runner_token_hash = ?`, hash)
	return scanMachine(row)
}

// TouchMachineLastSeen records a successful runner connection time.
func (s *Store) TouchMachineLastSeen(id string) error {
	_, err := s.db.Exec(`UPDATE machines SET last_seen_at=? WHERE id=?`, time.Now().UTC(), id)
	return err
}
