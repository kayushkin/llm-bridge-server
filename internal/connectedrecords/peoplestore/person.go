package peoplestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PersonFilter selects persons. The zero value lists every live person.
type PersonFilter struct {
	Query string
	Tag   string
	// PrincipalID keeps only the person pointing at this principal-store id.
	PrincipalID    string
	IncludeDeleted bool
	Limit          int
	Offset         int
}

const personColumns = `id, display_name, birthday, how_met, tags, principal_id, created_at, updated_at, deleted_at`

func scanPerson(scan func(...any) error) (*Person, error) {
	var person Person
	var tags string
	if err := scan(&person.ID, &person.DisplayName, &person.Birthday, &person.HowMet, &tags, &person.PrincipalID,
		&person.CreatedAt, &person.UpdatedAt, &person.DeletedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tags), &person.Tags); err != nil {
		return nil, fmt.Errorf("person %s: tags: %w", person.ID, err)
	}
	return &person, nil
}

func getPerson(q queryer, id string) (*Person, error) {
	person, err := scanPerson(q.QueryRow(`SELECT `+personColumns+` FROM persons WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: person %s", ErrNotFound, id)
	}
	return person, err
}

// getLivePerson is getPerson that refuses a deleted person with a 409, for
// writes that would hang something new on them.
func getLivePerson(q queryer, id string) (*Person, error) {
	person, err := getPerson(q, id)
	if err != nil {
		return nil, err
	}
	if person.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: person %s is deleted; POST /persons/%s/restore first", ErrConflict, id, id)
	}
	return person, nil
}

// CreatePerson records a new person and hands back its id.
func (s *Store) CreatePerson(input PersonInput) (*PersonDetail, error) {
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
	principalID, err := s.checkPrincipalID(input.PrincipalID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, err := insertPerson(tx, displayName, birthday, strings.TrimSpace(input.HowMet), tags, principalID)
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if _, _, err := addIdentity(tx, id, link.input, link.source, ""); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPerson(id)
}

func insertPerson(tx *sql.Tx, displayName, birthday, howMet string, tags []string, principalID string) (string, error) {
	encodedTags, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	seq, err := nextSeq(tx, "persons")
	if err != nil {
		return "", err
	}
	id := formatID(personIDPrefix, seq)
	timestamp := now()
	if _, err := tx.Exec(`INSERT INTO persons (id, seq, display_name, birthday, how_met, tags, principal_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, id, seq, displayName, birthday, howMet, string(encodedTags), principalID, timestamp, timestamp); err != nil {
		return "", err
	}
	return id, reindexPerson(tx, id)
}

// GetPerson returns one person with identities and relationships. A deleted
// person is returned like any other, deleted_at set, so that whatever points
// at them still renders a name.
func (s *Store) GetPerson(id string) (*PersonDetail, error) {
	person, err := getPerson(s.db, id)
	if err != nil {
		return nil, err
	}
	detail := &PersonDetail{Person: *person}
	if detail.Identities, err = listIdentities(s.db, id); err != nil {
		return nil, err
	}
	if detail.Relationships, err = listRelationshipViews(s.db, id); err != nil {
		return nil, err
	}
	return detail, nil
}

// ListPersons returns persons in display-name order, without identities or
// relationships.
func (s *Store) ListPersons(filter PersonFilter) ([]Person, error) {
	query := `SELECT ` + personColumns + ` FROM persons WHERE 1=1`
	var args []any
	if !filter.IncludeDeleted {
		query += ` AND deleted_at = 0`
	}
	if tag := strings.TrimSpace(filter.Tag); tag != "" {
		query += ` AND EXISTS (SELECT 1 FROM json_each(persons.tags) WHERE json_each.value = ?)`
		args = append(args, tag)
	}
	if filter.PrincipalID != "" {
		if !principalIDPattern.MatchString(filter.PrincipalID) {
			return nil, fmt.Errorf("%w: principal_id %q is not a principal-store id (principal_000001)", ErrInvalid, filter.PrincipalID)
		}
		query += ` AND principal_id = ?`
		args = append(args, filter.PrincipalID)
	}
	if strings.TrimSpace(filter.Query) != "" {
		expression := searchExpression(filter.Query)
		if err := s.validateSearchExpression(expression); err != nil {
			return nil, err
		}
		query += ` AND seq IN (SELECT rowid FROM persons_fts WHERE persons_fts MATCH ?)`
		args = append(args, expression)
	}
	query += ` ORDER BY display_name COLLATE NOCASE, seq`
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
	query += fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, filter.Offset)
	rows, err := s.db.Query(query, args...)
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

// searchExpression turns typed words into prefix terms, so "ali" finds Alice.
// Anything already carrying FTS5 syntax is passed through as it is.
func searchExpression(q string) string {
	q = strings.TrimSpace(q)
	if strings.ContainsAny(q, `"*:()`) {
		return q
	}
	words := strings.Fields(q)
	for _, word := range words {
		switch word {
		case "AND", "OR", "NOT", "NEAR":
			return q
		}
	}
	terms := make([]string, 0, len(words))
	for _, word := range words {
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

// validateSearchExpression probes the index so a malformed query is the
// caller's 400, not a 500 from inside the list.
func (s *Store) validateSearchExpression(expression string) error {
	var discard int
	err := s.db.QueryRow(`SELECT 1 FROM persons_fts WHERE persons_fts MATCH ? LIMIT 1`, expression).Scan(&discard)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("%w: bad search query %q: %v", ErrInvalid, expression, err)
}

// PatchPerson changes the fields the patch carries and records the person as
// it was before. A deleted person cannot be patched.
func (s *Store) PatchPerson(id string, patch PersonPatch) (*PersonDetail, error) {
	var sets []string
	var args []any
	if patch.DisplayName != nil {
		displayName := strings.TrimSpace(*patch.DisplayName)
		if displayName == "" {
			return nil, fmt.Errorf("%w: display_name cannot be empty", ErrInvalid)
		}
		sets, args = append(sets, "display_name = ?"), append(args, displayName)
	}
	if patch.Birthday != nil {
		birthday, err := normalizeBirthday(*patch.Birthday)
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "birthday = ?"), append(args, birthday)
	}
	if patch.HowMet != nil {
		sets, args = append(sets, "how_met = ?"), append(args, strings.TrimSpace(*patch.HowMet))
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
		sets, args = append(sets, "tags = ?"), append(args, string(encoded))
	}
	if patch.PrincipalID != nil {
		principalID, err := s.checkPrincipalID(*patch.PrincipalID)
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "principal_id = ?"), append(args, principalID)
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("%w: the patch changes nothing", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := getLivePerson(tx, id); err != nil {
		return nil, err
	}
	if err := recordRevision(tx, id, RevisionActionUpdate); err != nil {
		return nil, err
	}
	sets, args = append(sets, "updated_at = ?"), append(args, now(), id)
	if _, err := tx.Exec(`UPDATE persons SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return nil, err
	}
	if err := reindexPerson(tx, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPerson(id)
}

// DeletePerson stamps deleted_at. Nothing is removed: identities stay attached
// (so a deleted person's handles are still theirs), and restore undoes it.
func (s *Store) DeletePerson(id string) (*PersonDetail, error) {
	return s.setDeleted(id, true)
}

// RestorePerson clears deleted_at.
func (s *Store) RestorePerson(id string) (*PersonDetail, error) {
	return s.setDeleted(id, false)
}

func (s *Store) setDeleted(id string, deleted bool) (*PersonDetail, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	person, err := getPerson(tx, id)
	if err != nil {
		return nil, err
	}
	if deleted && person.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: person %s is already deleted", ErrConflict, id)
	}
	if !deleted && person.DeletedAt == 0 {
		return nil, fmt.Errorf("%w: person %s is not deleted", ErrConflict, id)
	}
	action, deletedAt := RevisionActionRestore, int64(0)
	if deleted {
		action, deletedAt = RevisionActionDelete, now()
	}
	if err := recordRevision(tx, id, action); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE persons SET deleted_at = ?, updated_at = ? WHERE id = ?`, deletedAt, now(), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPerson(id)
}
