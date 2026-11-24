package modelstore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AddProvider registers a provider.
func (s *Store) AddProvider(p Provider) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO providers (id, name) VALUES (?, ?)`,
		p.ID, p.Name,
	)
	return err
}

// Providers lists all registered providers.
func (s *Store) Providers() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT id, name FROM providers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			continue
		}
		providers = append(providers, p)
	}
	return providers, nil
}

// AddModel registers a model. It writes with INSERT OR REPLACE, which deletes
// the old row and inserts a fresh one rather than patching in place, so every
// column the caller cares about has to appear in the list below — anything
// omitted silently reverts to its DEFAULT. That is why sync copies the user-set
// fields off the existing row before calling here.
func (s *Store) AddModel(m Model) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO models (id, provider, name, short_name, max_tokens, input_cost, output_cost, enabled, priority) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Provider, m.Name, m.ShortName, m.MaxTokens, m.InputCost, m.OutputCost, enabled, m.Priority,
	)
	if err != nil {
		return err
	}
	// Add aliases
	for _, alias := range m.Aliases {
		s.db.Exec(`INSERT OR REPLACE INTO model_aliases (alias, model_id) VALUES (?, ?)`, alias, m.ID)
	}
	return nil
}

// Models lists models for a provider.
func (s *Store) Models(provider string) ([]Model, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, name, short_name, max_tokens, input_cost, output_cost, enabled, priority FROM models WHERE provider = ? ORDER BY priority`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []Model
	for rows.Next() {
		var m Model
		var enabled int
		if err := rows.Scan(&m.ID, &m.Provider, &m.Name, &m.ShortName, &m.MaxTokens, &m.InputCost, &m.OutputCost, &enabled, &m.Priority); err != nil {
			continue
		}
		m.Enabled = enabled != 0
		// Load aliases
		aliasRows, _ := s.db.Query(`SELECT alias FROM model_aliases WHERE model_id = ?`, m.ID)
		if aliasRows != nil {
			for aliasRows.Next() {
				var a string
				aliasRows.Scan(&a)
				m.Aliases = append(m.Aliases, a)
			}
			aliasRows.Close()
		}
		models = append(models, m)
	}
	return models, nil
}

// ResolveModel resolves a model ID, alias, or canonical role to a full Model.
//
// Lookup order is id, then alias, then role. Roles are checked LAST on purpose:
// the role set is fixed (CanonicalRoles), so a collision with a
// real id or alias is unlikely, but if one ever exists the concrete id/alias
// wins and the role is never consulted — a caller that names a real row gets
// that row, not a pointer to something else.
func (s *Store) ResolveModel(idOrAliasOrRole string) (*Model, error) {
	// 1. Direct ID.
	if m, err := s.modelByID(idOrAliasOrRole); err == nil {
		return m, nil
	}

	// 2. Alias.
	var modelID string
	err := s.db.QueryRow(`SELECT model_id FROM model_aliases WHERE alias = ?`, idOrAliasOrRole).Scan(&modelID)
	if err == nil {
		m, err := s.modelByID(modelID)
		if err != nil {
			return nil, fmt.Errorf("alias %q points at unknown model %q", idOrAliasOrRole, modelID)
		}
		return m, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 3. Canonical role (see the doc comment for why this comes after id and alias).
	if IsCanonicalRole(idOrAliasOrRole) {
		// Read model_roles directly rather than via ResolveRole: ResolveRole calls
		// back into ResolveModel with the assigned id, so going through it here
		// would be a needless round trip on the happy path.
		err := s.db.QueryRow(`SELECT model_id FROM model_roles WHERE role = ?`, idOrAliasOrRole).Scan(&modelID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("role %q is not assigned", idOrAliasOrRole)
		}
		if err != nil {
			return nil, err
		}
		m, err := s.modelByID(modelID)
		if err != nil {
			return nil, fmt.Errorf("role %q points at unknown model %q", idOrAliasOrRole, modelID)
		}
		return m, nil
	}

	return nil, fmt.Errorf("model not found: %s", idOrAliasOrRole)
}

// modelByID loads one model row by its exact id. sql.ErrNoRows is returned
// unwrapped so callers can distinguish "absent" from a real database error.
func (s *Store) modelByID(id string) (*Model, error) {
	var m Model
	var enabled int
	err := s.db.QueryRow(
		`SELECT id, provider, name, short_name, max_tokens, input_cost, output_cost, enabled, priority FROM models WHERE id = ?`,
		id,
	).Scan(&m.ID, &m.Provider, &m.Name, &m.ShortName, &m.MaxTokens, &m.InputCost, &m.OutputCost, &enabled, &m.Priority)
	if err != nil {
		return nil, err
	}
	m.Enabled = enabled != 0
	return &m, nil
}

// AllModelsWithStatus returns all models joined with their health data.
func (s *Store) AllModelsWithStatus() ([]ModelStatus, error) {
	s.migrateHealth()

	rows, err := s.db.Query(`
		SELECT m.id, m.provider, m.name, m.short_name, m.max_tokens, m.input_cost, m.output_cost, m.enabled, m.priority,
		       COALESCE(h.last_success_at, 0), COALESCE(h.last_success_ms, 0), COALESCE(h.avg_response_ms, 0),
		       COALESCE(h.last_error_at, 0), COALESCE(h.last_error, ''), COALESCE(h.consecutive_errors, 0)
		FROM models m
		LEFT JOIN model_health h ON m.id = h.model
		ORDER BY m.priority, m.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelStatus
	for rows.Next() {
		var ms ModelStatus
		var enabled int
		var successAt, errorAt int64
		var health ModelHealth

		if err := rows.Scan(
			&ms.ID, &ms.Provider, &ms.Name, &ms.ShortName, &ms.MaxTokens, &ms.InputCost, &ms.OutputCost, &enabled, &ms.Priority,
			&successAt, &health.LastSuccessMs, &health.AvgResponseMs,
			&errorAt, &health.LastError, &health.ConsecutiveErr,
		); err != nil {
			continue
		}

		ms.Enabled = enabled != 0
		health.Model = ms.ID
		if successAt > 0 {
			health.LastSuccessAt = time.Unix(successAt, 0)
		}
		if errorAt > 0 {
			health.LastErrorAt = time.Unix(errorAt, 0)
		}
		ms.Health = &health
		result = append(result, ms)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load aliases per model. Without this the HTTP surface reported
	// aliases:null for every row — the `best` pointer and every nickname were
	// invisible to any consumer reading /api/models, which is the entire reason
	// callers reinvented their own default resolution. Done after the rows loop
	// because sqlite serializes one connection's queries: iterating the outer
	// rows while opening an inner query on the same *sql.DB deadlocks.
	for i := range result {
		aliasRows, err := s.db.Query(`SELECT alias FROM model_aliases WHERE model_id = ?`, result[i].ID)
		if err != nil {
			return nil, err
		}
		for aliasRows.Next() {
			var a string
			if err := aliasRows.Scan(&a); err != nil {
				aliasRows.Close()
				return nil, err
			}
			result[i].Aliases = append(result[i].Aliases, a)
		}
		aliasRows.Close()
	}
	return result, nil
}

// FailoverChain returns enabled models ordered by priority (for failover).
func (s *Store) FailoverChain() ([]Model, error) {
	rows, err := s.db.Query(`
		SELECT id, provider, name, short_name, max_tokens, input_cost, output_cost, enabled, priority
		FROM models WHERE enabled = 1 ORDER BY priority
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []Model
	for rows.Next() {
		var m Model
		var enabled int
		if err := rows.Scan(&m.ID, &m.Provider, &m.Name, &m.ShortName, &m.MaxTokens, &m.InputCost, &m.OutputCost, &enabled, &m.Priority); err != nil {
			continue
		}
		m.Enabled = true
		models = append(models, m)
	}
	return models, nil
}

// SetEnabled enables or disables a model.
func (s *Store) SetEnabled(modelID string, enabled bool) error {
	val := 0
	if enabled {
		val = 1
	}
	res, err := s.db.Exec("UPDATE models SET enabled = ? WHERE id = ?", val, modelID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %q not found", modelID)
	}
	return nil
}

// SetCost updates a model's per-million-token input and output cost.
// Like SetEnabled, it keys on the exact model ID, not an alias.
func (s *Store) SetCost(modelID string, inputCost, outputCost float64) error {
	res, err := s.db.Exec(
		"UPDATE models SET input_cost = ?, output_cost = ? WHERE id = ?",
		inputCost, outputCost, modelID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %q not found", modelID)
	}
	return nil
}

// SetPriority updates a model's failover priority (lower = preferred).
// Like SetEnabled, it keys on the exact model ID, not an alias.
func (s *Store) SetPriority(modelID string, priority int) error {
	res, err := s.db.Exec("UPDATE models SET priority = ? WHERE id = ?", priority, modelID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %q not found", modelID)
	}
	return nil
}

// SetShortName updates a model's short display nickname, the terse label a UI
// falls back to when the full name will not fit. It does not create an alias:
// nothing resolves a model by its short name, so two models sharing one is a
// cosmetic annoyance rather than an ambiguous lookup.
// Like SetEnabled, it keys on the exact model ID, not an alias.
func (s *Store) SetShortName(modelID, short string) error {
	res, err := s.db.Exec("UPDATE models SET short_name = ? WHERE id = ?", short, modelID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %q not found", modelID)
	}
	return nil
}

// AddAlias points an alias at a model. The model must already exist, and the
// alias must not collide with an existing model ID (ResolveModel tries IDs
// first, so such an alias would be permanently unreachable) or an alias that
// already points elsewhere. Remove the old alias first to repoint it.
func (s *Store) AddAlias(modelID, alias string) error {
	var exists string
	if err := s.db.QueryRow("SELECT id FROM models WHERE id = ?", modelID).Scan(&exists); err != nil {
		return fmt.Errorf("model %q not found", modelID)
	}
	if err := s.db.QueryRow("SELECT id FROM models WHERE id = ?", alias).Scan(&exists); err == nil {
		return fmt.Errorf("alias %q collides with an existing model ID and would be unreachable", alias)
	}
	var current string
	if err := s.db.QueryRow("SELECT model_id FROM model_aliases WHERE alias = ?", alias).Scan(&current); err == nil {
		if current == modelID {
			return nil // already points where asked; nothing to do
		}
		return fmt.Errorf("alias %q already points to %q; remove it first to repoint", alias, current)
	}
	_, err := s.db.Exec("INSERT INTO model_aliases (alias, model_id) VALUES (?, ?)", alias, modelID)
	return err
}

// RemoveAlias deletes an alias. It is an error, not a silent no-op, to remove
// one that does not exist.
func (s *Store) RemoveAlias(alias string) error {
	res, err := s.db.Exec("DELETE FROM model_aliases WHERE alias = ?", alias)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %q not found", alias)
	}
	return nil
}

// DeleteModel removes a model and every alias that pointed at it. It keys on the
// exact model ID and is an error if no such model exists.
func (s *Store) DeleteModel(modelID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM model_aliases WHERE model_id = ?", modelID); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM models WHERE id = ?", modelID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %q not found", modelID)
	}
	return tx.Commit()
}
