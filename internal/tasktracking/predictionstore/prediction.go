package predictionstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const predictionColumns = `
	p.id, p.seq, p.claim, p.resolution_criteria, p.category, p.tags,
	p.provenance, p.author, p.probability, p.initial_probability, p.due_at,
	p.status, p.outcome, p.resolved_at, p.resolution_note,
	p.created_at, p.updated_at, p.deleted_at`

func scanPrediction(scan func(...any) error) (*Prediction, error) {
	var p Prediction
	var tags string
	err := scan(&p.ID, &p.Seq, &p.Claim, &p.ResolutionCriteria, &p.Category, &tags,
		&p.Provenance, &p.Author, &p.Probability, &p.InitialProbability, &p.DueAt,
		&p.Status, &p.Outcome, &p.ResolvedAt, &p.ResolutionNote,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if err != nil {
		return nil, err
	}
	p.Tags = unmarshalTags(tags)
	if brier, ok := brierOf(&p); ok {
		p.Brier = &brier
	}
	return &p, nil
}

// validateForWrite checks the fields every prediction must carry, whatever the
// write path. Refusing here rather than storing a blank is the whole point:
// a claim with no resolution criteria can never be scored, so it is not a
// prediction, it is a mood.
func validateForWrite(p *Prediction) error {
	p.Claim = strings.TrimSpace(p.Claim)
	if p.Claim == "" {
		return fmt.Errorf("%w: claim is required", ErrInvalidPrediction)
	}
	p.ResolutionCriteria = strings.TrimSpace(p.ResolutionCriteria)
	if p.ResolutionCriteria == "" {
		return fmt.Errorf(
			"%w: resolution_criteria is required — say 85%% OF WHAT. A claim that cannot resolve cannot be scored, and an unresolvable row is worse than no row",
			ErrInvalidPrediction)
	}
	category, ok := NormalizeCategory(p.Category)
	if !ok {
		return ErrUnknownCategory(p.Category)
	}
	p.Category = category
	p.Provenance = strings.ToLower(strings.TrimSpace(p.Provenance))
	if !ValidProvenance(p.Provenance) {
		return fmt.Errorf("%w: unknown provenance %q: use one of %s — where the belief came from is a different fact from how strong it is, and dropping it makes a guess read as a measurement",
			ErrInvalidPrediction, p.Provenance, strings.Join(Provenances, ", "))
	}
	snapped, err := SnapToLadder(p.Probability)
	if err != nil {
		return err
	}
	p.Probability = snapped
	return nil
}

// Create records a new prediction and its first estimate in one transaction.
func (s *Store) Create(input *Prediction) (*Prediction, error) {
	if err := validateForWrite(input); err != nil {
		return nil, err
	}
	if err := validateDueDate(input.DueAt); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM predictions`).Scan(&maxSeq); err != nil {
		return nil, err
	}
	seq := maxSeq.Int64 + 1
	id := formatID(seq)
	ts := now()

	_, err = tx.Exec(`
		INSERT INTO predictions
			(id, seq, claim, resolution_criteria, category, tags, provenance, author,
			 probability, initial_probability, due_at, status, outcome, resolved_at,
			 resolution_note, created_at, updated_at, deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,'open','',0,'',?,?,0)`,
		id, seq, input.Claim, input.ResolutionCriteria, input.Category,
		marshalTags(input.Tags), input.Provenance, strings.TrimSpace(input.Author),
		input.Probability, input.Probability, input.DueAt, ts, ts)
	if err != nil {
		return nil, err
	}

	// The opening odds are an estimate like any other, so the trail starts full
	// rather than starting at the first revision.
	if _, err := tx.Exec(`
		INSERT INTO estimates (prediction_id, probability, rationale, provenance, author, created_at)
		VALUES (?,?,?,?,?,?)`,
		id, input.Probability, "initial estimate", input.Provenance,
		strings.TrimSpace(input.Author), ts); err != nil {
		return nil, err
	}

	for _, l := range input.Links {
		if err := insertLink(tx, id, l, ts); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get returns one prediction with its estimates and links.
func (s *Store) Get(id string) (*Prediction, error) {
	row := s.db.QueryRow(`SELECT `+predictionColumns+` FROM predictions p WHERE p.id = ?`, id)
	p, err := scanPrediction(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: prediction %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if p.Estimates, err = s.ListEstimates(id); err != nil {
		return nil, err
	}
	if p.Links, err = s.ListLinks(id); err != nil {
		return nil, err
	}
	return p, nil
}

// buildQuery assembles the shared WHERE for List, Count and the scorers.
func (s *Store) buildQuery(f Filter, selectClause string) (string, []any, error) {
	var args []any
	from := ` FROM predictions p`
	if f.EntityType != "" || f.EntityRef != "" {
		from += ` JOIN links l ON l.prediction_id = p.id`
	}
	where := ` WHERE 1=1`
	if !f.IncludeDeleted {
		where += ` AND p.deleted_at = 0`
	}
	if f.Status != "" {
		where += ` AND p.status = ?`
		args = append(args, f.Status)
	}
	if f.Category != "" {
		category, ok := NormalizeCategory(f.Category)
		if !ok {
			return "", nil, ErrUnknownCategory(f.Category)
		}
		where += ` AND p.category = ?`
		args = append(args, category)
	}
	if f.Author != "" {
		where += ` AND p.author = ?`
		args = append(args, f.Author)
	}
	if f.Provenance != "" {
		where += ` AND p.provenance = ?`
		args = append(args, f.Provenance)
	}
	if f.Outcome != "" {
		where += ` AND p.outcome = ?`
		args = append(args, f.Outcome)
	}
	if f.Tag != "" {
		// Match whole tags only: a LIKE on the raw JSON would let "ops" match
		// "ops-review".
		where += ` AND EXISTS (SELECT 1 FROM json_each(p.tags) WHERE json_each.value = ?)`
		args = append(args, f.Tag)
	}
	if f.EntityType != "" {
		where += ` AND l.entity_type = ?`
		args = append(args, f.EntityType)
	}
	if f.EntityRef != "" {
		where += ` AND l.entity_ref = ?`
		args = append(args, f.EntityRef)
	}
	if f.Overdue {
		where += ` AND p.status = 'open' AND p.due_at > 0 AND p.due_at <= ?`
		args = append(args, now())
	}
	if f.Since > 0 {
		where += ` AND p.created_at >= ?`
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where += ` AND p.created_at < ?`
		args = append(args, f.Until)
	}
	if f.Query != "" {
		if err := s.validateSearchQuery(f.Query); err != nil {
			return "", nil, err
		}
		where += ` AND p.seq IN (SELECT rowid FROM predictions_fts WHERE predictions_fts MATCH ?)`
		args = append(args, f.Query)
	}
	return selectClause + from + where, args, nil
}

// validateSearchQuery probes the FTS index so a malformed query is the caller's
// 400 rather than a 500 from deep inside the list handler.
func (s *Store) validateSearchQuery(query string) error {
	var discard int
	err := s.db.QueryRow(
		`SELECT 1 FROM predictions_fts WHERE predictions_fts MATCH ? LIMIT 1`, query).Scan(&discard)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("%w: bad search query %q: %v", ErrInvalidPrediction, query, err)
}

// List returns predictions newest first.
func (s *Store) List(f Filter) ([]*Prediction, error) {
	query, args, err := s.buildQuery(f, `SELECT DISTINCT `+predictionColumns)
	if err != nil {
		return nil, err
	}
	query += ` ORDER BY p.created_at DESC, p.seq DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	if f.Offset > 0 {
		if f.Limit <= 0 {
			query += ` LIMIT -1`
		}
		query += fmt.Sprintf(` OFFSET %d`, f.Offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Prediction{}
	for rows.Next() {
		p, err := scanPrediction(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if f.Expand {
		for _, p := range out {
			if p.Estimates, err = s.ListEstimates(p.ID); err != nil {
				return nil, err
			}
			if p.Links, err = s.ListLinks(p.ID); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Count is List's total, ignoring limit and offset.
func (s *Store) Count(f Filter) (int, error) {
	query, args, err := s.buildQuery(f, `SELECT COUNT(DISTINCT p.id)`)
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Patch edits the descriptive fields. It refuses probability and outcome by
// having nowhere to put them: the number moves through AddEstimate so the trail
// survives, and the outcome through Resolve so it happens once.
//
// resolution_note is the exception, and only on a row that has already
// resolved: the note explains an outcome it cannot change, so correcting it
// costs the ledger nothing. On an open row it is refused.
func (s *Store) Patch(id string, patch Patch) (*Prediction, error) {
	current, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if patch.Claim != nil {
		current.Claim = *patch.Claim
	}
	if patch.ResolutionCriteria != nil {
		current.ResolutionCriteria = *patch.ResolutionCriteria
	}
	if patch.Category != nil {
		current.Category = *patch.Category
	}
	if patch.Provenance != nil {
		current.Provenance = *patch.Provenance
	}
	if patch.Tags != nil {
		current.Tags = *patch.Tags
	}
	if patch.Author != nil {
		current.Author = strings.TrimSpace(*patch.Author)
	}
	if patch.DueAt != nil && *patch.DueAt != current.DueAt {
		// Checked only when the value actually moves. A row goes overdue by the
		// clock advancing, not by anyone writing to it, and a read-modify-write
		// caller resending the deadline it just read must still be able to fix a
		// typo in the claim. What is refused is a NEW deadline that is already
		// past — which is the only way the mistake gets in.
		if err := validateDueDate(*patch.DueAt); err != nil {
			return nil, err
		}
		current.DueAt = *patch.DueAt
	}
	if patch.ResolutionNote != nil {
		// An open row has no resolution, so it can have no note about one. Writing
		// one anyway would let a caller state the outcome in prose while the row
		// still scores nothing, and the next reader would believe the row was
		// settled. Refuse, and name the route that does settle it.
		if current.Status == "open" {
			return nil, fmt.Errorf("%w: prediction %s is still open, so it has no resolution to annotate: "+
				"record the outcome and the note together with POST /predictions/{id}/resolve", ErrInvalidPrediction, id)
		}
		current.ResolutionNote = strings.TrimSpace(*patch.ResolutionNote)
	}
	if err := validateForWrite(current); err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`
		UPDATE predictions SET claim=?, resolution_criteria=?, category=?, tags=?,
			provenance=?, author=?, due_at=?, resolution_note=?, updated_at=?
		WHERE id=?`,
		current.Claim, current.ResolutionCriteria, current.Category, marshalTags(current.Tags),
		current.Provenance, current.Author, current.DueAt, current.ResolutionNote, now(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// SoftDelete hides a prediction from every read path without destroying it.
func (s *Store) SoftDelete(id string) error {
	res, err := s.db.Exec(`UPDATE predictions SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at=0`, now(), now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: prediction %s", ErrNotFound, id)
	}
	return nil
}

// Restore brings a soft-deleted prediction back exactly as it was.
func (s *Store) Restore(id string) (*Prediction, error) {
	res, err := s.db.Exec(`UPDATE predictions SET deleted_at=0, updated_at=? WHERE id=?`, now(), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: prediction %s", ErrNotFound, id)
	}
	return s.Get(id)
}

// Purge destroys the row and, by cascade, its estimates and links.
func (s *Store) Purge(id string) error {
	res, err := s.db.Exec(`DELETE FROM predictions WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: prediction %s", ErrNotFound, id)
	}
	return nil
}

// AddEstimate appends a revised probability. This is the Bayesian write path:
// the old number is never overwritten, so the record shows what moved and when.
func (s *Store) AddEstimate(id string, e *Estimate) (*Prediction, error) {
	current, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if current.Status != "open" {
		return nil, fmt.Errorf("%w: prediction %s is %s — revising the odds after the answer is known is not an update, it is a rewrite",
			ErrAlreadyResolved, id, current.Status)
	}
	snapped, err := SnapToLadder(e.Probability)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEstimate, err)
	}
	provenance := strings.ToLower(strings.TrimSpace(e.Provenance))
	if provenance == "" {
		provenance = current.Provenance
	}
	if !ValidProvenance(provenance) {
		return nil, fmt.Errorf("%w: unknown provenance %q: use one of %s",
			ErrInvalidEstimate, e.Provenance, strings.Join(Provenances, ", "))
	}
	author := strings.TrimSpace(e.Author)
	if author == "" {
		author = current.Author
	}
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO estimates (prediction_id, probability, rationale, provenance, author, created_at)
		VALUES (?,?,?,?,?,?)`,
		id, snapped, strings.TrimSpace(e.Rationale), provenance, author, ts); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE predictions SET probability=?, provenance=?, updated_at=? WHERE id=?`,
		snapped, provenance, ts, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// ListEstimates returns the full trail, oldest first.
func (s *Store) ListEstimates(id string) ([]*Estimate, error) {
	rows, err := s.db.Query(`
		SELECT id, prediction_id, probability, rationale, provenance, author, created_at
		FROM estimates WHERE prediction_id=? ORDER BY created_at ASC, id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Estimate{}
	for rows.Next() {
		var e Estimate
		if err := rows.Scan(&e.ID, &e.PredictionID, &e.Probability, &e.Rationale,
			&e.Provenance, &e.Author, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// Resolve records what actually happened. Outcome is "true", "false", or
// "void" for a claim that stopped being resolvable; void is excluded from every
// score rather than counted as a miss.
func (s *Store) Resolve(id, outcome, note string) (*Prediction, error) {
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if !ValidOutcome(outcome) {
		return nil, fmt.Errorf("%w: unknown outcome %q: use one of %s",
			ErrInvalidPrediction, outcome, strings.Join(Outcomes, ", "))
	}
	current, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if current.Status != "open" {
		return nil, fmt.Errorf("%w: prediction %s already resolved %q at %d",
			ErrAlreadyResolved, id, current.Outcome, current.ResolvedAt)
	}
	status, stored := "resolved", outcome
	if outcome == "void" {
		status, stored = "void", ""
	}
	ts := now()
	if _, err := s.db.Exec(`
		UPDATE predictions SET status=?, outcome=?, resolved_at=?, resolution_note=?, updated_at=?
		WHERE id=?`, status, stored, ts, strings.TrimSpace(note), ts, id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// ListTags counts the tags on live predictions.
func (s *Store) ListTags() ([]TagCount, error) {
	rows, err := s.db.Query(`
		SELECT json_each.value AS tag, COUNT(*) AS n
		FROM predictions p, json_each(p.tags)
		WHERE p.deleted_at = 0
		GROUP BY tag ORDER BY n DESC, tag ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// validateDueDate refuses a deadline that has already passed at the moment it
// is written.
//
// The store already refuses a claim with no resolution criteria and a
// probability off the ladder, for the same reason: a row that cannot be scored
// is worse than no row. A due date in the past is that failure wearing a
// timestamp. Nothing can be predicted about a moment that has gone, so such a
// row is not a prediction with a deadline, it is a typo — and the typo is
// silent, because every read path treats the row as merely late.
//
// Measured on the live store 2026-09-04: three of twenty-nine rows carried a
// due date EARLIER than their own created_at, all three off by exactly one year
// (2025 for 2026) from one authoring hand. The overdue sweep dutifully reported
// two of them as "336 days overdue" and "307 days overdue" and wrote them into
// a noteboard todo telling the reader to resolve claims whose evidence window
// had not opened yet.
//
// A due date is optional — leave it at 0 for a claim with no deadline. So
// refusing a past one costs no legitimate caller anything.
func validateDueDate(dueAt int64) error {
	if dueAt <= 0 {
		return nil
	}
	if current := now(); dueAt <= current {
		return fmt.Errorf(
			"%w: due_at %d (%s) has already passed — a deadline in the past cannot be predicted against, and it reads downstream as merely overdue rather than as the mistake it is. Use a future date, or omit due_at for a claim with no deadline",
			ErrInvalidPrediction, dueAt, time.Unix(dueAt, 0).UTC().Format(time.RFC3339))
	}
	return nil
}
