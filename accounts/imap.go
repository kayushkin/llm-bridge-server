package accounts

import (
	"database/sql"
	"errors"
	"time"
)

type TLSMode string

const (
	TLSModeTLS      TLSMode = "tls"
	TLSModeSTARTTLS TLSMode = "starttls"
)

// ImapCred stores credentials for a single IMAP/SMTP mailbox.
// Password is plaintext; protection is filesystem perms (0600 on accounts.db).
type ImapCred struct {
	Name      string  `json:"name"`
	Address   string  `json:"address"`
	ImapHost  string  `json:"imap_host"`
	ImapPort  int     `json:"imap_port"`
	SmtpHost  string  `json:"smtp_host"`
	SmtpPort  int     `json:"smtp_port"`
	Username  string  `json:"username"`
	Password  string  `json:"password,omitempty"`
	TLSMode   TLSMode `json:"tls_mode"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

func (s *Store) ListImapCreds() ([]*ImapCred, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT name, address, imap_host, imap_port, smtp_host, smtp_port,
		       username, password, tls_mode, created_at, updated_at
		FROM imap_creds ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ImapCred
	for rows.Next() {
		c, err := scanImapCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetImapCred(name string) (*ImapCred, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
		SELECT name, address, imap_host, imap_port, smtp_host, smtp_port,
		       username, password, tls_mode, created_at, updated_at
		FROM imap_creds WHERE name = ?
	`, name)
	c, err := scanImapCred(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *Store) SaveImapCred(c *ImapCred) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	_, err := s.db.Exec(`
		INSERT INTO imap_creds (name, address, imap_host, imap_port, smtp_host, smtp_port,
		                       username, password, tls_mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			address = excluded.address,
			imap_host = excluded.imap_host,
			imap_port = excluded.imap_port,
			smtp_host = excluded.smtp_host,
			smtp_port = excluded.smtp_port,
			username = excluded.username,
			password = excluded.password,
			tls_mode = excluded.tls_mode,
			updated_at = excluded.updated_at
	`, c.Name, c.Address, c.ImapHost, c.ImapPort, c.SmtpHost, c.SmtpPort,
		c.Username, c.Password, string(c.TLSMode), c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *Store) DeleteImapCred(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM imap_creds WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanImapCred(row scanner) (*ImapCred, error) {
	var c ImapCred
	var tls string
	err := row.Scan(&c.Name, &c.Address, &c.ImapHost, &c.ImapPort, &c.SmtpHost, &c.SmtpPort,
		&c.Username, &c.Password, &tls, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.TLSMode = TLSMode(tls)
	return &c, nil
}

// ─── imap_folder_map ──────────────────────────────────────────────────
// Caches the provider-native folder ID for each canonical kind per account,
// so we don't reprobe SPECIAL-USE on every request.

func (s *Store) GetFolderMap(accountID string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT canonical, folder_id FROM imap_folder_map WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SaveFolderMap(accountID string, m map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM imap_folder_map WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO imap_folder_map (account_id, canonical, folder_id) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range m {
		if _, err := stmt.Exec(accountID, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
