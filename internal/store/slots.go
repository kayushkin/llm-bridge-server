package store

import (
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Credential slot tracking - runtime state for active credential usage.
// Static config (instances, credential bindings) lives in harness-store.

func (s *Store) migrateSlots() error {
	_, err := s.db.Exec(`
		-- Runtime credential slot tracking
		CREATE TABLE IF NOT EXISTS credential_slots (
			instance_id   TEXT NOT NULL,
			credential_id TEXT NOT NULL,
			session_id    TEXT NOT NULL,
			acquired_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id)
		);
		CREATE INDEX IF NOT EXISTS idx_cred_slots_instance ON credential_slots(instance_id);
		CREATE INDEX IF NOT EXISTS idx_cred_slots_cred ON credential_slots(credential_id);
	`)
	return err
}

// AcquireCredentialSlot reserves a credential slot for a session.
// credentialBindings should come from harness-store.ListInstanceCredentials().
// Returns the credential ID acquired, or error if no slots available.
func (s *Store) AcquireCredentialSlot(instanceID, sessionID string, credentialBindings []msg.InstanceCredential) (string, error) {
	// Find highest-priority credential with available slots
	for _, binding := range credentialBindings {
		if !binding.Enabled {
			continue
		}

		// Count current usage of this credential on this instance
		var inUse int
		err := s.db.QueryRow(`
			SELECT COUNT(*) FROM credential_slots
			WHERE instance_id = ? AND credential_id = ?`,
			instanceID, binding.CredentialID).Scan(&inUse)
		if err != nil {
			return "", err
		}

		if inUse < binding.MaxConcurrent {
			// Reserve the slot
			_, err = s.db.Exec(`
				INSERT INTO credential_slots (instance_id, credential_id, session_id, acquired_at)
				VALUES (?, ?, ?, ?)`,
				instanceID, binding.CredentialID, sessionID, time.Now().UTC())
			if err != nil {
				return "", err
			}
			return binding.CredentialID, nil
		}
	}

	return "", fmt.Errorf("no credential slots available for instance %s", instanceID)
}

// ReleaseCredentialSlot frees a credential slot when a session ends.
func (s *Store) ReleaseCredentialSlot(sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM credential_slots WHERE session_id = ?`, sessionID)
	return err
}

// GetCredentialForSession returns the credential ID bound to a session.
func (s *Store) GetCredentialForSession(sessionID string) (string, error) {
	var credID string
	err := s.db.QueryRow(`SELECT credential_id FROM credential_slots WHERE session_id = ?`, sessionID).Scan(&credID)
	return credID, err
}

// GetSlotForSession returns full slot info for a session.
func (s *Store) GetSlotForSession(sessionID string) (*msg.CredentialSlot, error) {
	var slot msg.CredentialSlot
	err := s.db.QueryRow(`
		SELECT instance_id, credential_id, session_id, acquired_at
		FROM credential_slots WHERE session_id = ?`, sessionID,
	).Scan(&slot.InstanceID, &slot.CredentialID, &slot.SessionID, &slot.AcquiredAt)
	if err != nil {
		return nil, err
	}
	return &slot, nil
}

// ListSlotsByInstance returns all active slots for an instance.
func (s *Store) ListSlotsByInstance(instanceID string) ([]msg.CredentialSlot, error) {
	rows, err := s.db.Query(`
		SELECT instance_id, credential_id, session_id, acquired_at
		FROM credential_slots WHERE instance_id = ?`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var slots []msg.CredentialSlot
	for rows.Next() {
		var slot msg.CredentialSlot
		if err := rows.Scan(&slot.InstanceID, &slot.CredentialID, &slot.SessionID, &slot.AcquiredAt); err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

// ListSlotsByCredential returns all active slots using a specific credential.
func (s *Store) ListSlotsByCredential(credentialID string) ([]msg.CredentialSlot, error) {
	rows, err := s.db.Query(`
		SELECT instance_id, credential_id, session_id, acquired_at
		FROM credential_slots WHERE credential_id = ?`, credentialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var slots []msg.CredentialSlot
	for rows.Next() {
		var slot msg.CredentialSlot
		if err := rows.Scan(&slot.InstanceID, &slot.CredentialID, &slot.SessionID, &slot.AcquiredAt); err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

// CountSlotsByCredential returns how many sessions are using a credential on an instance.
func (s *Store) CountSlotsByCredential(instanceID, credentialID string) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM credential_slots
		WHERE instance_id = ? AND credential_id = ?`,
		instanceID, credentialID).Scan(&count)
	return count, err
}

// CountSlotsByInstance returns total active slots on an instance.
func (s *Store) CountSlotsByInstance(instanceID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM credential_slots WHERE instance_id = ?`, instanceID).Scan(&count)
	return count, err
}

// GetCredentialStatus computes availability for credentials on an instance.
// credentialBindings should come from harness-store.
func (s *Store) GetCredentialStatus(instanceID string, credentialBindings []msg.InstanceCredential) ([]msg.CredentialStatus, error) {
	var statuses []msg.CredentialStatus

	for _, binding := range credentialBindings {
		inUse, err := s.CountSlotsByCredential(instanceID, binding.CredentialID)
		if err != nil {
			return nil, err
		}

		statuses = append(statuses, msg.CredentialStatus{
			CredentialID:  binding.CredentialID,
			Priority:      binding.Priority,
			MaxConcurrent: binding.MaxConcurrent,
			InUse:         inUse,
			Available:     binding.MaxConcurrent - inUse,
			Enabled:       binding.Enabled,
		})
	}

	return statuses, nil
}

// CleanupOrphanedSlots removes slots for sessions that no longer exist.
func (s *Store) CleanupOrphanedSlots() (int, error) {
	res, err := s.db.Exec(`
		DELETE FROM credential_slots
		WHERE session_id NOT IN (SELECT id FROM sessions)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
