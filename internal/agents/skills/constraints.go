package skills

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrReferencedRowNotFound is returned when a write names a parent row that
// does not exist — a bad source_id on a skill, or a bad skill_id on a seed
// version or an observed hash.
//
// It exists because the database's own answer to that mistake names a
// constraint, not the id that was wrong, and gives the HTTP edge nothing to
// match on, so a caller's correctable mistake would be reported as a 500 — a
// server fault — instead of a 404. Callers should match this with errors.Is rather than on message text.
var ErrReferencedRowNotFound = errors.New("referenced row does not exist")

// wrapForeignKeyViolation returns err unchanged unless it is Postgres refusing
// a write for want of the parent row, in which case it returns an error that
// names the id that was wrong and wraps ErrReferencedRowNotFound.
//
// The classification reads the SQLSTATE rather than matching the message:
// foreign_key_violation (23503) is the only thing that means this, and the
// message is neither stable nor unique to it.
func wrapForeignKeyViolation(err error, referencedBy string, id int64) error {
	if err == nil {
		return nil
	}
	var postgresErr *pgconn.PgError
	if !errors.As(err, &postgresErr) || postgresErr.Code != foreignKeyViolation {
		return err
	}
	return fmt.Errorf("%s %d: %w", referencedBy, id, ErrReferencedRowNotFound)
}

// foreignKeyViolation is the SQLSTATE Postgres reports when a write names a
// parent row that does not exist.
const foreignKeyViolation = "23503"
