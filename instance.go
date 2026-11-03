package harnessstore

import (
	"database/sql"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// CreateInstance registers a new harness instance bound to a machine.
// MachineID must reference an existing machines row.
func (s *Store) CreateInstance(inst *msg.Instance) error {
	now := time.Now().UTC()
	inst.CreatedAt = now
	inst.UpdatedAt = now
	if inst.MaxConcurrentSessions == 0 {
		inst.MaxConcurrentSessions = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO instances (id, harness_type, name, machine_id, working_dir, max_concurrent_sessions, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inst.ID, inst.HarnessType, inst.Name, inst.MachineID, inst.WorkingDir,
		inst.MaxConcurrentSessions, inst.Enabled, inst.CreatedAt, inst.UpdatedAt,
	)
	return err
}

// instanceRowCols is the SELECT clause used by every instance query so
// scanning stays consistent.
const instanceRowCols = `id, harness_type, name, machine_id, working_dir, max_concurrent_sessions, enabled, created_at, updated_at`

func scanInstance(row interface{ Scan(dest ...any) error }) (*msg.Instance, error) {
	var inst msg.Instance
	var enabled int
	if err := row.Scan(&inst.ID, &inst.HarnessType, &inst.Name, &inst.MachineID, &inst.WorkingDir,
		&inst.MaxConcurrentSessions, &enabled, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
		return nil, err
	}
	inst.Enabled = enabled != 0
	return &inst, nil
}

// GetInstance retrieves an instance by ID. The Machine field is left nil;
// callers that want it should call GetMachine separately or use
// GetInstanceWithMachine.
func (s *Store) GetInstance(id string) (*msg.Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceRowCols+` FROM instances WHERE id = ?`, id)
	return scanInstance(row)
}

// GetInstanceByName retrieves an instance by name.
func (s *Store) GetInstanceByName(name string) (*msg.Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceRowCols+` FROM instances WHERE name = ?`, name)
	return scanInstance(row)
}

// GetInstanceWithMachine joins instances with machines so the caller
// receives the instance plus its joined Machine populated. Used by API
// handlers serializing for the client.
func (s *Store) GetInstanceWithMachine(id string) (*msg.Instance, error) {
	inst, err := s.GetInstance(id)
	if err != nil {
		return nil, err
	}
	m, err := s.GetMachine(inst.MachineID)
	if err != nil {
		return nil, err
	}
	inst.Machine = m
	return inst, nil
}

// listInstances is the shared body for the list-style queries.
func (s *Store) listInstances(query string, args ...any) ([]msg.Instance, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []msg.Instance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, *inst)
	}
	return instances, rows.Err()
}

// ListInstances returns all registered instances (no Machine populated).
func (s *Store) ListInstances() ([]msg.Instance, error) {
	return s.listInstances(`SELECT ` + instanceRowCols + ` FROM instances ORDER BY name`)
}

// ListInstancesByHarness returns enabled instances for a specific harness type.
func (s *Store) ListInstancesByHarness(harnessType msg.Harness) ([]msg.Instance, error) {
	return s.listInstances(`SELECT `+instanceRowCols+` FROM instances WHERE harness_type = ? AND enabled = 1 ORDER BY name`, harnessType)
}

// ListInstancesByMachine returns all instances bound to a machine.
func (s *Store) ListInstancesByMachine(machineID string) ([]msg.Instance, error) {
	return s.listInstances(`SELECT `+instanceRowCols+` FROM instances WHERE machine_id = ? ORDER BY name`, machineID)
}

// UpdateInstance updates an instance's mutable fields.
func (s *Store) UpdateInstance(inst *msg.Instance) error {
	inst.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(`
		UPDATE instances SET harness_type=?, name=?, machine_id=?, working_dir=?, max_concurrent_sessions=?, enabled=?, updated_at=?
		WHERE id=?`,
		inst.HarnessType, inst.Name, inst.MachineID, inst.WorkingDir,
		inst.MaxConcurrentSessions, inst.Enabled, inst.UpdatedAt, inst.ID,
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

// DeleteInstance removes an instance. Cascading FK on instance_credentials
// is not declared; callers that need that behavior should call
// ClearInstanceCredentials first.
func (s *Store) DeleteInstance(id string) error {
	res, err := s.db.Exec(`DELETE FROM instances WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetInstanceEnabled enables or disables an instance.
func (s *Store) SetInstanceEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE instances SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountInstances returns the total number of instances.
func (s *Store) CountInstances() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM instances`).Scan(&count)
	return count, err
}

// CountEnabledInstances returns the number of enabled instances.
func (s *Store) CountEnabledInstances() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE enabled = 1`).Scan(&count)
	return count, err
}
