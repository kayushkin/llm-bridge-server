package accounts

import (
	"database/sql"
	"fmt"
)

// ============================ schema migrations ============================
//
// Until the message index there was no migration path here at all: schema.go is
// one block of CREATE TABLE IF NOT EXISTS run at every open, which creates a
// missing table but can never add a column to a table that already exists. That
// was survivable while the three original tables were stable. It stops being
// survivable now, because the message index is the first table here that is
// expected to grow columns.
//
// So: SQLite's user_version pragma, an integer stored in the database header.
// Each migration is applied once, in order, inside a transaction, and the
// version is bumped in the same transaction. An unknown-but-higher version (a
// database written by a newer binary) is refused rather than downgraded.

// migrations are applied in order. Append only; never edit or reorder an entry
// that has shipped, because a database that already ran it will not run it
// again and the two would silently diverge.
var migrations = []struct {
	name string
	stmt string
}{
	{
		name: "0001_messages_index",
		stmt: messagesSchema,
	},
}

func migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database is at schema version %d but this binary only knows %d — "+
			"it was written by a newer mailstack; refusing to run rather than corrupt it",
			current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		m := migrations[i]
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(m.stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		// PRAGMA does not accept a bound parameter, and i+1 is a loop index
		// rather than anything caller-supplied, so the format is safe here.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump user_version after %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.name, err)
		}
	}
	return nil
}
