package peoplestore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// listRelationshipViews shows every relationship that touches personID from
// personID's side: the other person, and what they are to this one. A row
// where this person is the `from` side is shown with the inverse kind.
func listRelationshipViews(q queryer, personID string) ([]RelationshipView, error) {
	rows, err := q.Query(`
		SELECT r.id, r.from_person_id, r.to_person_id, r.kind, r.note, r.created_at, p.display_name, p.deleted_at
		FROM relationships r
		JOIN persons p ON p.id = CASE WHEN r.from_person_id = ? THEN r.to_person_id ELSE r.from_person_id END
		WHERE r.from_person_id = ? OR r.to_person_id = ?
		ORDER BY r.seq`, personID, personID, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := []RelationshipView{}
	for rows.Next() {
		var view RelationshipView
		var fromPersonID, toPersonID, kind string
		if err := rows.Scan(&view.ID, &fromPersonID, &toPersonID, &kind, &view.Note, &view.CreatedAt, &view.PersonDisplayName, &view.PersonDeletedAt); err != nil {
			return nil, err
		}
		if toPersonID == personID {
			// (other, this, kind): the other person is this one's kind.
			view.PersonID, view.Kind = fromPersonID, kind
		} else {
			// (this, other, kind): this person is the other's kind, so the
			// other is this one's inverse.
			view.PersonID, view.Kind = toPersonID, InverseRelationshipKind(kind)
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// AddRelationship records that input.PersonID is personID's input.Kind. The
// same fact stated from either side is one row, and a second statement of it
// is a 409.
func (s *Store) AddRelationship(personID string, input RelationshipInput) (*Relationship, error) {
	kind, err := relationshipKindFor(input.Kind)
	if err != nil {
		return nil, err
	}
	otherID := strings.TrimSpace(input.PersonID)
	if otherID == "" {
		return nil, fmt.Errorf("%w: person_id is required", ErrInvalid)
	}
	if otherID == personID {
		return nil, fmt.Errorf("%w: a person cannot have a relationship with themselves", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := getLivePerson(tx, personID); err != nil {
		return nil, err
	}
	if _, err := getLivePerson(tx, otherID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: person_id: %v", ErrInvalid, err)
		}
		return nil, err
	}
	// Stored as (from=other, to=this, kind). The mirror (from=this, to=other,
	// inverse) says the same thing and must not be a second row.
	var existing string
	err = tx.QueryRow(`SELECT id FROM relationships WHERE (from_person_id = ? AND to_person_id = ? AND kind = ?) OR (from_person_id = ? AND to_person_id = ? AND kind = ?)`,
		otherID, personID, kind.Kind, personID, otherID, kind.Inverse).Scan(&existing)
	if err == nil {
		return nil, fmt.Errorf("%w: %s already records that %s is %s's %s", ErrConflict, existing, otherID, personID, kind.Kind)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	seq, err := nextSeq(tx, "relationships")
	if err != nil {
		return nil, err
	}
	relationship := Relationship{
		ID: formatID(relationshipIDPrefix, seq), FromPersonID: otherID, ToPersonID: personID,
		Kind: kind.Kind, Note: strings.TrimSpace(input.Note), CreatedAt: now(),
	}
	if _, err := tx.Exec(`INSERT INTO relationships (id, seq, from_person_id, to_person_id, kind, note, created_at) VALUES (?,?,?,?,?,?,?)`,
		relationship.ID, seq, relationship.FromPersonID, relationship.ToPersonID, relationship.Kind, relationship.Note, relationship.CreatedAt); err != nil {
		return nil, err
	}
	return &relationship, tx.Commit()
}

// RemoveRelationship deletes the row. Nothing joins on a relationship id.
func (s *Store) RemoveRelationship(id string) error {
	result, err := s.db.Exec(`DELETE FROM relationships WHERE id = ?`, id)
	if err != nil {
		return err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if removed == 0 {
		return fmt.Errorf("%w: relationship %s", ErrNotFound, id)
	}
	return nil
}
