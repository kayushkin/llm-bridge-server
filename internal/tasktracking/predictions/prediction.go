package predictions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

const predictionColumns = `
	p.seq, p.claim, p.resolution_criteria, p.category, p.tags,
	p.provenance, p.author, p.probability, p.initial_probability, p.due_at,
	p.status, p.outcome, p.resolved_at, p.resolution_note,
	p.created_at, p.updated_at, p.deleted_at`

func scanPrediction(scan func(...any) error) (*Prediction, error) {
	var p Prediction
	var tags string
	err := scan(&p.Seq, &p.Claim, &p.ResolutionCriteria, &p.Category, &tags,
		&p.Provenance, &p.Author, &p.Probability, &p.InitialProbability, &p.DueAt,
		&p.Status, &p.Outcome, &p.ResolvedAt, &p.ResolutionNote,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if err != nil {
		return nil, err
	}
	p.ID = formatPredictionID(p.Seq)
	if p.Tags, err = unmarshalTags(tags); err != nil {
		return nil, fmt.Errorf("prediction %s: %w", p.ID, err)
	}
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
func (s *Store) Create(ctx context.Context, input *Prediction) (*Prediction, error) {
	if err := validateForWrite(input); err != nil {
		return nil, err
	}
	if err := validateDueDate(input.DueAt); err != nil {
		return nil, err
	}
	// Checked before the transaction, so a bad link refuses the whole create
	// without using up a prediction number.
	for _, link := range input.Links {
		if err := validateLink(link); err != nil {
			return nil, err
		}
	}
	tags, err := marshalTags(input.Tags)
	if err != nil {
		return nil, err
	}
	author := strings.TrimSpace(input.Author)
	var seq int64
	err = postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		ts := now()
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO predictions
				(claim, resolution_criteria, category, tags, provenance, author,
				 probability, initial_probability, due_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $7, $8, $9, $9)
			RETURNING seq`,
			input.Claim, input.ResolutionCriteria, input.Category, tags, input.Provenance, author,
			input.Probability, input.DueAt, ts).Scan(&seq); err != nil {
			return fmt.Errorf("inserting the prediction: %w", err)
		}
		// The opening odds are an estimate like any other, so the trail starts
		// full rather than starting at the first revision.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO estimates (prediction_seq, probability, rationale, provenance, author, created_at)
			VALUES ($1, $2, 'initial estimate', $3, $4, $5)`,
			seq, input.Probability, input.Provenance, author, ts); err != nil {
			return fmt.Errorf("inserting the opening estimate: %w", err)
		}
		for _, link := range input.Links {
			if err := insertLink(ctx, tx, seq, link, ts); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, formatPredictionID(seq))
}

// Get returns one prediction with its estimates and links. A soft-deleted one
// is still readable by id.
func (s *Store) Get(ctx context.Context, id string) (*Prediction, error) {
	seq, err := predictionNumberOf(id)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+predictionColumns+` FROM predictions p WHERE p.seq = $1`, seq)
	p, err := scanPrediction(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: prediction %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if p.Estimates, err = s.listEstimatesOf(ctx, seq); err != nil {
		return nil, err
	}
	if p.Links, err = s.listLinksOf(ctx, seq); err != nil {
		return nil, err
	}
	return p, nil
}

// queryArguments numbers the arguments of a query as they are added.
type queryArguments struct{ values []any }

func (arguments *queryArguments) add(value any) string {
	arguments.values = append(arguments.values, value)
	return fmt.Sprintf("$%d", len(arguments.values))
}

// buildQuery assembles the shared WHERE for List, Count and the scorers.
func (s *Store) buildQuery(ctx context.Context, f Filter, selectClause string, arguments *queryArguments) (string, error) {
	from := ` FROM predictions p`
	if f.EntityType != "" || f.EntityRef != "" {
		from += ` JOIN links l ON l.prediction_seq = p.seq`
	}
	where := ` WHERE true`
	if !f.IncludeDeleted {
		where += ` AND p.deleted_at = 0`
	}
	if f.Status != "" {
		where += ` AND p.status = ` + arguments.add(f.Status)
	}
	if f.Category != "" {
		category, ok := NormalizeCategory(f.Category)
		if !ok {
			return "", ErrUnknownCategory(f.Category)
		}
		where += ` AND p.category = ` + arguments.add(category)
	}
	if f.Author != "" {
		where += ` AND p.author = ` + arguments.add(f.Author)
	}
	if f.Provenance != "" {
		where += ` AND p.provenance = ` + arguments.add(f.Provenance)
	}
	if f.Outcome != "" {
		where += ` AND p.outcome = ` + arguments.add(f.Outcome)
	}
	if f.Tag != "" {
		// Whole tags only: "ops" must not match "ops-review".
		where += ` AND p.tags @> jsonb_build_array(` + arguments.add(f.Tag) + `::text)`
	}
	if f.EntityType != "" {
		where += ` AND l.entity_type = ` + arguments.add(f.EntityType)
	}
	if f.EntityRef != "" {
		where += ` AND l.entity_ref = ` + arguments.add(f.EntityRef)
	}
	if f.Overdue {
		where += ` AND p.status = 'open' AND p.due_at > 0 AND p.due_at <= ` + arguments.add(now())
	}
	if f.Since > 0 {
		where += ` AND p.created_at >= ` + arguments.add(f.Since)
	}
	if f.Until > 0 {
		where += ` AND p.created_at < ` + arguments.add(f.Until)
	}
	if f.Query != "" {
		searchQuery, err := s.validateSearchQuery(ctx, f.Query)
		if err != nil {
			return "", err
		}
		where += ` AND p.search_document @@ websearch_to_tsquery('simple', ` + arguments.add(searchQuery) + `)`
	}
	return selectClause + from + where, nil
}

// validateSearchQuery refuses a query that cannot mean anything, as the
// caller's 400 rather than an empty answer that reads as "nothing matched", and
// returns it rewritten for websearch_to_tsquery (searchQueryOf).
//
// q is read as a web search: words must all appear, "quoted words" must appear
// together, `or` between words lets either match, and -word must not appear.
// Postgres reads any text that way without an error, so the two mistakes the
// old FTS5 syntax refused are refused here: an unclosed quote, and a query
// with no word in it at all.
func (s *Store) validateSearchQuery(ctx context.Context, query string) (string, error) {
	searchQuery, err := searchQueryOf(query)
	if err != nil {
		return "", err
	}
	var terms int
	if err := s.db.QueryRowContext(ctx, `SELECT numnode(websearch_to_tsquery('simple', $1))`, searchQuery).Scan(&terms); err != nil {
		return "", fmt.Errorf("reading search query %q: %w", query, err)
	}
	if terms == 0 {
		return "", fmt.Errorf("%w: bad search query %q: it holds no word to search for", ErrInvalidPrediction, query)
	}
	return searchQuery, nil
}

// List returns predictions newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]*Prediction, error) {
	arguments := &queryArguments{}
	query, err := s.buildQuery(ctx, f, `SELECT DISTINCT `+predictionColumns, arguments)
	if err != nil {
		return nil, err
	}
	query += ` ORDER BY p.created_at DESC, p.seq DESC`
	if f.Limit > 0 {
		query += ` LIMIT ` + arguments.add(f.Limit)
	}
	if f.Offset > 0 {
		query += ` OFFSET ` + arguments.add(f.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, arguments.values...)
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
			if p.Estimates, err = s.listEstimatesOf(ctx, p.Seq); err != nil {
				return nil, err
			}
			if p.Links, err = s.listLinksOf(ctx, p.Seq); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Count is List's total, ignoring limit and offset.
func (s *Store) Count(ctx context.Context, f Filter) (int, error) {
	arguments := &queryArguments{}
	query, err := s.buildQuery(ctx, f, `SELECT COUNT(DISTINCT p.seq)`, arguments)
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query, arguments.values...).Scan(&n); err != nil {
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
func (s *Store) Patch(ctx context.Context, id string, patch Patch) (*Prediction, error) {
	current, err := s.Get(ctx, id)
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
	tags, err := marshalTags(current.Tags)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE predictions SET claim = $1, resolution_criteria = $2, category = $3, tags = $4::jsonb,
			provenance = $5, author = $6, due_at = $7, resolution_note = $8, updated_at = $9
		WHERE seq = $10`,
		current.Claim, current.ResolutionCriteria, current.Category, tags,
		current.Provenance, current.Author, current.DueAt, current.ResolutionNote, now(), current.Seq); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// SoftDelete hides a prediction from every read path without destroying it.
func (s *Store) SoftDelete(ctx context.Context, id string) error {
	seq, err := predictionNumberOf(id)
	if err != nil {
		return err
	}
	ts := now()
	result, err := s.db.ExecContext(ctx, `UPDATE predictions SET deleted_at = $1, updated_at = $1 WHERE seq = $2 AND deleted_at = 0`, ts, seq)
	if err != nil {
		return err
	}
	return requireOneRowChanged(result, fmt.Sprintf("prediction %s", id))
}

// Restore brings a soft-deleted prediction back exactly as it was.
func (s *Store) Restore(ctx context.Context, id string) (*Prediction, error) {
	seq, err := predictionNumberOf(id)
	if err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE predictions SET deleted_at = 0, updated_at = $1 WHERE seq = $2`, now(), seq)
	if err != nil {
		return nil, err
	}
	if err := requireOneRowChanged(result, fmt.Sprintf("prediction %s", id)); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Purge destroys the row and, by cascade, its estimates and links.
func (s *Store) Purge(ctx context.Context, id string) error {
	seq, err := predictionNumberOf(id)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM predictions WHERE seq = $1`, seq)
	if err != nil {
		return err
	}
	return requireOneRowChanged(result, fmt.Sprintf("prediction %s", id))
}

// requireOneRowChanged turns a statement that matched nothing into ErrNotFound.
func requireOneRowChanged(result sql.Result, what string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting the rows changed for %s: %w", what, err)
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}

// AddEstimate appends a revised probability. This is the Bayesian write path:
// the old number is never overwritten, so the record shows what moved and when.
func (s *Store) AddEstimate(ctx context.Context, id string, e *Estimate) (*Prediction, error) {
	seq, err := predictionNumberOf(id)
	if err != nil {
		return nil, err
	}
	err = postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		// The row is locked first, so a resolve that lands at the same moment
		// either happens before this check or waits for this estimate.
		var status, currentProvenance, currentAuthor string
		err := tx.QueryRowContext(ctx, `SELECT status, provenance, author FROM predictions WHERE seq = $1 FOR UPDATE`, seq).
			Scan(&status, &currentProvenance, &currentAuthor)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: prediction %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if status != "open" {
			return fmt.Errorf("%w: prediction %s is %s — revising the odds after the answer is known is not an update, it is a rewrite",
				ErrAlreadyResolved, id, status)
		}
		snapped, err := SnapToLadder(e.Probability)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEstimate, err)
		}
		provenance := strings.ToLower(strings.TrimSpace(e.Provenance))
		if provenance == "" {
			provenance = currentProvenance
		}
		if !ValidProvenance(provenance) {
			return fmt.Errorf("%w: unknown provenance %q: use one of %s",
				ErrInvalidEstimate, e.Provenance, strings.Join(Provenances, ", "))
		}
		author := strings.TrimSpace(e.Author)
		if author == "" {
			author = currentAuthor
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO estimates (prediction_seq, probability, rationale, provenance, author, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			seq, snapped, strings.TrimSpace(e.Rationale), provenance, author, ts); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE predictions SET probability = $1, provenance = $2, updated_at = $3 WHERE seq = $4`,
			snapped, provenance, ts, seq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ListEstimates returns the full trail, oldest first.
// An id that names no prediction has no estimates, malformed or not, as when
// the id was a stored string.
func (s *Store) ListEstimates(ctx context.Context, id string) ([]*Estimate, error) {
	seq, err := predictionNumberOf(id)
	if errors.Is(err, ErrNotFound) {
		return []*Estimate{}, nil
	}
	return s.listEstimatesOf(ctx, seq)
}

func (s *Store) listEstimatesOf(ctx context.Context, seq int64) ([]*Estimate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, prediction_seq, probability, rationale, provenance, author, created_at
		FROM estimates WHERE prediction_seq = $1 ORDER BY created_at ASC, id ASC`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Estimate{}
	for rows.Next() {
		var e Estimate
		var predictionSeq int64
		if err := rows.Scan(&e.ID, &predictionSeq, &e.Probability, &e.Rationale,
			&e.Provenance, &e.Author, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.PredictionID = formatPredictionID(predictionSeq)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// Resolve records what actually happened. Outcome is "true", "false", or
// "void" for a claim that stopped being resolvable; void is excluded from every
// score rather than counted as a miss.
func (s *Store) Resolve(ctx context.Context, id, outcome, note string) (*Prediction, error) {
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if !ValidOutcome(outcome) {
		return nil, fmt.Errorf("%w: unknown outcome %q: use one of %s",
			ErrInvalidPrediction, outcome, strings.Join(Outcomes, ", "))
	}
	seq, err := predictionNumberOf(id)
	if err != nil {
		return nil, err
	}
	status, stored := "resolved", outcome
	if outcome == "void" {
		status, stored = "void", ""
	}
	err = postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		// Locked, so two resolves at once cannot both find the row open.
		var currentStatus, currentOutcome string
		var resolvedAt int64
		err := tx.QueryRowContext(ctx, `SELECT status, outcome, resolved_at FROM predictions WHERE seq = $1 FOR UPDATE`, seq).
			Scan(&currentStatus, &currentOutcome, &resolvedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: prediction %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if currentStatus != "open" {
			return fmt.Errorf("%w: prediction %s already resolved %q at %d",
				ErrAlreadyResolved, id, currentOutcome, resolvedAt)
		}
		ts := now()
		_, err = tx.ExecContext(ctx, `
			UPDATE predictions SET status = $1, outcome = $2, resolved_at = $3, resolution_note = $4, updated_at = $3
			WHERE seq = $5`, status, stored, ts, strings.TrimSpace(note), seq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ListTags counts the tags on live predictions, most used first, then in
// byte order of the tag, whatever the database's collation.
func (s *Store) ListTags(ctx context.Context) ([]TagCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tag, COUNT(*) AS n
		FROM predictions p, jsonb_array_elements_text(p.tags) AS tag
		WHERE p.deleted_at = 0
		GROUP BY tag ORDER BY n DESC, tag COLLATE "C" ASC`)
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
