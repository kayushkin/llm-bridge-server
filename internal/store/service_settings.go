package store

import "fmt"

// The server's own behaviour settings: what internal/config declares Editable
// and llm-bridge's servicesettings package reads and writes. One row per key,
// the value written as the setting's type says. The environment seeds a row
// once, on the first start after a setting becomes editable; after that this
// table is the only source for it.

func (s *Store) migrateServiceSettings() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS service_settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	return err
}

// ServiceSettingValues is the servicesettings.StoredValues over this store.
type ServiceSettingValues struct{ store *Store }

// ServiceSettingValues returns the stored behaviour settings of this server.
func (s *Store) ServiceSettingValues() ServiceSettingValues {
	return ServiceSettingValues{store: s}
}

// Load returns every stored key with its value.
func (v ServiceSettingValues) Load() (map[string]string, error) {
	rows, err := v.store.dbRO.Query(`SELECT key, value FROM service_settings`)
	if err != nil {
		return nil, fmt.Errorf("read service_settings: %w", err)
	}
	defer rows.Close()
	held := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("read service_settings: %w", err)
		}
		held[key] = value
	}
	return held, rows.Err()
}

// Save writes one key, replacing what was there.
func (v ServiceSettingValues) Save(key, value string) error {
	_, err := v.store.db.Exec(`
		INSERT INTO service_settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value)
	if err != nil {
		return fmt.Errorf("write service_settings %s: %w", key, err)
	}
	return nil
}

// MoveServiceSettingKeys carries stored rows over a change of keys, in one
// transaction: each renamed key's value is copied to its new key when the new
// key holds no row yet, then every retired key's row is deleted. Run it
// before the rows are loaded, so the settings registry never sees an old key
// and a renamed setting keeps the value it was given rather than its default.
func (s *Store) MoveServiceSettingKeys(renamed map[string]string, retired []string) error {
	transaction, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("move service_settings keys: %w", err)
	}
	defer transaction.Rollback()
	for oldKey, newKey := range renamed {
		if _, err := transaction.Exec(`
			INSERT INTO service_settings (key, value, updated_at)
			SELECT ?, value, CURRENT_TIMESTAMP FROM service_settings WHERE key = ?
			ON CONFLICT(key) DO NOTHING`, newKey, oldKey); err != nil {
			return fmt.Errorf("move service_settings %s to %s: %w", oldKey, newKey, err)
		}
	}
	for _, key := range retired {
		if _, err := transaction.Exec(`DELETE FROM service_settings WHERE key = ?`, key); err != nil {
			return fmt.Errorf("delete retired service_settings %s: %w", key, err)
		}
	}
	return transaction.Commit()
}
