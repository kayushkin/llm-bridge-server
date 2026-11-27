package usage

import (
	"database/sql"
	"fmt"
	"time"
)

// KeyMeta is the per-API-key metadata snapshot. One row per (provider,
// api_key_id), upserted on every refresh. Daily spend lives in spend_daily —
// totals over arbitrary windows are computed from there at read time so
// "remaining since baseline" works even when the baseline is older than 30d.
type KeyMeta struct {
	Provider     string `json:"provider"`
	APIKeyID     string `json:"api_key_id"`
	APIKeyName   string `json:"api_key_name"`
	APIKeyHint   string `json:"api_key_hint"`
	APIKeyStatus string `json:"api_key_status"`
	RawJSON      string `json:"raw_json,omitempty"`
	FetchedAt    int64  `json:"fetched_at"`
}

// DailySpend is one (api_key, day) row.
type DailySpend struct {
	Provider  string
	APIKeyID  string
	Date      string // YYYY-MM-DD UTC
	TotalUSD  float64
	FetchedAt int64
}

// Topup is a recorded credit deposit for a provider account. Used to compute
// the remaining-balance figure on the Usage page.
type Topup struct {
	ID         int64   `json:"id"`
	Provider   string  `json:"provider"`
	AmountUSD  float64 `json:"amount_usd"`
	OccurredAt int64   `json:"occurred_at"` // unix sec
	Note       string  `json:"note"`
	CreatedAt  int64   `json:"created_at"`
}

// SaveKeyMeta upserts the metadata + raw JSON for one API key.
func (s *Store) SaveKeyMeta(m KeyMeta) error {
	if m.Provider == "" || m.APIKeyID == "" {
		return fmt.Errorf("provider and api_key_id are required")
	}
	if m.FetchedAt == 0 {
		m.FetchedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`
		INSERT INTO spend_keys
			(provider, api_key_id, api_key_name, api_key_hint, api_key_status, raw_json, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT(provider, api_key_id) DO UPDATE SET
			api_key_name   = excluded.api_key_name,
			api_key_hint   = excluded.api_key_hint,
			api_key_status = excluded.api_key_status,
			raw_json       = excluded.raw_json,
			fetched_at     = excluded.fetched_at`,
		m.Provider, m.APIKeyID, m.APIKeyName, m.APIKeyHint, m.APIKeyStatus, m.RawJSON, m.FetchedAt,
	)
	return err
}

// SaveDailySpend upserts one (key, day) total.
func (s *Store) SaveDailySpend(d DailySpend) error {
	if d.FetchedAt == 0 {
		d.FetchedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`
		INSERT INTO spend_daily
			(provider, api_key_id, date, total_usd, fetched_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT(provider, api_key_id, date) DO UPDATE SET
			total_usd  = excluded.total_usd,
			fetched_at = excluded.fetched_at`,
		d.Provider, d.APIKeyID, d.Date, d.TotalUSD, d.FetchedAt,
	)
	return err
}

// ListKeyMeta returns metadata for every key seen for a provider, ordered
// newest-active first then archived last.
func (s *Store) ListKeyMeta(provider string) ([]KeyMeta, error) {
	rows, err := s.db.Query(`
		SELECT provider, api_key_id, api_key_name, api_key_hint, api_key_status, fetched_at
		FROM spend_keys WHERE provider = $1
		ORDER BY (api_key_status = 'active') DESC, api_key_name ASC`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyMeta{}
	for rows.Next() {
		var k KeyMeta
		if err := rows.Scan(&k.Provider, &k.APIKeyID, &k.APIKeyName, &k.APIKeyHint, &k.APIKeyStatus, &k.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LatestKeyRaw returns the raw_json for one key.
func (s *Store) LatestKeyRaw(provider, apiKeyID string) (string, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`
		SELECT raw_json FROM spend_keys WHERE provider = $1 AND api_key_id = $2`,
		provider, apiKeyID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return raw.String, nil
}

// SumSpendSince returns total USD spent for one key (or all keys if apiKeyID
// is empty) over [sinceUnix, +inf). Uses YYYY-MM-DD comparison since dates
// are stored as strings; sinceUnix is converted to UTC date.
func (s *Store) SumSpendSince(provider, apiKeyID string, sinceUnix int64) (float64, error) {
	since := time.Unix(sinceUnix, 0).UTC().Format("2006-01-02")
	var total sql.NullFloat64
	var err error
	if apiKeyID == "" {
		err = s.db.QueryRow(`
			SELECT COALESCE(SUM(total_usd), 0) FROM spend_daily
			WHERE provider = $1 AND date >= $2`,
			provider, since,
		).Scan(&total)
	} else {
		err = s.db.QueryRow(`
			SELECT COALESCE(SUM(total_usd), 0) FROM spend_daily
			WHERE provider = $1 AND api_key_id = $2 AND date >= $3`,
			provider, apiKeyID, since,
		).Scan(&total)
	}
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

// PerKeyTotals returns {api_key_id: usd} summed for [sinceUnix, +inf).
func (s *Store) PerKeyTotals(provider string, sinceUnix int64) (map[string]float64, error) {
	since := time.Unix(sinceUnix, 0).UTC().Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT api_key_id, SUM(total_usd) FROM spend_daily
		WHERE provider = $1 AND date >= $2
		GROUP BY api_key_id`,
		provider, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var total float64
		if err := rows.Scan(&id, &total); err != nil {
			return nil, err
		}
		out[id] = total
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AddTopup records a credit deposit. CreatedAt defaults to now.
func (s *Store) AddTopup(t Topup) (int64, error) {
	if t.Provider == "" {
		return 0, fmt.Errorf("provider required")
	}
	if t.AmountUSD <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}
	if t.OccurredAt == 0 {
		return 0, fmt.Errorf("occurred_at required")
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().Unix()
	}
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO spend_topups (provider, amount_usd, occurred_at, note, created_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		t.Provider, t.AmountUSD, t.OccurredAt, t.Note, t.CreatedAt,
	).Scan(&id)
	return id, err
}

// ListTopups returns top-ups for a provider, ordered earliest first.
func (s *Store) ListTopups(provider string) ([]Topup, error) {
	rows, err := s.db.Query(`
		SELECT id, provider, amount_usd, occurred_at, note, created_at
		FROM spend_topups WHERE provider = $1
		ORDER BY occurred_at ASC, id ASC`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Topup{}
	for rows.Next() {
		var t Topup
		if err := rows.Scan(&t.ID, &t.Provider, &t.AmountUSD, &t.OccurredAt, &t.Note, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteTopup removes one top-up by ID. No-op if not found.
func (s *Store) DeleteTopup(id int64) error {
	_, err := s.db.Exec(`DELETE FROM spend_topups WHERE id = $1`, id)
	return err
}
