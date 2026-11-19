package postgresstorage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// A record's public id is its type's prefix and the number its table gave it:
// prediction_000012. The number comes from an identity column, so Postgres
// hands each one out once and never again, including after a deleted row or a
// rolled-back insert. Six digits is the least width, not the most: number
// 1234567 is prediction_1234567.

// FormatRecordID writes the public id of record number of the type prefix names.
func FormatRecordID(prefix string, number int64) string {
	return fmt.Sprintf("%s_%06d", prefix, number)
}

// ParseRecordID reads the number out of a public id of the type prefix names.
// An id of another type, or one that is not prefix, underscore and at least six
// digits, is an error: a caller must never guess a row from a malformed id.
func ParseRecordID(prefix, id string) (int64, error) {
	digits, hasPrefix := strings.CutPrefix(id, prefix+"_")
	if !hasPrefix {
		return 0, fmt.Errorf("postgresstorage: %q is not a %s id", id, prefix)
	}
	if len(digits) < 6 {
		return 0, fmt.Errorf("postgresstorage: %q is not a %s id: it has fewer than six digits", id, prefix)
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("postgresstorage: %q is not a %s id: %q is not a digit", id, prefix, digit)
		}
	}
	number, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || number < 1 {
		return 0, fmt.Errorf("postgresstorage: %q is not a %s id: its number is out of range", id, prefix)
	}
	return number, nil
}

// ContinueNumberingAfter makes the next row inserted into table take the number
// after lastNumber in its identity column. A store moving its records in from
// another database calls it once, after copying them with their numbers, so no
// id that already exists in chats, notes and links is ever handed out again.
// lastNumber 0 means the table starts at 1.
func ContinueNumberingAfter(ctx context.Context, tx *sql.Tx, table, identityColumn string, lastNumber int64) error {
	if err := checkIdentifier("table", table); err != nil {
		return err
	}
	if err := checkIdentifier("column", identityColumn); err != nil {
		return err
	}
	if lastNumber < 0 {
		return fmt.Errorf("postgresstorage: the last number of %s.%s is %d, which is below zero", table, identityColumn, lastNumber)
	}
	var sequence sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT pg_get_serial_sequence($1, $2)", table, identityColumn).Scan(&sequence); err != nil {
		return fmt.Errorf("postgresstorage: finding the sequence of %s.%s: %w", table, identityColumn, err)
	}
	if !sequence.Valid {
		return fmt.Errorf("postgresstorage: %s.%s is not an identity column, so it has no numbering to continue", table, identityColumn)
	}
	// setval(sequence, n, false) makes the next value n itself.
	if _, err := tx.ExecContext(ctx, "SELECT setval($1::regclass, $2, false)", sequence.String, lastNumber+1); err != nil {
		return fmt.Errorf("postgresstorage: continuing the numbering of %s.%s after %d: %w", table, identityColumn, lastNumber, err)
	}
	return nil
}
