package peoplestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// The review queue is, for each owner, the handles the owner talks with — a
// handle_activity row with messages or a one-to-one chat — that no one has
// reviewed and no one claimed in the last ReviewClaimSeconds. The contact
// review agent claims a batch, links or suggests what it can, and marks the
// batch reviewed. The queue lives here, beside the suggestions the agent
// writes, so that the claim that keeps two sessions apart and the suggestions
// they write are in one store.

// pendingReviews reads the queue within scope, people with a one-to-one chat
// first, then the most recently active, then the most messages. Handles sort
// by their bytes, as SQLite's binary collation sorted them.
func pendingReviews(ctx context.Context, q queryer, scope ownership.Scope) ([]PendingReview, int, error) {
	cutoff := now() - ReviewClaimSeconds
	var args arguments
	cutoffPlaceholder := args.add(cutoff)
	rows, err := q.QueryContext(ctx, `SELECT a.owner_principal_id, a.kind, a.value, i.person_seq, a.last_activity_at, a.message_count, a.direct_chat
		FROM handle_activity a
		LEFT JOIN identities i ON i.owner_principal_id = a.owner_principal_id AND i.kind = a.kind AND i.value = a.value
		WHERE (a.message_count > 0 OR a.direct_chat) AND `+args.within(scope, "a.owner_principal_id")+`
		AND NOT EXISTS (SELECT 1 FROM handle_reviews r WHERE r.owner_principal_id = a.owner_principal_id AND r.kind = a.kind AND r.value = a.value)
		AND NOT EXISTS (SELECT 1 FROM handle_review_claims c WHERE c.owner_principal_id = a.owner_principal_id AND c.kind = a.kind AND c.value = a.value AND c.claimed_at > `+cutoffPlaceholder+`)
		ORDER BY a.direct_chat DESC, a.last_activity_at DESC, a.message_count DESC, a.kind COLLATE "C", a.value COLLATE "C", a.owner_principal_id`, args.values...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	pending := []PendingReview{}
	for rows.Next() {
		var review PendingReview
		var personSeq sql.NullInt64
		if err := rows.Scan(&review.OwnerPrincipalID, &review.Kind, &review.Value, &personSeq, &review.LastActivityAt, &review.MessageCount, &review.DirectChat); err != nil {
			return nil, 0, err
		}
		review.PersonID = formatOptionalPersonID(personSeq)
		pending = append(pending, review)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var countArgs arguments
	countQuery := `SELECT COUNT(*) FROM handle_review_claims WHERE claimed_at > ` + countArgs.add(cutoff) + ` AND ` + countArgs.within(scope, "owner_principal_id")
	var activeClaims int
	if err := q.QueryRowContext(ctx, countQuery, countArgs.values...).Scan(&activeClaims); err != nil {
		return nil, 0, err
	}
	return pending, activeClaims, nil
}

// PendingReviews answers GET /reviews/pending: the first limit (default 25,
// at most 1000) of the queue within scope, its size, and how many handles are
// claimed now.
func (s *Store) PendingReviews(ctx context.Context, scope ownership.Scope, limit int) (*ReviewQueue, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be 1 to 1000", ErrInvalid)
	}
	pending, activeClaims, err := pendingReviews(ctx, s.db, scope)
	if err != nil {
		return nil, err
	}
	return &ReviewQueue{Total: len(pending), ActiveClaims: activeClaims, Pending: pending[:min(limit, len(pending))]}, nil
}

// ClaimReviews hands a reviewer the next limit (1 to 100) handles of owner's
// queue and claims them, so they leave the queue for ReviewClaimSeconds and a
// second session never gets them.
func (s *Store) ClaimReviews(ctx context.Context, owner string, input ReviewClaimInput) (*ReviewClaim, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	if input.Limit < 1 || input.Limit > 100 {
		return nil, fmt.Errorf("%w: limit must be 1 to 100", ErrInvalid)
	}
	source, err := linkSourceFor(strings.TrimSpace(input.ClaimedBy), "claimed_by")
	if err != nil {
		return nil, err
	}
	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pending, _, err := pendingReviews(ctx, tx, ownership.OwnedBy(owner))
	if err != nil {
		return nil, err
	}
	batch := pending[:min(input.Limit, len(pending))]
	timestamp := now()
	for _, review := range batch {
		if _, err := tx.ExecContext(ctx, `INSERT INTO handle_review_claims (owner_principal_id, kind, value, claimed_by, claimed_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (owner_principal_id, kind, value) DO UPDATE SET claimed_by = excluded.claimed_by, claimed_at = excluded.claimed_at`,
			owner, review.Kind, review.Value, source.Source, timestamp); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ReviewClaim{Claimed: batch, Remaining: len(pending) - len(batch)}, nil
}

// MarkReviewed records that owner's handles were reviewed and ends their
// claims. A handle reviewed again keeps the newer review.
func (s *Store) MarkReviewed(ctx context.Context, owner string, input ReviewInput) (*ReviewResult, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	source, err := linkSourceFor(strings.TrimSpace(input.ReviewedBy), "reviewed_by")
	if err != nil {
		return nil, err
	}
	if len(input.Handles) == 0 {
		return nil, fmt.Errorf("%w: handles is required", ErrInvalid)
	}
	timestamp := now()
	reviewedAt := input.ReviewedAt
	if reviewedAt < 0 || reviewedAt > timestamp+60 {
		return nil, fmt.Errorf("%w: reviewed_at %d is negative or in the future", ErrInvalid, reviewedAt)
	}
	if reviewedAt == 0 {
		reviewedAt = timestamp
	}
	handles := make([]IdentityInput, 0, len(input.Handles))
	for index, raw := range input.Handles {
		handle, err := NormalizeIdentity(IdentityInput{Kind: raw.Kind, Value: raw.Value})
		if err != nil {
			return nil, fmt.Errorf("handles[%d]: %w", index, err)
		}
		handles = append(handles, handle)
	}
	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, handle := range handles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO handle_reviews (owner_principal_id, kind, value, reviewed_by, reviewed_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (owner_principal_id, kind, value) DO UPDATE SET reviewed_by = excluded.reviewed_by, reviewed_at = excluded.reviewed_at`,
			owner, handle.Kind, handle.Value, source.Source, reviewedAt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM handle_review_claims WHERE owner_principal_id = $1 AND kind = $2 AND value = $3`, owner, handle.Kind, handle.Value); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ReviewResult{Reviewed: len(handles)}, nil
}
