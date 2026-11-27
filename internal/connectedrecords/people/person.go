package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// PersonFilter selects persons. The zero value lists every live person in
// scope.
type PersonFilter struct {
	Query string
	Tag   string
	// PrincipalID keeps only the person pointing at this principal-store id.
	PrincipalID    string
	IncludeDeleted bool
	Limit          int
	Offset         int
}

const personColumns = `seq, owner_principal_id, display_name, birthday, how_met, tags::text, principal_id, created_at, updated_at, deleted_at`

func scanPerson(scan func(...any) error) (*Person, error) {
	var person Person
	var seq int64
	var tags string
	if err := scan(&seq, &person.OwnerPrincipalID, &person.DisplayName, &person.Birthday, &person.HowMet, &tags, &person.PrincipalID,
		&person.CreatedAt, &person.UpdatedAt, &person.DeletedAt); err != nil {
		return nil, err
	}
	person.ID = formatID(personIDPrefix, seq)
	if err := json.Unmarshal([]byte(tags), &person.Tags); err != nil {
		return nil, fmt.Errorf("person %s: tags: %w", person.ID, err)
	}
	return &person, nil
}

func getPerson(ctx context.Context, q queryer, scope ownership.Scope, seq int64) (*Person, error) {
	var args arguments
	query := `SELECT ` + personColumns + ` FROM persons WHERE seq = ` + args.add(seq) + ` AND ` + args.within(scope, "owner_principal_id")
	person, err := scanPerson(q.QueryRowContext(ctx, query, args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: person %s", ErrNotFound, formatID(personIDPrefix, seq))
	}
	return person, err
}

// getLivePerson is getPerson that refuses a deleted person with a 409, for
// writes that would hang something new on them.
func getLivePerson(ctx context.Context, q queryer, scope ownership.Scope, seq int64) (*Person, error) {
	person, err := getPerson(ctx, q, scope, seq)
	if err != nil {
		return nil, err
	}
	if person.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: person %s is deleted; POST /persons/%s/restore first", ErrConflict, person.ID, person.ID)
	}
	return person, nil
}

// otherLivePerson finds the second person of a write that joins two records:
// the person named in a body, who must belong to owner. One outside owner
// answers as a missing one does (a 400 naming the field), unless the caller
// sees every owner and named another owner's person, which is a 400 saying so.
func otherLivePerson(ctx context.Context, tx *sql.Tx, scope ownership.Scope, owner, field, id string) (*Person, error) {
	// A body naming a malformed id is the caller's 400, as a missing one is.
	seq, err := postgresstorage.ParseRecordID(personIDPrefix, strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: person %s not found", ErrInvalid, field, id)
	}
	person, err := getLivePerson(ctx, tx, ownership.OwnedBy(owner), seq)
	if errors.Is(err, ErrNotFound) {
		if scope.IsEveryOwner() {
			if elsewhere, lookErr := getPerson(ctx, tx, ownership.EveryOwner(), seq); lookErr == nil {
				return nil, fmt.Errorf("%w: %s: %s is kept by %s, and this record by %s; a write joins only one owner's records", ErrInvalid, field, id, elsewhere.OwnerPrincipalID, owner)
			}
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, field, err)
	}
	return person, err
}

// CreatePerson records a new person owned by owner and hands back its id.
func (s *Store) CreatePerson(ctx context.Context, owner string, input PersonInput) (*PersonDetail, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		return nil, fmt.Errorf("%w: display_name is required", ErrInvalid)
	}
	birthday, err := normalizeBirthday(input.Birthday)
	if err != nil {
		return nil, err
	}
	tags, err := normalizeTags(input.Tags)
	if err != nil {
		return nil, err
	}
	type link struct {
		input  IdentityLinkInput
		source LinkSource
	}
	var links []link
	seen := map[string]bool{}
	for index, raw := range input.Identities {
		if raw.Move {
			return nil, fmt.Errorf("%w: identities[%d]: a new person cannot move a handle; create them, then POST /persons/{id}/identities with move", ErrInvalid, index)
		}
		normalized, source, err := normalizeLinkInput(raw)
		if err != nil {
			return nil, fmt.Errorf("identities[%d]: %w", index, err)
		}
		key := normalized.Kind + "\x00" + normalized.Value
		if seen[key] {
			return nil, fmt.Errorf("%w: identities[%d]: %s %s is named twice", ErrInvalid, index, normalized.Kind, normalized.Value)
		}
		seen[key] = true
		links = append(links, link{normalized, source})
	}
	principalID, err := s.checkPrincipalID(ctx, input.PrincipalID)
	if err != nil {
		return nil, err
	}
	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seq, err := insertPerson(ctx, tx, owner, displayName, birthday, strings.TrimSpace(input.HowMet), tags, principalID)
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if _, _, err := addIdentity(ctx, tx, owner, seq, link.input, link.source, 0); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPerson(ctx, ownership.OwnedBy(owner), formatID(personIDPrefix, seq))
}

func insertPerson(ctx context.Context, tx *sql.Tx, owner, displayName, birthday, howMet string, tags []string, principalID string) (int64, error) {
	encodedTags, err := json.Marshal(tags)
	if err != nil {
		return 0, err
	}
	timestamp := now()
	var seq int64
	err = tx.QueryRowContext(ctx, `INSERT INTO persons (owner_principal_id, display_name, birthday, how_met, tags, principal_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8) RETURNING seq`, owner, displayName, birthday, howMet, string(encodedTags), principalID, timestamp, timestamp).Scan(&seq)
	return seq, err
}

// GetPerson returns one person in scope with identities and relationships. A
// deleted person is returned like any other, deleted_at set, so that whatever
// points at them still renders a name. A person outside scope is ErrNotFound,
// as a missing one.
func (s *Store) GetPerson(ctx context.Context, scope ownership.Scope, id string) (*PersonDetail, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	seq, err := numberOf(personIDPrefix, id)
	if err != nil {
		return nil, err
	}
	return personDetail(ctx, s.db, scope, seq)
}

func personDetail(ctx context.Context, q queryer, scope ownership.Scope, seq int64) (*PersonDetail, error) {
	person, err := getPerson(ctx, q, scope, seq)
	if err != nil {
		return nil, err
	}
	detail := &PersonDetail{Person: *person}
	if detail.Identities, err = listIdentities(ctx, q, seq); err != nil {
		return nil, err
	}
	if detail.Relationships, err = listRelationshipViews(ctx, q, seq); err != nil {
		return nil, err
	}
	return detail, nil
}

// ListPersons returns the persons in scope in display-name order, without
// identities or relationships.
func (s *Store) ListPersons(ctx context.Context, scope ownership.Scope, filter PersonFilter) ([]Person, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	query := `SELECT ` + personColumns + ` FROM persons WHERE ` + args.within(scope, "owner_principal_id")
	if !filter.IncludeDeleted {
		query += ` AND deleted_at = 0`
	}
	if tag := strings.TrimSpace(filter.Tag); tag != "" {
		query += ` AND tags @> jsonb_build_array(` + args.add(tag) + `::text)`
	}
	if filter.PrincipalID != "" {
		if !principalIDPattern.MatchString(filter.PrincipalID) {
			return nil, fmt.Errorf("%w: principal_id %q is not a principal-store id (principal_000001)", ErrInvalid, filter.PrincipalID)
		}
		query += ` AND principal_id = ` + args.add(filter.PrincipalID)
	}
	if strings.TrimSpace(filter.Query) != "" {
		expression, err := searchQueryOf(filter.Query)
		if err != nil {
			return nil, err
		}
		query += ` AND search_document @@ to_tsquery('simple', ` + args.add(expression) + `)`
	}
	if filter.Limit < 0 || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit and offset must not be negative", ErrInvalid)
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, fmt.Errorf("%w: limit %d is over 1000", ErrInvalid, limit)
	}
	// The standalone store sorted by SQLite's NOCASE collation: byte order
	// with only ASCII letters folded. translate folds exactly those, and the
	// "C" collation compares bytes, so the order is the same.
	query += fmt.Sprintf(` ORDER BY translate(display_name, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') COLLATE "C", seq LIMIT %d OFFSET %d`, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	persons := []Person{}
	for rows.Next() {
		person, err := scanPerson(rows.Scan)
		if err != nil {
			return nil, err
		}
		persons = append(persons, *person)
	}
	return persons, rows.Err()
}

// PatchPerson changes the fields the patch carries and records the person as
// it was before. A deleted person cannot be patched.
func (s *Store) PatchPerson(ctx context.Context, scope ownership.Scope, id string, patch PersonPatch) (*PersonDetail, error) {
	var sets []string
	var args arguments
	if patch.DisplayName != nil {
		displayName := strings.TrimSpace(*patch.DisplayName)
		if displayName == "" {
			return nil, fmt.Errorf("%w: display_name cannot be empty", ErrInvalid)
		}
		sets = append(sets, "display_name = "+args.add(displayName))
	}
	if patch.Birthday != nil {
		birthday, err := normalizeBirthday(*patch.Birthday)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "birthday = "+args.add(birthday))
	}
	if patch.HowMet != nil {
		sets = append(sets, "how_met = "+args.add(strings.TrimSpace(*patch.HowMet)))
	}
	if patch.Tags != nil {
		tags, err := normalizeTags(*patch.Tags)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(tags)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "tags = "+args.add(string(encoded))+"::jsonb")
	}
	if patch.PrincipalID != nil {
		principalID, err := s.checkPrincipalID(ctx, *patch.PrincipalID)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "principal_id = "+args.add(principalID))
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("%w: the patch changes nothing", ErrInvalid)
	}
	tx, seq, owner, err := s.beginForPerson(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := getLivePerson(ctx, tx, scope, seq); err != nil {
		return nil, err
	}
	if err := recordRevision(ctx, tx, seq, RevisionActionUpdate); err != nil {
		return nil, err
	}
	sets = append(sets, "updated_at = "+args.add(now()))
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET `+strings.Join(sets, ", ")+` WHERE seq = `+args.add(seq), args.values...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return personDetail(ctx, s.db, ownership.OwnedBy(owner), seq)
}

// DeletePerson stamps deleted_at. Nothing is removed: identities stay attached
// (so a deleted person's handles are still theirs), and restore undoes it.
func (s *Store) DeletePerson(ctx context.Context, scope ownership.Scope, id string) (*PersonDetail, error) {
	return s.setDeleted(ctx, scope, id, true)
}

// RestorePerson clears deleted_at.
func (s *Store) RestorePerson(ctx context.Context, scope ownership.Scope, id string) (*PersonDetail, error) {
	return s.setDeleted(ctx, scope, id, false)
}

func (s *Store) setDeleted(ctx context.Context, scope ownership.Scope, id string, deleted bool) (*PersonDetail, error) {
	tx, seq, owner, err := s.beginForPerson(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	person, err := getPerson(ctx, tx, scope, seq)
	if err != nil {
		return nil, err
	}
	if deleted && person.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: person %s is already deleted", ErrConflict, person.ID)
	}
	if !deleted && person.DeletedAt == 0 {
		return nil, fmt.Errorf("%w: person %s is not deleted", ErrConflict, person.ID)
	}
	action, deletedAt := RevisionActionRestore, int64(0)
	if deleted {
		action, deletedAt = RevisionActionDelete, now()
	}
	if err := recordRevision(ctx, tx, seq, action); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET deleted_at = $1, updated_at = $2 WHERE seq = $3`, deletedAt, now(), seq); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return personDetail(ctx, s.db, ownership.OwnedBy(owner), seq)
}
