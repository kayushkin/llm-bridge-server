package harnessstore

import "github.com/kayushkin/llm-bridge/msg"

// HarnessType holds display metadata for a harness type.
type HarnessType struct {
	Name  msg.Harness `json:"name"`
	Label string      `json:"label"`
	Emoji string      `json:"emoji"`
	Image string      `json:"image,omitempty"`
}

// UpsertHarnessType inserts or updates harness type metadata.
func (s *Store) UpsertHarnessType(ht *HarnessType) error {
	_, err := s.db.Exec(`
		INSERT INTO harness_types (name, label, emoji, image, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET
			label = excluded.label,
			emoji = excluded.emoji,
			image = excluded.image,
			updated_at = CURRENT_TIMESTAMP`,
		string(ht.Name), ht.Label, ht.Emoji, ht.Image,
	)
	return err
}

// GetHarnessType returns metadata for a single harness type.
func (s *Store) GetHarnessType(name msg.Harness) (*HarnessType, error) {
	row := s.db.QueryRow(`SELECT name, label, emoji, image FROM harness_types WHERE name = ?`, string(name))
	var ht HarnessType
	var n string
	if err := row.Scan(&n, &ht.Label, &ht.Emoji, &ht.Image); err != nil {
		return nil, err
	}
	ht.Name = msg.Harness(n)
	return &ht, nil
}

// ListHarnessTypes returns all harness type metadata.
func (s *Store) ListHarnessTypes() ([]HarnessType, error) {
	rows, err := s.db.Query(`SELECT name, label, emoji, image FROM harness_types ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []HarnessType
	for rows.Next() {
		var ht HarnessType
		var n string
		if err := rows.Scan(&n, &ht.Label, &ht.Emoji, &ht.Image); err != nil {
			return nil, err
		}
		ht.Name = msg.Harness(n)
		types = append(types, ht)
	}
	return types, rows.Err()
}

// DeleteHarnessType removes a harness type entry.
func (s *Store) DeleteHarnessType(name msg.Harness) error {
	_, err := s.db.Exec(`DELETE FROM harness_types WHERE name = ?`, string(name))
	return err
}
