package harnessstore

import (
	"database/sql"

	"github.com/kayushkin/llm-bridge/msg"
)

// BindCredential associates a credential with an instance.
// If already bound, updates priority.
func (s *Store) BindCredential(ic *msg.InstanceCredential) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO instance_credentials (instance_id, credential_id, priority, enabled)
		VALUES (?, ?, ?, ?)`,
		ic.InstanceID, ic.CredentialID, ic.Priority, ic.Enabled,
	)
	return err
}

// UnbindCredential removes a credential binding from an instance.
func (s *Store) UnbindCredential(instanceID, credentialID string) error {
	res, err := s.db.Exec(`DELETE FROM instance_credentials WHERE instance_id = ? AND credential_id = ?`,
		instanceID, credentialID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListInstanceCredentials returns all credential bindings for an instance, ordered by priority.
func (s *Store) ListInstanceCredentials(instanceID string) ([]msg.InstanceCredential, error) {
	rows, err := s.db.Query(`
		SELECT instance_id, credential_id, priority, enabled
		FROM instance_credentials WHERE instance_id = ? ORDER BY priority`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []msg.InstanceCredential
	for rows.Next() {
		var ic msg.InstanceCredential
		var enabled int
		if err := rows.Scan(&ic.InstanceID, &ic.CredentialID, &ic.Priority, &enabled); err != nil {
			return nil, err
		}
		ic.Enabled = enabled != 0
		creds = append(creds, ic)
	}
	return creds, rows.Err()
}

// ListCredentialInstances returns all instances that have a specific credential bound.
func (s *Store) ListCredentialInstances(credentialID string) ([]msg.InstanceCredential, error) {
	rows, err := s.db.Query(`
		SELECT instance_id, credential_id, priority, enabled
		FROM instance_credentials WHERE credential_id = ? ORDER BY instance_id`, credentialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []msg.InstanceCredential
	for rows.Next() {
		var ic msg.InstanceCredential
		var enabled int
		if err := rows.Scan(&ic.InstanceID, &ic.CredentialID, &ic.Priority, &enabled); err != nil {
			return nil, err
		}
		ic.Enabled = enabled != 0
		bindings = append(bindings, ic)
	}
	return bindings, rows.Err()
}

// GetCredentialBinding returns a specific credential binding.
func (s *Store) GetCredentialBinding(instanceID, credentialID string) (*msg.InstanceCredential, error) {
	var ic msg.InstanceCredential
	var enabled int
	err := s.db.QueryRow(`
		SELECT instance_id, credential_id, priority, enabled
		FROM instance_credentials WHERE instance_id = ? AND credential_id = ?`,
		instanceID, credentialID,
	).Scan(&ic.InstanceID, &ic.CredentialID, &ic.Priority, &enabled)
	if err != nil {
		return nil, err
	}
	ic.Enabled = enabled != 0
	return &ic, nil
}

// SetCredentialEnabled enables or disables a credential binding.
func (s *Store) SetCredentialEnabled(instanceID, credentialID string, enabled bool) error {
	res, err := s.db.Exec(`
		UPDATE instance_credentials SET enabled = ? WHERE instance_id = ? AND credential_id = ?`,
		enabled, instanceID, credentialID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateCredentialPriority updates the priority of a credential binding.
func (s *Store) UpdateCredentialPriority(instanceID, credentialID string, priority int) error {
	res, err := s.db.Exec(`
		UPDATE instance_credentials SET priority = ? WHERE instance_id = ? AND credential_id = ?`,
		priority, instanceID, credentialID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ClearInstanceCredentials removes all credential bindings for an instance.
func (s *Store) ClearInstanceCredentials(instanceID string) error {
	_, err := s.db.Exec(`DELETE FROM instance_credentials WHERE instance_id = ?`, instanceID)
	return err
}

// CountCredentialBindings returns the number of credential bindings for an instance.
func (s *Store) CountCredentialBindings(instanceID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM instance_credentials WHERE instance_id = ?`, instanceID).Scan(&count)
	return count, err
}
