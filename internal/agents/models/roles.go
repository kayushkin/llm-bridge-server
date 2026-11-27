package models

import (
	"database/sql"
	"errors"
	"fmt"
)

// Roles are global, purpose-named pointers into the model registry. They exist
// so a caller can ask for "the efficient model" rather than naming a concrete
// id it will then have to keep up to date. A role is NOT tied to a provider or
// a harness: it names a capability tier that any harness can be pointed at,
// which is the whole reason the resolution lives here in the one source of
// truth rather than being reinvented per caller.
//
//	best      — highest performance, cost no object
//	default   — the everyday model: much cheaper than best, better than balanced
//	balanced  — between default and efficient: judgment that runs at volume,
//	            where efficient answers too carelessly (merging duplicate cards)
//	efficient — cheap enough to run at volume (titles, classification, polling)
//
// A role may also name ONE job whose model an eval-store run chose, when no
// tier fits it and moving a tier would move every other caller of that tier:
//
//	turn-end-signal — llm-bridge-server's turn-end classifier, which turns a
//	            finished turn's last message into question cards. Chosen by
//	            eval-store eval_run_000010 (claude-haiku-5-5 accepted on 12 of
//	            13 cases, gpt-5.6-terra on 10), while terra stays first in
//	            balanced for the classification jobs it did better at.
//
// A role may also name a job whose model the operator chose by name:
//
//	possession-photo-reader, possession-photo-second-reader — the two models
//	            possession-store asks, each on its own and then each shown
//	            the other's answer, what possessions a photo shows. The
//	            operator chose claude-opus-5-5 and gpt-6-astra on 2026-11-06
//	            so that two different makers' models must agree, and moved
//	            the second reader to gpt-6.2-astra on 2026-11-17. Each holds
//	            one model and no fallback: a reader that cannot answer stops
//	            the review rather than let a third model stand in.
//
//	time-entry-reviewer — the model time-ledger-store asks, for each time
//	            entry with its sources and evidence, whether the operator did
//	            it. The operator chose claude-opus-5-5 by name on 2026-11-07.
//	            It holds one model and no fallback: a review another model
//	            wrote would be signed with a name the operator did not pick.
//
// A role holds an ordered list of models: the first is the one callers use,
// and the rest are the fallbacks a caller tries, in order, when a call to the
// one before fails. model_roles keeps the first model, so a reader that knows
// only ResolveRole keeps working; model_role_fallbacks keeps the rest.
//
// The set is fixed and defined ONCE here. Nothing else may carry its own copy;
// a role outside this set is rejected at write time so a typo cannot create a
// silent fourth role that nothing resolves.
const (
	RoleBest      = "best"
	RoleDefault   = "default"
	RoleBalanced  = "balanced"
	RoleEfficient = "efficient"

	RoleTurnEndSignal = "turn-end-signal"

	RolePossessionPhotoReader       = "possession-photo-reader"
	RolePossessionPhotoSecondReader = "possession-photo-second-reader"

	RoleTimeEntryReviewer = "time-entry-reviewer"
)

// CanonicalRoles is the single definition of the valid role set. Iterate this
// rather than hardcoding the names anywhere else. The tiers come first, from
// most to least capable, then the roles that name one job.
var CanonicalRoles = []string{RoleBest, RoleDefault, RoleBalanced, RoleEfficient, RoleTurnEndSignal, RolePossessionPhotoReader, RolePossessionPhotoSecondReader, RoleTimeEntryReviewer}

// ErrUnknownRole is returned when a role name is not in CanonicalRoles.
var ErrUnknownRole = errors.New("unknown role")

// IsCanonicalRole reports whether name is one of the fixed roles.
func IsCanonicalRole(name string) bool {
	for _, r := range CanonicalRoles {
		if r == name {
			return true
		}
	}
	return false
}

// Roles returns the current role→model-id mapping. A role with no assignment is
// simply absent from the map rather than present with an empty value, so the
// caller can tell "unset" from "set to something".
func (s *Store) Roles() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT role, model_id FROM model_roles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var role, modelID string
		if err := rows.Scan(&role, &modelID); err != nil {
			return nil, err
		}
		out[role] = modelID
	}
	return out, rows.Err()
}

// SetRole makes modelID the first model of a role and keeps the role's
// fallbacks. The role must be canonical and the model must already exist in the
// registry — a role can only ever name a real model, never a free-text string,
// so it cannot drift out of sync with the models table the way a hardcoded
// default in a caller does. A fallback equal to the new first model is dropped,
// since a list that names one model twice would retry it.
func (s *Store) SetRole(role, modelID string) error {
	models, err := s.RoleModelIDs(role)
	if err != nil && !errors.Is(err, errRoleNotAssigned) {
		return err
	}
	list := []string{modelID}
	for i, id := range models {
		if i == 0 || id == modelID {
			continue
		}
		list = append(list, id)
	}
	return s.SetRoleModels(role, list)
}

// SetRoleModels replaces a role's whole list: modelIDs[0] is the model callers
// use and the rest are its fallbacks, in the order to try them. The list must
// name at least one model, every model must exist, and none may appear twice.
func (s *Store) SetRoleModels(role string, modelIDs []string) error {
	if !IsCanonicalRole(role) {
		return fmt.Errorf("%w: %q (valid roles: %v)", ErrUnknownRole, role, CanonicalRoles)
	}
	if len(modelIDs) == 0 {
		return fmt.Errorf("role %q needs at least one model; use RemoveRole to clear it", role)
	}
	seen := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		if seen[id] {
			return fmt.Errorf("role %q names model %q twice", role, id)
		}
		seen[id] = true
		var exists string
		err := s.db.QueryRow(`SELECT id FROM models WHERE id = $1`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("cannot point role %q at unknown model %q", role, id)
		}
		if err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO model_roles (role, model_id) VALUES ($1, $2)
		 ON CONFLICT(role) DO UPDATE SET model_id = excluded.model_id`,
		role, modelIDs[0],
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_role_fallbacks WHERE role = $1`, role); err != nil {
		return err
	}
	for position, id := range modelIDs[1:] {
		if _, err := tx.Exec(
			`INSERT INTO model_role_fallbacks (role, position, model_id) VALUES ($1, $2, $3)`,
			role, position+1, id,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var errRoleNotAssigned = errors.New("role is not assigned")

// RoleModelIDs returns a role's models in the order to try them: the first
// model, then each fallback. It fails on an unknown role and on one with no
// assignment, as ResolveRole does.
func (s *Store) RoleModelIDs(role string) ([]string, error) {
	if !IsCanonicalRole(role) {
		return nil, fmt.Errorf("%w: %q (valid roles: %v)", ErrUnknownRole, role, CanonicalRoles)
	}
	var first string
	err := s.db.QueryRow(`SELECT model_id FROM model_roles WHERE role = $1`, role).Scan(&first)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", errRoleNotAssigned, role)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT model_id FROM model_role_fallbacks WHERE role = $1 ORDER BY position`, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{first}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RoleModels resolves every model in a role's list, in the order to try them.
func (s *Store) RoleModels(role string) ([]*Model, error) {
	ids, err := s.RoleModelIDs(role)
	if err != nil {
		return nil, err
	}
	models := make([]*Model, 0, len(ids))
	for _, id := range ids {
		m, err := s.ResolveModel(id)
		if err != nil {
			return nil, fmt.Errorf("role %q: %w", role, err)
		}
		models = append(models, m)
	}
	return models, nil
}

// RoleModelLists returns every assigned role's list of model ids, first model
// first. An unassigned role is absent, as in Roles.
func (s *Store) RoleModelLists() (map[string][]string, error) {
	roles, err := s.Roles()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(roles))
	for role := range roles {
		if !IsCanonicalRole(role) {
			continue
		}
		ids, err := s.RoleModelIDs(role)
		if err != nil {
			return nil, err
		}
		out[role] = ids
	}
	return out, nil
}

// RemoveRole clears a role assignment. Removing a role that is not set is not an
// error — the desired end state (no assignment) already holds.
func (s *Store) RemoveRole(role string) error {
	if _, err := s.db.Exec(`DELETE FROM model_role_fallbacks WHERE role = $1`, role); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM model_roles WHERE role = $1`, role)
	return err
}

// ResolveRole resolves a role to its full model. It fails loud on an unknown
// role name and on a role that has no assignment — a caller asking for "the
// efficient model" when none is set must hear about it, not be handed a
// fabricated fallback that masks the missing configuration.
func (s *Store) ResolveRole(role string) (*Model, error) {
	if !IsCanonicalRole(role) {
		return nil, fmt.Errorf("%w: %q (valid roles: %v)", ErrUnknownRole, role, CanonicalRoles)
	}
	var modelID string
	err := s.db.QueryRow(`SELECT model_id FROM model_roles WHERE role = $1`, role).Scan(&modelID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", errRoleNotAssigned, role)
	}
	if err != nil {
		return nil, err
	}
	return s.ResolveModel(modelID)
}
