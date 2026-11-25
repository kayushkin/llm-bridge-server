package harnessstore

import (
	"database/sql"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// CreateInstance registers a new harness instance bound to a machine.
// MachineID must reference an existing machines row.
func (s *Store) CreateInstance(inst *msg.Instance) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	inst.CreatedAt = now
	inst.UpdatedAt = now
	if inst.MaxConcurrentSessions == 0 {
		inst.MaxConcurrentSessions = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO instances (id, harness_type, name, machine_id, working_dir, max_concurrent_sessions, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		inst.ID, inst.HarnessType, inst.Name, inst.MachineID, inst.WorkingDir,
		inst.MaxConcurrentSessions, inst.Enabled, inst.CreatedAt, inst.UpdatedAt,
	)
	return err
}

// instanceRowCols is the SELECT clause used by every instance query so
// scanning stays consistent.
const instanceRowCols = `id, harness_type, name, machine_id, working_dir, max_concurrent_sessions, enabled, created_at, updated_at`

// instanceRowColsOfInstancesTable is instanceRowCols named by table, for a
// query that joins machines.
const instanceRowColsOfInstancesTable = `instances.id, instances.harness_type, instances.name, instances.machine_id, instances.working_dir,
	instances.max_concurrent_sessions, instances.enabled, instances.created_at, instances.updated_at`

func instanceScanDestinations(inst *msg.Instance) []any {
	return []any{&inst.ID, &inst.HarnessType, &inst.Name, &inst.MachineID, &inst.WorkingDir,
		&inst.MaxConcurrentSessions, &inst.Enabled, &inst.CreatedAt, &inst.UpdatedAt}
}

func finishScannedInstance(inst *msg.Instance) {
	inUTC(&inst.CreatedAt)
	inUTC(&inst.UpdatedAt)
}

func scanInstance(row interface{ Scan(dest ...any) error }) (*msg.Instance, error) {
	var inst msg.Instance
	if err := row.Scan(instanceScanDestinations(&inst)...); err != nil {
		return nil, err
	}
	finishScannedInstance(&inst)
	return &inst, nil
}

// GetInstance retrieves an instance by ID. The Machine field is left nil;
// callers that want it should call GetMachine separately or use
// GetInstanceWithMachine.
func (s *Store) GetInstance(id string) (*msg.Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceRowCols+` FROM instances WHERE id = $1`, id)
	return scanInstance(row)
}

// GetInstanceByName retrieves an instance by name.
func (s *Store) GetInstanceByName(name string) (*msg.Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceRowCols+` FROM instances WHERE name = $1`, name)
	return scanInstance(row)
}

// GetInstanceWithMachine joins instances with machines so the caller
// receives the instance plus its joined Machine populated. Used by API
// handlers serializing for the client. It is one query: sql.ErrNoRows when
// the instance is missing or names a machine that is.
func (s *Store) GetInstanceWithMachine(id string) (*msg.Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceRowColsOfInstancesTable+`, `+machineRowColsOfMachinesTable+`
		FROM instances JOIN machines ON machines.id = instances.machine_id WHERE instances.id = $1`, id)
	var inst msg.Instance
	machine, err := scanMachine(rowAfter(row, instanceScanDestinations(&inst)...))
	if err != nil {
		return nil, err
	}
	finishScannedInstance(&inst)
	inst.Machine = machine
	return &inst, nil
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
	return s.listInstances(`SELECT `+instanceRowCols+` FROM instances WHERE harness_type = $1 AND enabled ORDER BY name`, harnessType)
}

// ListInstancesByMachine returns all instances bound to a machine.
func (s *Store) ListInstancesByMachine(machineID string) ([]msg.Instance, error) {
	return s.listInstances(`SELECT `+instanceRowCols+` FROM instances WHERE machine_id = $1 ORDER BY name`, machineID)
}

// UpdateInstance updates an instance's mutable fields.
func (s *Store) UpdateInstance(inst *msg.Instance) error {
	inst.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	res, err := s.db.Exec(`
		UPDATE instances SET harness_type=$1, name=$2, machine_id=$3, working_dir=$4, max_concurrent_sessions=$5, enabled=$6, updated_at=$7
		WHERE id=$8`,
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
	res, err := s.db.Exec(`DELETE FROM instances WHERE id = $1`, id)
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
	res, err := s.db.Exec(`UPDATE instances SET enabled = $1, updated_at = $2 WHERE id = $3`,
		enabled, time.Now().UTC().Truncate(time.Microsecond), id)
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
	err := s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE enabled`).Scan(&count)
	return count, err
}
