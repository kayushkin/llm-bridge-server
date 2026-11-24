package peoplestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const identityColumns = `id, person_id, kind, value, label, linked_by, reason, linked_at, created_at, updated_at`

func scanIdentity(scan func(...any) error) (Identity, error) {
	var identity Identity
	err := scan(&identity.ID, &identity.PersonID, &identity.Kind, &identity.Value, &identity.Label,
		&identity.LinkedBy, &identity.Reason, &identity.LinkedAt, &identity.CreatedAt, &identity.UpdatedAt)
	return identity, err
}

func listIdentities(q queryer, personID string) ([]Identity, error) {
	return queryIdentities(q, `SELECT `+identityColumns+` FROM identities WHERE person_id = ? ORDER BY seq`, personID)
}

func queryIdentities(q queryer, query string, args ...any) ([]Identity, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := []Identity{}
	for rows.Next() {
		identity, err := scanIdentity(rows.Scan)
		if err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

// IdentityFilter selects identities for GET /identities. An empty Kind lists
// every kind.
type IdentityFilter struct {
	Kind   string
	Limit  int
	Offset int
}

// ListIdentities lists identities in the order they were attached, so a
// caller can page through every handle of one kind.
func (s *Store) ListIdentities(filter IdentityFilter) ([]Identity, error) {
	if filter.Limit < 0 || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit and offset must not be negative", ErrInvalid)
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 1000
	}
	if limit > 1000 {
		return nil, fmt.Errorf("%w: limit %d is over 1000", ErrInvalid, limit)
	}
	query := `SELECT ` + identityColumns + ` FROM identities`
	var args []any
	if filter.Kind != "" {
		if !contains(identityKindNames(), filter.Kind) {
			return nil, fmt.Errorf("%w: unknown identity kind %q: use one of %s", ErrInvalid, filter.Kind, strings.Join(identityKindNames(), ", "))
		}
		query += ` WHERE kind = ?`
		args = append(args, filter.Kind)
	}
	query += fmt.Sprintf(` ORDER BY seq LIMIT %d OFFSET %d`, limit, filter.Offset)
	return queryIdentities(s.db, query, args...)
}

// identityOwner returns the identity with this normalized kind and value, or
// nil when no person holds it.
func identityOwner(q queryer, kind, value string) (*Identity, error) {
	identity, err := scanIdentity(q.QueryRow(`SELECT `+identityColumns+` FROM identities WHERE kind = ? AND value = ?`, kind, value).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

// conflictingOwner is the 409 for a handle that already belongs to someone.
func conflictingOwner(q queryer, owner *Identity) error {
	person, err := getPerson(q, owner.PersonID)
	if err != nil {
		return fmt.Errorf("%s %s belongs to %s, which cannot be read: %w", owner.Kind, owner.Value, owner.PersonID, err)
	}
	return fmt.Errorf("%w: %s %s already belongs to %s (%s) as %s", ErrConflict, owner.Kind, owner.Value, owner.PersonID, person.DisplayName, owner.ID)
}

// normalizeLinkInput checks a link and puts its handle in stored form. A
// LinkedAt of 0 becomes now.
func normalizeLinkInput(input IdentityLinkInput) (IdentityLinkInput, LinkSource, error) {
	handle, err := NormalizeIdentity(IdentityInput{Kind: input.Kind, Value: input.Value, Label: input.Label})
	if err != nil {
		return input, LinkSource{}, err
	}
	source, err := linkSourceFor(strings.TrimSpace(input.LinkedBy), "linked_by")
	if err != nil {
		return input, LinkSource{}, err
	}
	reason := strings.TrimSpace(input.Reason)
	if source.ReasonRequired && reason == "" {
		return input, LinkSource{}, fmt.Errorf("%w: reason is required when linked_by is %s: say why this handle is this person", ErrInvalid, source.Source)
	}
	if input.Move && !source.OverridesRefusals {
		return input, LinkSource{}, fmt.Errorf("%w: only a source that overrides refusals (the operator) may move a handle from one person to another", ErrInvalid)
	}
	timestamp := now()
	linkedAt := input.LinkedAt
	if linkedAt < 0 || linkedAt > timestamp+60 {
		return input, LinkSource{}, fmt.Errorf("%w: linked_at %d is negative or in the future", ErrInvalid, linkedAt)
	}
	if linkedAt == 0 {
		linkedAt = timestamp
	}
	return IdentityLinkInput{Kind: handle.Kind, Value: handle.Value, Label: handle.Label, LinkedBy: source.Source, Reason: reason, LinkedAt: linkedAt, Move: input.Move}, source, nil
}

// refusalOf returns the refusal of this handle for this person, or nil.
func refusalOf(q queryer, kind, value, personID string) (*Refusal, error) {
	var refusal Refusal
	err := q.QueryRow(`SELECT id, kind, value, person_id, refused_by, reason, created_at FROM identity_refusals WHERE kind = ? AND value = ? AND person_id = ?`, kind, value, personID).
		Scan(&refusal.ID, &refusal.Kind, &refusal.Value, &refusal.PersonID, &refusal.RefusedBy, &refusal.Reason, &refusal.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &refusal, nil
}

// insertRefusal records that a handle is not a person. A pair already refused
// keeps its first row.
func insertRefusal(tx *sql.Tx, kind, value, personID, refusedBy, reason string) error {
	existing, err := refusalOf(tx, kind, value, personID)
	if err != nil || existing != nil {
		return err
	}
	seq, err := nextSeq(tx, "identity_refusals")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO identity_refusals (id, seq, kind, value, person_id, refused_by, reason, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		formatID(refusalIDPrefix, seq), seq, kind, value, personID, refusedBy, reason, now())
	return err
}

// RefusalFilter selects refusals; empty fields match everything.
type RefusalFilter struct {
	PersonID string
	Kind     string
	Value    string
}

// ListRefusals lists refusals oldest first. Kind and Value are normalized as
// on write; Value needs Kind.
func (s *Store) ListRefusals(filter RefusalFilter) ([]Refusal, error) {
	var where []string
	var args []any
	if filter.PersonID != "" {
		where = append(where, `person_id = ?`)
		args = append(args, filter.PersonID)
	}
	if filter.Value != "" && filter.Kind == "" {
		return nil, fmt.Errorf("%w: value needs kind", ErrInvalid)
	}
	if filter.Kind != "" {
		if filter.Value != "" {
			handle, err := NormalizeIdentity(IdentityInput{Kind: filter.Kind, Value: filter.Value})
			if err != nil {
				return nil, err
			}
			where = append(where, `kind = ? AND value = ?`)
			args = append(args, handle.Kind, handle.Value)
		} else {
			where = append(where, `kind = ?`)
			args = append(args, filter.Kind)
		}
	}
	query := `SELECT id, kind, value, person_id, refused_by, reason, created_at FROM identity_refusals`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := s.db.Query(query+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refusals := []Refusal{}
	for rows.Next() {
		var refusal Refusal
		if err := rows.Scan(&refusal.ID, &refusal.Kind, &refusal.Value, &refusal.PersonID, &refusal.RefusedBy, &refusal.Reason, &refusal.CreatedAt); err != nil {
			return nil, err
		}
		refusals = append(refusals, refusal)
	}
	return refusals, rows.Err()
}

// addIdentity attaches one normalized handle to a person inside tx, after
// recording the person's revision. A handle already on this same person is
// returned as it is (added=false). One on another person is a 409, unless the
// link moves it (operator only), which records a refusal against the person it
// leaves. A pair the operator refused is a 409 for every source but the
// operator, whose link deletes the refusal. Every waiting suggestion that
// holds the handle, except exceptSuggestionID, loses it (closeCoveredHandle).
func addIdentity(tx *sql.Tx, personID string, input IdentityLinkInput, source LinkSource, exceptSuggestionID string) (identity Identity, added bool, err error) {
	owner, err := identityOwner(tx, input.Kind, input.Value)
	if err != nil {
		return Identity{}, false, err
	}
	if owner != nil && owner.PersonID == personID {
		return *owner, false, nil
	}
	if owner != nil && !input.Move {
		return Identity{}, false, conflictingOwner(tx, owner)
	}
	refusal, err := refusalOf(tx, input.Kind, input.Value, personID)
	if err != nil {
		return Identity{}, false, err
	}
	if refusal != nil {
		if !source.OverridesRefusals {
			return Identity{}, false, fmt.Errorf("%w: %s %s was refused for %s by %s (%s, %s); only the operator can link them", ErrConflict, input.Kind, input.Value, personID, refusal.RefusedBy, refusal.ID, refusal.Reason)
		}
		if _, err := tx.Exec(`DELETE FROM identity_refusals WHERE id = ?`, refusal.ID); err != nil {
			return Identity{}, false, err
		}
	}
	if err := recordRevision(tx, personID, RevisionActionIdentityAdd); err != nil {
		return Identity{}, false, err
	}
	timestamp := now()
	var id string
	if owner != nil {
		// A move: the row keeps its id and changes person.
		if err := recordRevision(tx, owner.PersonID, RevisionActionIdentityRemove); err != nil {
			return Identity{}, false, err
		}
		if err := insertRefusal(tx, input.Kind, input.Value, owner.PersonID, source.Source, "moved to "+personID+": "+input.Reason); err != nil {
			return Identity{}, false, err
		}
		id = owner.ID
		if _, err := tx.Exec(`UPDATE identities SET person_id = ?, label = ?, linked_by = ?, reason = ?, linked_at = ?, updated_at = ? WHERE id = ?`,
			personID, input.Label, input.LinkedBy, input.Reason, input.LinkedAt, timestamp, id); err != nil {
			return Identity{}, false, err
		}
		if _, err := tx.Exec(`UPDATE persons SET updated_at = ? WHERE id = ?`, timestamp, owner.PersonID); err != nil {
			return Identity{}, false, err
		}
		if err := reindexPerson(tx, owner.PersonID); err != nil {
			return Identity{}, false, err
		}
	} else {
		seq, err := nextSeq(tx, "identities")
		if err != nil {
			return Identity{}, false, err
		}
		id = formatID(identityIDPrefix, seq)
		if _, err := tx.Exec(`INSERT INTO identities (id, seq, person_id, kind, value, label, linked_by, reason, linked_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, seq, personID, input.Kind, input.Value, input.Label, input.LinkedBy, input.Reason, input.LinkedAt, timestamp, timestamp); err != nil {
			return Identity{}, false, err
		}
	}
	if _, err := tx.Exec(`UPDATE persons SET updated_at = ? WHERE id = ?`, timestamp, personID); err != nil {
		return Identity{}, false, err
	}
	if err := reindexPerson(tx, personID); err != nil {
		return Identity{}, false, err
	}
	if err := closeCoveredHandle(tx, input.Kind, input.Value, personID, exceptSuggestionID); err != nil {
		return Identity{}, false, err
	}
	identity, err = scanIdentity(tx.QueryRow(`SELECT `+identityColumns+` FROM identities WHERE id = ?`, id).Scan)
	return identity, true, err
}

// closeCoveredHandle takes a handle that was just attached to personID out of
// every waiting suggestion that holds it, but exceptSuggestionID: the handle
// moves from payload.identities to payload.covered, and personID joins
// payload.matched_person_ids. A suggestion left with no handle is superseded,
// with person_id set when every covered handle went to one person. Its rows
// in suggestion_handles stay, so no importer proposes the handle again.
func closeCoveredHandle(tx *sql.Tx, kind, value, personID, exceptSuggestionID string) error {
	rows, err := tx.Query(`SELECT s.id, s.payload FROM suggestions s JOIN suggestion_handles h ON h.suggestion_id = s.id
		WHERE h.kind = ? AND h.value = ? AND s.status = ? AND s.id <> ? ORDER BY s.seq`, kind, value, SuggestionStatusProposed, exceptSuggestionID)
	if err != nil {
		return err
	}
	type waiting struct{ id, payload string }
	var suggestions []waiting
	for rows.Next() {
		var item waiting
		if err := rows.Scan(&item.id, &item.payload); err != nil {
			rows.Close()
			return err
		}
		suggestions = append(suggestions, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	timestamp := now()
	for _, item := range suggestions {
		var payload SuggestionPayload
		if err := json.Unmarshal([]byte(item.payload), &payload); err != nil {
			return fmt.Errorf("suggestion %s: payload: %w", item.id, err)
		}
		kept := []IdentityInput{}
		for _, identity := range payload.Identities {
			if identity.Kind == kind && identity.Value == value {
				continue
			}
			kept = append(kept, identity)
		}
		if len(kept) == len(payload.Identities) {
			continue
		}
		payload.Identities = kept
		payload.Covered = append(payload.Covered, CoveredHandle{Kind: kind, Value: value, PersonID: personID, CoveredAt: timestamp})
		if !contains(payload.MatchedPersonIDs, personID) {
			payload.MatchedPersonIDs = append(payload.MatchedPersonIDs, personID)
			sort.Strings(payload.MatchedPersonIDs)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if len(kept) > 0 {
			if _, err := tx.Exec(`UPDATE suggestions SET payload = ? WHERE id = ?`, string(encoded), item.id); err != nil {
				return err
			}
			continue
		}
		onePerson := payload.Covered[0].PersonID
		for _, covered := range payload.Covered {
			if covered.PersonID != onePerson {
				onePerson = ""
				break
			}
		}
		if _, err := tx.Exec(`UPDATE suggestions SET payload = ?, status = ?, person_id = ?, decided_at = ? WHERE id = ?`,
			string(encoded), SuggestionStatusSuperseded, onePerson, timestamp, item.id); err != nil {
			return err
		}
	}
	return nil
}

// AddIdentity attaches a handle to a live person. It answers created=false
// when the person already had exactly this handle.
func (s *Store) AddIdentity(personID string, input IdentityLinkInput) (Identity, bool, error) {
	normalized, source, err := normalizeLinkInput(input)
	if err != nil {
		return Identity{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Identity{}, false, err
	}
	defer tx.Rollback()
	if _, err := getLivePerson(tx, personID); err != nil {
		return Identity{}, false, err
	}
	identity, added, err := addIdentity(tx, personID, normalized, source, "")
	if err != nil {
		return Identity{}, false, err
	}
	return identity, added, tx.Commit()
}

// identityRemovalReason is the refusal reason DELETE /identities/{id} writes.
const identityRemovalReason = "the operator removed this handle from this person"

// RemoveIdentity detaches a handle and records that it is not this person, so
// no automatic source attaches it to them again. Removing is the operator's
// act. The row goes; the person's revision keeps it.
func (s *Store) RemoveIdentity(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	identity, err := scanIdentity(tx.QueryRow(`SELECT `+identityColumns+` FROM identities WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: identity %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if err := recordRevision(tx, identity.PersonID, RevisionActionIdentityRemove); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM identities WHERE id = ?`, id); err != nil {
		return err
	}
	if err := insertRefusal(tx, identity.Kind, identity.Value, identity.PersonID, LinkSourceOperator, identityRemovalReason); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE persons SET updated_at = ? WHERE id = ?`, now(), identity.PersonID); err != nil {
		return err
	}
	if err := reindexPerson(tx, identity.PersonID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveIdentity finds who owns a handle, after normalizing it the way it
// was stored. A deleted owner is returned with deleted_at set.
func (s *Store) ResolveIdentity(kind, value string) (*IdentityResolution, error) {
	normalized, err := NormalizeIdentity(IdentityInput{Kind: kind, Value: value})
	if err != nil {
		return nil, err
	}
	owner, err := identityOwner(s.db, normalized.Kind, normalized.Value)
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, fmt.Errorf("%w: no person has %s %s", ErrNotFound, normalized.Kind, strings.TrimSpace(normalized.Value))
	}
	person, err := getPerson(s.db, owner.PersonID)
	if err != nil {
		return nil, err
	}
	return &IdentityResolution{Identity: *owner, Person: *person}, nil
}
