package peoplestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

const identityColumns = `seq, owner_principal_id, person_seq, kind, value, label, linked_by, reason, linked_at, created_at, updated_at`

func scanIdentity(scan func(...any) error) (Identity, error) {
	var identity Identity
	var seq, personSeq int64
	err := scan(&seq, &identity.OwnerPrincipalID, &personSeq, &identity.Kind, &identity.Value, &identity.Label,
		&identity.LinkedBy, &identity.Reason, &identity.LinkedAt, &identity.CreatedAt, &identity.UpdatedAt)
	identity.ID, identity.PersonID = formatID(identityIDPrefix, seq), formatID(personIDPrefix, personSeq)
	return identity, err
}

func listIdentities(ctx context.Context, q queryer, personSeq int64) ([]Identity, error) {
	return queryIdentities(ctx, q, `SELECT `+identityColumns+` FROM identities WHERE person_seq = $1 ORDER BY seq`, personSeq)
}

func queryIdentities(ctx context.Context, q queryer, query string, args ...any) ([]Identity, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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

// ListIdentities lists the identities in scope in the order they were
// attached, so a caller can page through every handle of one kind.
func (s *Store) ListIdentities(ctx context.Context, scope ownership.Scope, filter IdentityFilter) ([]Identity, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
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
	var args arguments
	query := `SELECT ` + identityColumns + ` FROM identities WHERE ` + args.within(scope, "owner_principal_id")
	if filter.Kind != "" {
		if !contains(identityKindNames(), filter.Kind) {
			return nil, fmt.Errorf("%w: unknown identity kind %q: use one of %s", ErrInvalid, filter.Kind, strings.Join(identityKindNames(), ", "))
		}
		query += ` AND kind = ` + args.add(filter.Kind)
	}
	query += fmt.Sprintf(` ORDER BY seq LIMIT %d OFFSET %d`, limit, filter.Offset)
	return queryIdentities(ctx, s.db, query, args.values...)
}

// identityHolder returns owner's identity with this normalized kind and value,
// or nil when none of owner's persons holds it.
func identityHolder(ctx context.Context, q queryer, owner, kind, value string) (*Identity, error) {
	identity, err := scanIdentity(q.QueryRowContext(ctx, `SELECT `+identityColumns+` FROM identities WHERE owner_principal_id = $1 AND kind = $2 AND value = $3`, owner, kind, value).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

// conflictingHolder is the 409 for a handle that already belongs to someone.
func conflictingHolder(ctx context.Context, q queryer, holder *Identity, holderSeq int64) error {
	person, err := getPerson(ctx, q, ownership.OwnedBy(holder.OwnerPrincipalID), holderSeq)
	if err != nil {
		return fmt.Errorf("%s %s belongs to %s, which cannot be read: %w", holder.Kind, holder.Value, holder.PersonID, err)
	}
	return fmt.Errorf("%w: %s %s already belongs to %s (%s) as %s", ErrConflict, holder.Kind, holder.Value, holder.PersonID, person.DisplayName, holder.ID)
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

const refusalColumns = `seq, owner_principal_id, kind, value, person_seq, refused_by, reason, created_at`

func scanRefusal(scan func(...any) error) (Refusal, error) {
	var refusal Refusal
	var seq, personSeq int64
	err := scan(&seq, &refusal.OwnerPrincipalID, &refusal.Kind, &refusal.Value, &personSeq, &refusal.RefusedBy, &refusal.Reason, &refusal.CreatedAt)
	refusal.ID, refusal.PersonID = formatID(refusalIDPrefix, seq), formatID(personIDPrefix, personSeq)
	return refusal, err
}

// refusalOf returns the refusal of this handle for this person, or nil. A
// person has one owner, so the refusal is that owner's.
func refusalOf(ctx context.Context, q queryer, kind, value string, personSeq int64) (*Refusal, error) {
	refusal, err := scanRefusal(q.QueryRowContext(ctx, `SELECT `+refusalColumns+` FROM identity_refusals WHERE kind = $1 AND value = $2 AND person_seq = $3`, kind, value, personSeq).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &refusal, nil
}

// insertRefusal records that a handle is not owner's person. A pair already
// refused keeps its first row.
func insertRefusal(ctx context.Context, tx *sql.Tx, owner, kind, value string, personSeq int64, refusedBy, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO identity_refusals (owner_principal_id, kind, value, person_seq, refused_by, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (owner_principal_id, kind, value, person_seq) DO NOTHING`,
		owner, kind, value, personSeq, refusedBy, reason, now())
	return err
}

// RefusalFilter selects refusals; empty fields match everything.
type RefusalFilter struct {
	PersonID string
	Kind     string
	Value    string
}

// ListRefusals lists the refusals in scope oldest first. Kind and Value are
// normalized as on write; Value needs Kind.
func (s *Store) ListRefusals(ctx context.Context, scope ownership.Scope, filter RefusalFilter) ([]Refusal, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := []string{args.within(scope, "owner_principal_id")}
	if filter.PersonID != "" {
		personSeq, err := numberOf(personIDPrefix, filter.PersonID)
		if err != nil {
			return nil, fmt.Errorf("%w: person_id %q is not a person id (person_000001)", ErrInvalid, filter.PersonID)
		}
		where = append(where, `person_seq = `+args.add(personSeq))
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
			where = append(where, `kind = `+args.add(handle.Kind)+` AND value = `+args.add(handle.Value))
		} else {
			where = append(where, `kind = `+args.add(filter.Kind))
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+refusalColumns+` FROM identity_refusals WHERE `+strings.Join(where, ` AND `)+` ORDER BY seq`, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refusals := []Refusal{}
	for rows.Next() {
		refusal, err := scanRefusal(rows.Scan)
		if err != nil {
			return nil, err
		}
		refusals = append(refusals, refusal)
	}
	return refusals, rows.Err()
}

// addIdentity attaches one normalized handle to owner's person inside tx,
// after recording the person's revision. A handle already on this same person
// is returned as it is (added=false). One on another of owner's persons is a
// 409, unless the link moves it (operator only), which records a refusal
// against the person it leaves; a handle moves only between one owner's
// persons, since another owner's records never enter the lookup. A pair the
// operator refused is a 409 for every source but the operator, whose link
// deletes the refusal. Every waiting suggestion of owner's that holds the
// handle, except exceptSuggestionSeq, loses it (closeCoveredHandle).
func addIdentity(ctx context.Context, tx *sql.Tx, owner string, personSeq int64, input IdentityLinkInput, source LinkSource, exceptSuggestionSeq int64) (identity Identity, added bool, err error) {
	holder, err := identityHolder(ctx, tx, owner, input.Kind, input.Value)
	if err != nil {
		return Identity{}, false, err
	}
	personID := formatID(personIDPrefix, personSeq)
	var holderSeq int64
	if holder != nil {
		if holderSeq, err = numberOf(personIDPrefix, holder.PersonID); err != nil {
			return Identity{}, false, err
		}
	}
	if holder != nil && holderSeq == personSeq {
		return *holder, false, nil
	}
	if holder != nil && !input.Move {
		return Identity{}, false, conflictingHolder(ctx, tx, holder, holderSeq)
	}
	refusal, err := refusalOf(ctx, tx, input.Kind, input.Value, personSeq)
	if err != nil {
		return Identity{}, false, err
	}
	if refusal != nil {
		if !source.OverridesRefusals {
			return Identity{}, false, fmt.Errorf("%w: %s %s was refused for %s by %s (%s, %s); only the operator can link them", ErrConflict, input.Kind, input.Value, personID, refusal.RefusedBy, refusal.ID, refusal.Reason)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM identity_refusals WHERE seq = $1`, mustNumber(refusalIDPrefix, refusal.ID)); err != nil {
			return Identity{}, false, err
		}
	}
	if err := recordRevision(ctx, tx, personSeq, RevisionActionIdentityAdd); err != nil {
		return Identity{}, false, err
	}
	timestamp := now()
	var seq int64
	if holder != nil {
		// A move: the row keeps its id and changes person.
		if err := recordRevision(ctx, tx, holderSeq, RevisionActionIdentityRemove); err != nil {
			return Identity{}, false, err
		}
		if err := insertRefusal(ctx, tx, owner, input.Kind, input.Value, holderSeq, source.Source, "moved to "+personID+": "+input.Reason); err != nil {
			return Identity{}, false, err
		}
		seq = mustNumber(identityIDPrefix, holder.ID)
		if _, err := tx.ExecContext(ctx, `UPDATE identities SET person_seq = $1, label = $2, linked_by = $3, reason = $4, linked_at = $5, updated_at = $6 WHERE seq = $7`,
			personSeq, input.Label, input.LinkedBy, input.Reason, input.LinkedAt, timestamp, seq); err != nil {
			return Identity{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET updated_at = $1 WHERE seq = $2`, timestamp, holderSeq); err != nil {
			return Identity{}, false, err
		}
		if err := reindexPerson(ctx, tx, holderSeq); err != nil {
			return Identity{}, false, err
		}
	} else {
		err := tx.QueryRowContext(ctx, `INSERT INTO identities (owner_principal_id, person_seq, kind, value, label, linked_by, reason, linked_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING seq`,
			owner, personSeq, input.Kind, input.Value, input.Label, input.LinkedBy, input.Reason, input.LinkedAt, timestamp, timestamp).Scan(&seq)
		if err != nil {
			return Identity{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET updated_at = $1 WHERE seq = $2`, timestamp, personSeq); err != nil {
		return Identity{}, false, err
	}
	if err := reindexPerson(ctx, tx, personSeq); err != nil {
		return Identity{}, false, err
	}
	if err := closeCoveredHandle(ctx, tx, owner, input.Kind, input.Value, personID, exceptSuggestionSeq); err != nil {
		return Identity{}, false, err
	}
	identity, err = scanIdentity(tx.QueryRowContext(ctx, `SELECT `+identityColumns+` FROM identities WHERE seq = $1`, seq).Scan)
	return identity, true, err
}

// mustNumber reads the number of an id this store formatted itself.
func mustNumber(prefix, id string) int64 {
	seq, err := numberOf(prefix, id)
	if err != nil {
		panic(fmt.Sprintf("peoplestore formatted %q, which does not parse as a %s id", id, prefix))
	}
	return seq
}

// closeCoveredHandle takes a handle that was just attached to personID out of
// every waiting suggestion of owner's that holds it, but exceptSuggestionSeq:
// the handle moves from payload.identities to payload.covered, and personID
// joins payload.matched_person_ids. A suggestion left with no handle is
// superseded, with person_id set when every covered handle went to one person.
// Its rows in suggestion_handles stay, so no importer proposes the handle
// again.
func closeCoveredHandle(ctx context.Context, tx *sql.Tx, owner, kind, value, personID string, exceptSuggestionSeq int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT s.seq, s.payload FROM suggestions s JOIN suggestion_handles h ON h.suggestion_seq = s.seq
		WHERE s.owner_principal_id = $1 AND h.kind = $2 AND h.value = $3 AND s.status = $4 AND s.seq <> $5 ORDER BY s.seq`,
		owner, kind, value, SuggestionStatusProposed, exceptSuggestionSeq)
	if err != nil {
		return err
	}
	type waiting struct {
		seq     int64
		payload string
	}
	var suggestions []waiting
	for rows.Next() {
		var item waiting
		if err := rows.Scan(&item.seq, &item.payload); err != nil {
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
			return fmt.Errorf("suggestion %s: payload: %w", formatID(suggestionIDPrefix, item.seq), err)
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
			if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET payload = $1 WHERE seq = $2`, string(encoded), item.seq); err != nil {
				return err
			}
			continue
		}
		var onePerson sql.NullInt64
		if first, err := numberOf(personIDPrefix, payload.Covered[0].PersonID); err == nil {
			onePerson = sql.NullInt64{Int64: first, Valid: true}
		}
		for _, covered := range payload.Covered {
			if covered.PersonID != payload.Covered[0].PersonID {
				onePerson = sql.NullInt64{}
				break
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET payload = $1, status = $2, person_seq = $3, decided_at = $4 WHERE seq = $5`,
			string(encoded), SuggestionStatusSuperseded, onePerson, timestamp, item.seq); err != nil {
			return err
		}
	}
	return nil
}

// AddIdentity attaches a handle to a live person in scope; the identity takes
// the person's owner. It answers created=false when the person already had
// exactly this handle.
func (s *Store) AddIdentity(ctx context.Context, scope ownership.Scope, personID string, input IdentityLinkInput) (Identity, bool, error) {
	normalized, source, err := normalizeLinkInput(input)
	if err != nil {
		return Identity{}, false, err
	}
	tx, seq, owner, err := s.beginForPerson(ctx, scope, personID)
	if err != nil {
		return Identity{}, false, err
	}
	defer tx.Rollback()
	if _, err := getLivePerson(ctx, tx, scope, seq); err != nil {
		return Identity{}, false, err
	}
	identity, added, err := addIdentity(ctx, tx, owner, seq, normalized, source, 0)
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
func (s *Store) RemoveIdentity(ctx context.Context, scope ownership.Scope, id string) error {
	if err := checkScope(scope); err != nil {
		return err
	}
	seq, err := numberOf(identityIDPrefix, id)
	if err != nil {
		return err
	}
	owner, err := ownerOfRow(ctx, s.db, scope, "identities", seq, identityIDPrefix)
	if err != nil {
		return err
	}
	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	identity, err := scanIdentity(tx.QueryRowContext(ctx, `SELECT `+identityColumns+` FROM identities WHERE seq = $1`, seq).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: identity %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	personSeq := mustNumber(personIDPrefix, identity.PersonID)
	if err := recordRevision(ctx, tx, personSeq, RevisionActionIdentityRemove); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE seq = $1`, seq); err != nil {
		return err
	}
	if err := insertRefusal(ctx, tx, owner, identity.Kind, identity.Value, personSeq, LinkSourceOperator, identityRemovalReason); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET updated_at = $1 WHERE seq = $2`, now(), personSeq); err != nil {
		return err
	}
	if err := reindexPerson(ctx, tx, personSeq); err != nil {
		return err
	}
	return tx.Commit()
}

// resolutions finds every record in scope holding a normalized handle, with
// its person, oldest person first.
func (s *Store) resolutions(ctx context.Context, scope ownership.Scope, kind, value string) ([]IdentityResolution, error) {
	var args arguments
	query := `SELECT ` + qualified("i", identityColumns) + `, ` + qualified("p", personColumns) + `
		FROM identities i JOIN persons p ON p.owner_principal_id = i.owner_principal_id AND p.seq = i.person_seq
		WHERE i.kind = ` + args.add(kind) + ` AND i.value = ` + args.add(value) + ` AND ` + args.within(scope, "i.owner_principal_id") + `
		ORDER BY p.created_at, p.seq`
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := []IdentityResolution{}
	for rows.Next() {
		var resolution IdentityResolution
		var identitySeq, identityPersonSeq, personSeq int64
		var tags string
		person := &resolution.Person
		identity := &resolution.Identity
		if err := rows.Scan(&identitySeq, &identity.OwnerPrincipalID, &identityPersonSeq, &identity.Kind, &identity.Value, &identity.Label,
			&identity.LinkedBy, &identity.Reason, &identity.LinkedAt, &identity.CreatedAt, &identity.UpdatedAt,
			&personSeq, &person.OwnerPrincipalID, &person.DisplayName, &person.Birthday, &person.HowMet, &tags, &person.PrincipalID,
			&person.CreatedAt, &person.UpdatedAt, &person.DeletedAt); err != nil {
			return nil, err
		}
		identity.ID, identity.PersonID = formatID(identityIDPrefix, identitySeq), formatID(personIDPrefix, identityPersonSeq)
		person.ID = formatID(personIDPrefix, personSeq)
		if err := json.Unmarshal([]byte(tags), &person.Tags); err != nil {
			return nil, fmt.Errorf("person %s: tags: %w", person.ID, err)
		}
		found = append(found, resolution)
	}
	return found, rows.Err()
}

// qualified puts alias in front of each column of a column list.
func qualified(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for index, part := range parts {
		parts[index] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// ResolveIdentity finds who holds a handle within scope, after normalizing it
// the way it was stored. A deleted holder is returned with deleted_at set.
// No record in scope holding it is ErrNotFound. A scope that reaches every
// owner, where more than one owner's records hold it, is ErrConflict: the
// caller must name owner_principal_id.
func (s *Store) ResolveIdentity(ctx context.Context, scope ownership.Scope, kind, value string) (*IdentityResolution, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	normalized, err := NormalizeIdentity(IdentityInput{Kind: kind, Value: value})
	if err != nil {
		return nil, err
	}
	found, err := s.resolutions(ctx, scope, normalized.Kind, normalized.Value)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%w: no person has %s %s", ErrNotFound, normalized.Kind, strings.TrimSpace(normalized.Value))
	case 1:
		return &found[0], nil
	}
	owners := make([]string, 0, len(found))
	for _, resolution := range found {
		owners = append(owners, resolution.Identity.OwnerPrincipalID)
	}
	return nil, fmt.Errorf("%w: %s %s is held by the records of %d owners (%s); name the one to read with %s", ErrConflict,
		normalized.Kind, normalized.Value, len(found), strings.Join(owners, ", "), ownership.OwnerParameter)
}

// ResolveIdentityInEveryOwner finds every owner's record holding a handle,
// normalized as on write, oldest person first; empty when none does. It is
// for the server's own use, such as telling who sent a message to a bot, and
// reaches every owner.
func (s *Store) ResolveIdentityInEveryOwner(ctx context.Context, kind, value string) ([]IdentityResolution, error) {
	normalized, err := NormalizeIdentity(IdentityInput{Kind: kind, Value: value})
	if err != nil {
		return nil, err
	}
	return s.resolutions(ctx, ownership.EveryOwner(), normalized.Kind, normalized.Value)
}
