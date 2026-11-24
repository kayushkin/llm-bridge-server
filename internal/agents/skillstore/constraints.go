package skillstore

import (
	"errors"
	"fmt"

	"github.com/mattn/go-sqlite3"
)

// ErrReferencedRowNotFound is returned when a write names a parent row that
// does not exist — a bad source_id on a skill, or a bad skill_id on a seed
// version or an observed hash.
//
// It exists because the driver's own answer to that mistake is the bare string
// "FOREIGN KEY constraint failed", which names no table, no column and not the
// id that was wrong. The HTTP edge has nothing to match on, so a caller's
// correctable mistake was reported as a 500 — a server fault — instead of a
// 404. Callers should match this with errors.Is rather than on message text.
var ErrReferencedRowNotFound = errors.New("referenced row does not exist")

// wrapForeignKeyViolation returns err unchanged unless it is SQLite refusing a
// write for want of the parent row, in which case it returns an error that
// names the id that was wrong and wraps ErrReferencedRowNotFound.
//
// The classification reads the driver's extended result code rather than
// matching the message: SQLITE_CONSTRAINT_FOREIGNKEY is the only thing that
// means this, and the message is neither stable nor unique to it.
func wrapForeignKeyViolation(err error, referencedBy string, id int64) error {
	if err == nil {
		return nil
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.ExtendedCode != sqlite3.ErrConstraintForeignKey {
		return err
	}
	return fmt.Errorf("%s %d: %w", referencedBy, id, ErrReferencedRowNotFound)
}
