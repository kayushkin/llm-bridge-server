package usagestore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LimitWindow represents a single rate-limit window for a provider.
//
// Field semantics intentionally match the union of:
//   - Anthropic /api/oauth/usage (utilization 0-100, resets_at ISO string)
//   - Codex app-server rate limits (usedPercent 0-100, windowDurationMins, resetsAt unix sec)
//
// Both shapes are normalized here. ResetsAt is always unix seconds.
type LimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes,omitempty"`
	ResetsAt      *int64  `json:"resets_at,omitempty"` // unix seconds
}

// ProviderLimits is the latest snapshot for one provider.
type ProviderLimits struct {
	Provider   string                  `json:"provider"`              // "anthropic", "codex"
	PlanType   string                  `json:"plan_type,omitempty"`   // "max", "plus", etc.
	Tier       string                  `json:"tier,omitempty"`        // anthropic rate_limit_tier
	Windows    map[string]*LimitWindow `json:"windows"`               // keyed by window name
	SnapshotAt int64                   `json:"snapshot_at"`           // unix seconds
	Source     string                  `json:"source"`                // "api", "app-server"
	StaleAfter *int64                  `json:"stale_after,omitempty"` // unix seconds when the snapshot becomes stale; set by the server, not stored
}

// IsStale returns true if StaleAfter is set and in the past.
func (p *ProviderLimits) IsStale() bool {
	if p == nil || p.StaleAfter == nil {
		return false
	}
	return time.Now().Unix() > *p.StaleAfter
}

// migrateLimits creates the limit_snapshots table.
func (s *Store) migrateLimits() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS limit_snapshots (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_at    INTEGER NOT NULL,
			provider       TEXT NOT NULL,
			window_key     TEXT NOT NULL,
			used_percent   REAL NOT NULL,
			window_minutes INTEGER,
			resets_at      INTEGER,
			plan_type      TEXT,
			tier           TEXT,
			source         TEXT NOT NULL,
			raw_json       TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_limit_snapshots_lookup
			ON limit_snapshots(provider, window_key, snapshot_at DESC);
		CREATE INDEX IF NOT EXISTS idx_limit_snapshots_at
			ON limit_snapshots(snapshot_at DESC);
	`)
	return err
}

// SaveLimits persists a full provider snapshot. One row per window.
// snapshot_at must be set on the input; if zero, time.Now() is used.
func (s *Store) SaveLimits(p ProviderLimits, rawJSON []byte) error {
	if p.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if p.SnapshotAt == 0 {
		p.SnapshotAt = time.Now().Unix()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO limit_snapshots
			(snapshot_at, provider, window_key, used_percent, window_minutes, resets_at, plan_type, tier, source, raw_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	rawStr := string(rawJSON)
	for key, win := range p.Windows {
		if win == nil {
			continue
		}
		if _, err := stmt.Exec(
			p.SnapshotAt, p.Provider, key, win.UsedPercent,
			win.WindowMinutes, win.ResetsAt, p.PlanType, p.Tier, p.Source, rawStr,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestLimits returns the most recent snapshot for a provider, reconstructed across all windows.
// Returns nil (no error) if no snapshot exists.
func (s *Store) LatestLimits(provider string) (*ProviderLimits, error) {
	// Find the snapshot_at of the most recent set of rows for this provider.
	var snapAt int64
	err := s.db.QueryRow(
		`SELECT MAX(snapshot_at) FROM limit_snapshots WHERE provider = ?`, provider,
	).Scan(&snapAt)
	if err == sql.ErrNoRows || snapAt == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT window_key, used_percent, window_minutes, resets_at, plan_type, tier, source
		FROM limit_snapshots WHERE provider = ? AND snapshot_at = ?
	`, provider, snapAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &ProviderLimits{Provider: provider, Windows: map[string]*LimitWindow{}, SnapshotAt: snapAt}
	for rows.Next() {
		var (
			key        string
			usedPct    float64
			winMin     sql.NullInt64
			resets     sql.NullInt64
			plan, tier sql.NullString
			source     string
		)
		if err := rows.Scan(&key, &usedPct, &winMin, &resets, &plan, &tier, &source); err != nil {
			return nil, err
		}
		w := &LimitWindow{UsedPercent: usedPct}
		if winMin.Valid {
			v := winMin.Int64
			w.WindowMinutes = &v
		}
		if resets.Valid {
			v := resets.Int64
			w.ResetsAt = &v
		}
		out.Windows[key] = w
		if plan.Valid {
			out.PlanType = plan.String
		}
		if tier.Valid {
			out.Tier = tier.String
		}
		out.Source = source
	}
	return out, nil
}

// LimitHistoryFilter scopes a history query.
type LimitHistoryFilter struct {
	Provider string // required
	Window   string // optional
	Since    int64  // unix seconds; 0 = no lower bound
	Until    int64  // unix seconds; 0 = no upper bound
	Limit    int    // 0 = no limit
}

// LimitHistoryRow is one snapshot row returned by HistoryLimits.
type LimitHistoryRow struct {
	SnapshotAt    int64   `json:"snapshot_at"`
	WindowKey     string  `json:"window_key"`
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes,omitempty"`
	ResetsAt      *int64  `json:"resets_at,omitempty"`
	PlanType      string  `json:"plan_type,omitempty"`
	Source        string  `json:"source"`
}

// HistoryLimits returns snapshot rows ordered by snapshot_at ascending.
func (s *Store) HistoryLimits(f LimitHistoryFilter) ([]LimitHistoryRow, error) {
	if f.Provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	q := `SELECT snapshot_at, window_key, used_percent, window_minutes, resets_at, plan_type, source
	      FROM limit_snapshots WHERE provider = ?`
	args := []interface{}{f.Provider}
	if f.Window != "" {
		q += " AND window_key = ?"
		args = append(args, f.Window)
	}
	if f.Since > 0 {
		q += " AND snapshot_at >= ?"
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		q += " AND snapshot_at <= ?"
		args = append(args, f.Until)
	}
	q += " ORDER BY snapshot_at ASC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LimitHistoryRow
	for rows.Next() {
		var r LimitHistoryRow
		var winMin, resets sql.NullInt64
		var plan sql.NullString
		if err := rows.Scan(&r.SnapshotAt, &r.WindowKey, &r.UsedPercent, &winMin, &resets, &plan, &r.Source); err != nil {
			return nil, err
		}
		if winMin.Valid {
			v := winMin.Int64
			r.WindowMinutes = &v
		}
		if resets.Valid {
			v := resets.Int64
			r.ResetsAt = &v
		}
		if plan.Valid {
			r.PlanType = plan.String
		}
		out = append(out, r)
	}
	return out, nil
}

// MarshalProviderJSON is a small helper for collectors to round-trip raw payloads.
func MarshalProviderJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
