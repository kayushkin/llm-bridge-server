package postgresstorage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// InTransaction runs work in one transaction: committed when work returns nil,
// rolled back when it returns an error or panics. The error work returned comes
// back unchanged, so a caller can still match it with errors.Is.
func InTransaction(ctx context.Context, db *sql.DB, work func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresstorage: beginning a transaction: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			// work panicked: undo what it did and let the panic go on.
			_ = tx.Rollback()
		}
	}()
	workErr := work(tx)
	finished = true
	if workErr != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(workErr, fmt.Errorf("postgresstorage: rolling back: %w", rollbackErr))
		}
		return workErr
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresstorage: committing: %w", err)
	}
	return nil
}
