package peoplestore

import (
	"fmt"
	"strings"
)

// The review queue is the handles the operator talks with — a handle_activity
// row with messages or a one-to-one chat — that no one has reviewed and no
// one claimed in the last ReviewClaimSeconds. The contact review agent claims
// a batch, links or suggests what it can, and marks the batch reviewed. The
// queue lives here, beside the suggestions the agent writes, so that the
// claim that keeps two sessions apart and the suggestions they write are in
// one store.

// pendingReviewQuery selects the queue, people with a one-to-one chat first,
// then the most recently active, then the most messages.
const pendingReviewQuery = `SELECT a.kind, a.value, COALESCE(i.person_id, ''), a.last_activity_at, a.message_count, a.direct_chat
	FROM handle_activity a
	LEFT JOIN identities i ON i.kind = a.kind AND i.value = a.value
	WHERE (a.message_count > 0 OR a.direct_chat = 1)
	AND NOT EXISTS (SELECT 1 FROM handle_reviews r WHERE r.kind = a.kind AND r.value = a.value)
	AND NOT EXISTS (SELECT 1 FROM handle_review_claims c WHERE c.kind = a.kind AND c.value = a.value AND c.claimed_at > ?)
	ORDER BY a.direct_chat DESC, a.last_activity_at DESC, a.message_count DESC, a.kind, a.value`

func pendingReviews(q queryer) ([]PendingReview, int, error) {
	cutoff := now() - ReviewClaimSeconds
	rows, err := q.Query(pendingReviewQuery, cutoff)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	pending := []PendingReview{}
	for rows.Next() {
		var review PendingReview
		if err := rows.Scan(&review.Kind, &review.Value, &review.PersonID, &review.LastActivityAt, &review.MessageCount, &review.DirectChat); err != nil {
			return nil, 0, err
		}
		pending = append(pending, review)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var activeClaims int
	if err := q.QueryRow(`SELECT COUNT(*) FROM handle_review_claims WHERE claimed_at > ?`, cutoff).Scan(&activeClaims); err != nil {
		return nil, 0, err
	}
	return pending, activeClaims, nil
}

// PendingReviews answers GET /reviews/pending: the first limit (default 25,
// at most 1000) of the queue, its size, and how many handles are claimed now.
func (s *Store) PendingReviews(limit int) (*ReviewQueue, error) {
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be 1 to 1000", ErrInvalid)
	}
	pending, activeClaims, err := pendingReviews(s.db)
	if err != nil {
		return nil, err
	}
	return &ReviewQueue{Total: len(pending), ActiveClaims: activeClaims, Pending: pending[:min(limit, len(pending))]}, nil
}

// ClaimReviews hands a reviewer the next limit (1 to 100) handles of the queue
// and claims them, so they leave the queue for ReviewClaimSeconds and a second
// session never gets them.
func (s *Store) ClaimReviews(input ReviewClaimInput) (*ReviewClaim, error) {
	if input.Limit < 1 || input.Limit > 100 {
		return nil, fmt.Errorf("%w: limit must be 1 to 100", ErrInvalid)
	}
	source, err := linkSourceFor(strings.TrimSpace(input.ClaimedBy), "claimed_by")
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pending, _, err := pendingReviews(tx)
	if err != nil {
		return nil, err
	}
	batch := pending[:min(input.Limit, len(pending))]
	timestamp := now()
	for _, review := range batch {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO handle_review_claims (kind, value, claimed_by, claimed_at) VALUES (?,?,?,?)`,
			review.Kind, review.Value, source.Source, timestamp); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ReviewClaim{Claimed: batch, Remaining: len(pending) - len(batch)}, nil
}

// MarkReviewed records that the handles were reviewed and ends their claims.
// A handle reviewed again keeps the newer review.
func (s *Store) MarkReviewed(input ReviewInput) (*ReviewResult, error) {
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
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, handle := range handles {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO handle_reviews (kind, value, reviewed_by, reviewed_at) VALUES (?,?,?,?)`,
			handle.Kind, handle.Value, source.Source, reviewedAt); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM handle_review_claims WHERE kind = ? AND value = ?`, handle.Kind, handle.Value); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ReviewResult{Reviewed: len(handles)}, nil
}
