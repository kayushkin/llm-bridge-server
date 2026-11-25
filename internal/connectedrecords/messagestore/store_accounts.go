package messagestore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const accountColumns = `id, platform, handle, display_name, kind, credential_id, created_at, updated_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var account Account
	err := row.Scan(&account.ID, &account.Platform, &account.Handle, &account.DisplayName, &account.Kind, &account.CredentialID, &account.CreatedAt, &account.UpdatedAt)
	return account, err
}

// PutAccount finds the account by (platform, handle) and makes it if new. A
// display name, credential or kind it sends replaces the stored one; an
// empty display name or credential keeps it.
func (s *Store) PutAccount(put AccountPut) (Account, error) {
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
	tx, err := s.db.Begin()
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	now := nowMillis()
	var id string
	err = tx.QueryRow(`SELECT id FROM accounts WHERE platform = ? AND handle = ?`, put.Platform, put.Handle).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		seq, err := nextSeq(tx, "accounts")
		if err != nil {
			return Account{}, err
		}
		id = formatID(prefixAccount, seq)
		if _, err := tx.Exec(`INSERT INTO accounts (seq, id, platform, handle, display_name, kind, credential_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			seq, id, put.Platform, put.Handle, put.DisplayName, put.Kind, put.CredentialID, now, now); err != nil {
			return Account{}, err
		}
	case err != nil:
		return Account{}, err
	default:
		if _, err := tx.Exec(`UPDATE accounts SET display_name = CASE WHEN ? != '' THEN ? ELSE display_name END,
				credential_id = CASE WHEN ? != '' THEN ? ELSE credential_id END, kind = ?, updated_at = ?
			WHERE id = ? AND (kind != ? OR (? != '' AND display_name != ?) OR (? != '' AND credential_id != ?))`,
			put.DisplayName, put.DisplayName, put.CredentialID, put.CredentialID, put.Kind, now,
			id, put.Kind, put.DisplayName, put.DisplayName, put.CredentialID, put.CredentialID); err != nil {
			return Account{}, err
		}
	}
	account, err := scanAccount(tx.QueryRow(`SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id))
	if err != nil {
		return Account{}, err
	}
	return account, tx.Commit()
}

// GetAccount reads one account.
func (s *Store) GetAccount(id string) (Account, error) {
	account, err := scanAccount(s.db.QueryRow(`SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: account %s", ErrNotFound, id)
	}
	return account, err
}

// ListAccounts lists every account, the user's own first, then in the order
// they were made.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountColumns + ` FROM accounts ORDER BY kind = 'user' DESC, seq`)
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
