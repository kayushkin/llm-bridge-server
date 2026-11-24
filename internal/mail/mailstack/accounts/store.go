package accounts

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Provider string

const (
	ProviderGoogle    Provider = "google"
	ProviderMicrosoft Provider = "microsoft"
	ProviderIMAP      Provider = "imap"
	// ProviderLocal is an account this host owns outright — no provider, no
	// network, no credentials. Its messages are stored locally and served
	// through the same routes as real mail, which is what lets synthetic data
	// exercise every consumer without any of them growing a second code path.
	ProviderLocal Provider = "local"
)

type Account struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Provider        Provider `json:"provider"`
	Address         string   `json:"address"`
	ApiauthProvider string   `json:"apiauth_provider,omitempty"`
	ApiauthAccount  string   `json:"apiauth_account,omitempty"`
	ImapCredName    string   `json:"imap_cred_name,omitempty"`
	Color           string   `json:"color"`
	Signature       string   `json:"signature"`
	Enabled         bool     `json:"enabled"`
	SortOrder       int      `json:"sort_order"`
	CreatedAt       int64    `json:"created_at"`
	UpdatedAt       int64    `json:"updated_at"`
}

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mailstack", "accounts.db")
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	// WAL plus a busy timeout. The index is written on every message list, so
	// readers and that writer now overlap routinely; the previous bare-path DSN
	// left the default rollback journal and NO busy timeout, which turns any
	// overlap straight into SQLITE_BUSY instead of a short wait.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// Everything added after the original three tables goes through migrate, so
	// that a later column change has somewhere to live. CREATE TABLE IF NOT
	// EXISTS cannot alter a table that already exists.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	_ = os.Chmod(path, 0600)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

var ErrNotFound = errors.New("account not found")

func (s *Store) List() ([]*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT id, label, provider, address, apiauth_provider, apiauth_account,
		       imap_cred_name, color, signature, enabled, sort_order, created_at, updated_at
		FROM accounts ORDER BY sort_order, label
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Get(id string) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
		SELECT id, label, provider, address, apiauth_provider, apiauth_account,
		       imap_cred_name, color, signature, enabled, sort_order, created_at, updated_at
		FROM accounts WHERE id = ?
	`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (s *Store) Save(a *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	_, err := s.db.Exec(`
		INSERT INTO accounts (id, label, provider, address, apiauth_provider, apiauth_account,
		                      imap_cred_name, color, signature, enabled, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			label = excluded.label,
			provider = excluded.provider,
			address = excluded.address,
			apiauth_provider = excluded.apiauth_provider,
			apiauth_account = excluded.apiauth_account,
			imap_cred_name = excluded.imap_cred_name,
			color = excluded.color,
			signature = excluded.signature,
			enabled = excluded.enabled,
			sort_order = excluded.sort_order,
			updated_at = excluded.updated_at
	`, a.ID, a.Label, string(a.Provider), a.Address, a.ApiauthProvider, a.ApiauthAccount,
		a.ImapCredName, a.Color, a.Signature, boolInt(a.Enabled), a.SortOrder, a.CreatedAt, a.UpdatedAt)
	return err
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(row scanner) (*Account, error) {
	var a Account
	var provider string
	var enabled int
	err := row.Scan(&a.ID, &a.Label, &provider, &a.Address, &a.ApiauthProvider, &a.ApiauthAccount,
		&a.ImapCredName, &a.Color, &a.Signature, &enabled, &a.SortOrder, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.Provider = Provider(provider)
	a.Enabled = enabled != 0
	return &a, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
