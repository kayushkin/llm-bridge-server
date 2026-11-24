package modelstore

import (
	"time"
)

// ModelHealth tracks the responsiveness of a model.
type ModelHealth struct {
	Model          string    `json:"model"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty"`
	LastSuccessMs  int64     `json:"last_success_ms,omitempty"`  // response duration
	AvgResponseMs  int64     `json:"avg_response_ms,omitempty"` // rolling average
	LastErrorAt    time.Time `json:"last_error_at,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	ConsecutiveErr int       `json:"consecutive_errors,omitempty"`
}

// LastRecordedOutcomeWasAnError reports whether the most recent outcome written
// for this model was a failure.
//
// It reads ConsecutiveErr rather than comparing LastErrorAt against
// LastSuccessAt, and that is the whole point. RecordSuccess zeroes the counter
// and RecordError increments it, so the counter *is* the last-outcome bit,
// recorded by the writer that knew the answer. The timestamps cannot answer it:
// they are stored as unix seconds, so an error arriving in the same second as
// the preceding success is not "after" it, and the comparison silently reported
// a failing model as healthy. A turn that fails fast — a 400, a 404, an
// immediately-refused request — fails well inside one second.
//
// Do not reintroduce an ordering test on these two timestamps. They are kept for
// display and diagnostics; at second resolution they cannot order two events in
// the same second, and health must not depend on how long a request took to fail.
func (h *ModelHealth) LastRecordedOutcomeWasAnError() bool {
	return h != nil && h.ConsecutiveErr > 0
}

// IsHealthy returns true if the model has responded successfully within the window
// and its last interaction wasn't an error.
func (h *ModelHealth) IsHealthy(window time.Duration) bool {
	if h == nil || h.LastSuccessAt.IsZero() {
		return false // never seen
	}
	if time.Since(h.LastSuccessAt) > window {
		return false // stale
	}
	if h.LastRecordedOutcomeWasAnError() {
		return false // last attempt failed
	}
	return true
}

// IsUnknown returns true if we've never tried this model.
func (h *ModelHealth) IsUnknown() bool {
	return h == nil || (h.LastSuccessAt.IsZero() && h.LastErrorAt.IsZero())
}

// migrateHealth adds the model_health table if it doesn't exist.
func (s *Store) migrateHealth() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS model_health (
			model TEXT PRIMARY KEY,
			last_success_at INTEGER DEFAULT 0,
			last_success_ms INTEGER DEFAULT 0,
			avg_response_ms INTEGER DEFAULT 0,
			last_error_at INTEGER DEFAULT 0,
			last_error TEXT DEFAULT '',
			consecutive_errors INTEGER DEFAULT 0
		);
	`)
	return err
}

// GetHealth returns the health record for a model.
func (s *Store) GetHealth(model string) *ModelHealth {
	s.migrateHealth()

	h := &ModelHealth{Model: model}
	var successAt, errorAt int64

	err := s.db.QueryRow(`
		SELECT last_success_at, last_success_ms, avg_response_ms,
		       last_error_at, last_error, consecutive_errors
		FROM model_health WHERE model = ?
	`, model).Scan(
		&successAt, &h.LastSuccessMs, &h.AvgResponseMs,
		&errorAt, &h.LastError, &h.ConsecutiveErr,
	)
	if err != nil {
		return h // empty/unknown
	}

	if successAt > 0 {
		h.LastSuccessAt = time.Unix(successAt, 0)
	}
	if errorAt > 0 {
		h.LastErrorAt = time.Unix(errorAt, 0)
	}
	return h
}

// RecordSuccess records a successful model response.
func (s *Store) RecordSuccess(model string, durationMs int64) {
	s.migrateHealth()

	// Get existing avg for rolling average
	existing := s.GetHealth(model)
	avg := durationMs
	if existing.AvgResponseMs > 0 {
		// Exponential moving average (weight 0.3 for new, 0.7 for history)
		avg = (3*durationMs + 7*existing.AvgResponseMs) / 10
	}

	now := time.Now().Unix()
	s.db.Exec(`
		INSERT INTO model_health (model, last_success_at, last_success_ms, avg_response_ms, last_error_at, last_error, consecutive_errors)
		VALUES (?, ?, ?, ?, 0, '', 0)
		ON CONFLICT(model) DO UPDATE SET
			last_success_at = ?,
			last_success_ms = ?,
			avg_response_ms = ?,
			last_error_at = 0,
			last_error = '',
			consecutive_errors = 0
	`, model, now, durationMs, avg, now, durationMs, avg)
}

// RecordError records a failed model response.
func (s *Store) RecordError(model string, errMsg string) {
	s.migrateHealth()

	now := time.Now().Unix()
	s.db.Exec(`
		INSERT INTO model_health (model, last_success_at, last_success_ms, avg_response_ms, last_error_at, last_error, consecutive_errors)
		VALUES (?, 0, 0, 0, ?, ?, 1)
		ON CONFLICT(model) DO UPDATE SET
			last_error_at = ?,
			last_error = ?,
			consecutive_errors = consecutive_errors + 1
	`, model, now, errMsg, now, errMsg)
}
