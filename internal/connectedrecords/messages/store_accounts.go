package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

const accountColumns = `seq, owner_principal_id, platform, handle, display_name, kind, credential_id, created_at, updated_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var account Account
	var seq int64
	err := row.Scan(&seq, &account.OwnerPrincipalID, &account.Platform, &account.Handle, &account.DisplayName, &account.Kind, &account.CredentialID, &account.CreatedAt, &account.UpdatedAt)
	account.ID = postgresstorage.FormatRecordID(AccountIDPrefix, seq)
	return account, err
}

// PutAccount finds ownerPrincipalID's account by (platform, handle) and makes
// it if new. A display name, credential or kind it sends replaces the stored
// one; an empty display name or credential keeps it.
func (s *Store) PutAccount(ctx context.Context, ownerPrincipalID string, put AccountPut) (Account, error) {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return Account{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if strings.TrimSpace(put.Handle) == "" {
		return Account{}, fmt.Errorf("%w: handle is required", ErrInvalid)
	}
	if !contains(Platforms, put.Platform) {
		return Account{}, fmt.Errorf("%w: platform %q is not one of %v", ErrInvalid, put.Platform, Platforms)
	}
	if !contains(AccountKinds, put.Kind) {
		return Account{}, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalid, put.Kind, AccountKinds)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	var account Account
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		now := nowMillis()
		var seq int64
		err := tx.QueryRowContext(ctx, `SELECT seq FROM accounts WHERE owner_principal_id = $1 AND platform = $2 AND handle = $3`,
			ownerPrincipalID, put.Platform, put.Handle).Scan(&seq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := tx.QueryRowContext(ctx, `INSERT INTO accounts (owner_principal_id, platform, handle, display_name, kind, credential_id, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING seq`,
				ownerPrincipalID, put.Platform, put.Handle, put.DisplayName, put.Kind, put.CredentialID, now).Scan(&seq); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET
					display_name = CASE WHEN $1 <> '' THEN $1 ELSE display_name END,
					credential_id = CASE WHEN $2 <> '' THEN $2 ELSE credential_id END,
					kind = $3, updated_at = $4
				WHERE seq = $5 AND (kind <> $3 OR ($1 <> '' AND display_name <> $1) OR ($2 <> '' AND credential_id <> $2))`,
				put.DisplayName, put.CredentialID, put.Kind, now, seq); err != nil {
				return err
			}
		}
		account, err = scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE seq = $1`, seq))
		return err
	})
	return account, err
}

// GetAccount reads one account in scope.
func (s *Store) GetAccount(ctx context.Context, scope ownership.Scope, id string) (Account, error) {
	seq, err := recordNumber(AccountIDPrefix, "account", id)
	if err != nil {
		return Account{}, err
	}
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return Account{}, err
	}
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE seq = `+args.add(seq)+` AND `+condition, args.values...))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: account %s", ErrNotFound, id)
	}
	return account, err
}

// ListAccounts lists every account in scope, the user's own first, then in
// the order they were made.
func (s *Store) ListAccounts(ctx context.Context, scope ownership.Scope) ([]Account, error) {
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE `+condition+` ORDER BY kind = 'user' DESC, seq`, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []Account{}
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}
