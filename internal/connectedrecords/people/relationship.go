package people

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// listRelationshipViews shows every relationship that touches the person from
// their side: the other person, and what they are to this one. A row where
// this person is the `from` side is shown with the inverse kind.
func listRelationshipViews(ctx context.Context, q queryer, personSeq int64) ([]RelationshipView, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT r.seq, r.owner_principal_id, r.from_person_seq, r.to_person_seq, r.kind, r.note, r.created_at, p.display_name, p.deleted_at
		FROM relationships r
		JOIN persons p ON p.owner_principal_id = r.owner_principal_id
			AND p.seq = CASE WHEN r.from_person_seq = $1 THEN r.to_person_seq ELSE r.from_person_seq END
		WHERE r.from_person_seq = $1 OR r.to_person_seq = $1
		ORDER BY r.seq`, personSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := []RelationshipView{}
	for rows.Next() {
		var view RelationshipView
		var seq, fromPersonSeq, toPersonSeq int64
		var kind string
		if err := rows.Scan(&seq, &view.OwnerPrincipalID, &fromPersonSeq, &toPersonSeq, &kind, &view.Note, &view.CreatedAt, &view.PersonDisplayName, &view.PersonDeletedAt); err != nil {
			return nil, err
		}
		view.ID = formatID(relationshipIDPrefix, seq)
		if toPersonSeq == personSeq {
			// (other, this, kind): the other person is this one's kind.
			view.PersonID, view.Kind = formatID(personIDPrefix, fromPersonSeq), kind
		} else {
			// (this, other, kind): this person is the other's kind, so the
			// other is this one's inverse.
			view.PersonID, view.Kind = formatID(personIDPrefix, toPersonSeq), InverseRelationshipKind(kind)
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// AddRelationship records that input.PersonID is personID's input.Kind. Both
// must be one owner's persons; the relationship takes that owner. The same
// fact stated from either side is one row, and a second statement of it is a
// 409.
func (s *Store) AddRelationship(ctx context.Context, scope ownership.Scope, personID string, input RelationshipInput) (*Relationship, error) {
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
	tx, seq, owner, err := s.beginForPerson(ctx, scope, personID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := getLivePerson(ctx, tx, scope, seq); err != nil {
		return nil, err
	}
	other, err := otherLivePerson(ctx, tx, scope, owner, "person_id", otherID)
	if err != nil {
		return nil, err
	}
	otherSeq := mustNumber(personIDPrefix, other.ID)
	if otherSeq == seq {
		return nil, fmt.Errorf("%w: a person cannot have a relationship with themselves", ErrInvalid)
	}
	// Stored as (from=other, to=this, kind). The mirror (from=this, to=other,
	// inverse) says the same thing and must not be a second row.
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT seq FROM relationships WHERE (from_person_seq = $1 AND to_person_seq = $2 AND kind = $3) OR (from_person_seq = $2 AND to_person_seq = $1 AND kind = $4)`,
		otherSeq, seq, kind.Kind, kind.Inverse).Scan(&existing)
	if err == nil {
		return nil, fmt.Errorf("%w: %s already records that %s is %s's %s", ErrConflict, formatID(relationshipIDPrefix, existing), other.ID, personID, kind.Kind)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	relationship := Relationship{
		OwnerPrincipalID: owner, FromPersonID: other.ID, ToPersonID: formatID(personIDPrefix, seq),
		Kind: kind.Kind, Note: strings.TrimSpace(input.Note), CreatedAt: now(),
	}
	var relationshipSeq int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO relationships (owner_principal_id, from_person_seq, to_person_seq, kind, note, created_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING seq`,
		owner, otherSeq, seq, relationship.Kind, relationship.Note, relationship.CreatedAt).Scan(&relationshipSeq); err != nil {
		return nil, err
	}
	relationship.ID = formatID(relationshipIDPrefix, relationshipSeq)
	return &relationship, tx.Commit()
}

// RemoveRelationship deletes the row, when it is in scope. Nothing joins on a
// relationship id.
func (s *Store) RemoveRelationship(ctx context.Context, scope ownership.Scope, id string) error {
	if err := checkScope(scope); err != nil {
		return err
	}
	seq, err := numberOf(relationshipIDPrefix, id)
	if err != nil {
		return err
	}
	var args arguments
	result, err := s.db.ExecContext(ctx, `DELETE FROM relationships WHERE seq = `+args.add(seq)+` AND `+args.within(scope, "owner_principal_id"), args.values...)
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
