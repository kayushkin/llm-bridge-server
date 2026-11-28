package machines

import (
	"database/sql"

	"github.com/kayushkin/llm-bridge/msg"
)

// BindCredential associates a credential with an instance.
// If already bound, it replaces the binding: priority, enabled, and
// max_concurrent back to its default, as SQLite's INSERT OR REPLACE did.
func (s *Store) BindCredential(ic *msg.InstanceCredential) error {
	_, err := s.db.Exec(`
		INSERT INTO instance_credentials (instance_id, credential_id, priority, enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, credential_id) DO UPDATE SET
			priority = excluded.priority, enabled = excluded.enabled, max_concurrent = DEFAULT`,
		ic.InstanceID, ic.CredentialID, ic.Priority, ic.Enabled,
	)
	return err
}

// UnbindCredential removes a credential binding from an instance.
func (s *Store) UnbindCredential(instanceID, credentialID string) error {
	res, err := s.db.Exec(`DELETE FROM instance_credentials WHERE instance_id = $1 AND credential_id = $2`,
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
		FROM instance_credentials WHERE instance_id = $1 ORDER BY priority`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []msg.InstanceCredential
	for rows.Next() {
		var ic msg.InstanceCredential
		if err := rows.Scan(&ic.InstanceID, &ic.CredentialID, &ic.Priority, &ic.Enabled); err != nil {
			return nil, err
		}
		creds = append(creds, ic)
	}
	return creds, rows.Err()
}

// ListCredentialInstances returns all instances that have a specific credential bound.
func (s *Store) ListCredentialInstances(credentialID string) ([]msg.InstanceCredential, error) {
	rows, err := s.db.Query(`
		SELECT instance_id, credential_id, priority, enabled
		FROM instance_credentials WHERE credential_id = $1 ORDER BY instance_id`, credentialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []msg.InstanceCredential
	for rows.Next() {
		var ic msg.InstanceCredential
		if err := rows.Scan(&ic.InstanceID, &ic.CredentialID, &ic.Priority, &ic.Enabled); err != nil {
			return nil, err
		}
		bindings = append(bindings, ic)
	}
	return bindings, rows.Err()
}

// GetCredentialBinding returns a specific credential binding.
func (s *Store) GetCredentialBinding(instanceID, credentialID string) (*msg.InstanceCredential, error) {
	var ic msg.InstanceCredential
	err := s.db.QueryRow(`
		SELECT instance_id, credential_id, priority, enabled
		FROM instance_credentials WHERE instance_id = $1 AND credential_id = $2`,
		instanceID, credentialID,
	).Scan(&ic.InstanceID, &ic.CredentialID, &ic.Priority, &ic.Enabled)
	if err != nil {
		return nil, err
	}
	return &ic, nil
}

// SetCredentialEnabled enables or disables a credential binding.
func (s *Store) SetCredentialEnabled(instanceID, credentialID string, enabled bool) error {
	res, err := s.db.Exec(`
		UPDATE instance_credentials SET enabled = $1 WHERE instance_id = $2 AND credential_id = $3`,
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
		UPDATE instance_credentials SET priority = $1 WHERE instance_id = $2 AND credential_id = $3`,
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
	_, err := s.db.Exec(`DELETE FROM instance_credentials WHERE instance_id = $1`, instanceID)
	return err
}

// CountCredentialBindings returns the number of credential bindings for an instance.
func (s *Store) CountCredentialBindings(instanceID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM instance_credentials WHERE instance_id = $1`, instanceID).Scan(&count)
	return count, err
}

// FirstEnabledCredentialID is the highest-priority enabled credential bound
// to an instance, or empty when none is.
func (s *Store) FirstEnabledCredentialID(instanceID string) string {
	bindings, err := s.ListInstanceCredentials(instanceID)
	if err != nil || len(bindings) == 0 {
		return ""
	}
	for _, b := range bindings {
		if b.Enabled {
			return b.CredentialID
		}
	}
	return ""
}
