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
