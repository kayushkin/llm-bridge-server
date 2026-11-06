package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// suggestionStatuses: pending waits for the operator; accepted took the
// model's category; edited took the operator's instead; rejected took
// nothing; superseded was replaced by a newer suggestion; stale found the
// transaction already sorted by someone else when it was decided.
var suggestionStatuses = []string{"pending", "accepted", "edited", "rejected", "superseded", "stale"}

// CategorySuggestion is a model's category for one transaction, with what
// the operator needs to judge it.
type CategorySuggestion struct {
	ID                string  `json:"id"`
	TransactionID     string  `json:"transaction_id"`
	CategoryID        string  `json:"category_id"` // empty: the model said a person must decide
	CategoryPath      string  `json:"category_path"`
	Confidence        float64 `json:"confidence"`
	Rationale         string  `json:"rationale"`
	NeedsReview       bool    `json:"needs_review"`
	Model             string  `json:"model"`
	OperationID       string  `json:"operation_id"`
	Status            string  `json:"status"`
	DecidedCategoryID string  `json:"decided_category_id"`
	DecidedAt         int64   `json:"decided_at"`
	CreatedAt         int64   `json:"created_at"`
	// The transaction, for reading without a second call.
	OccurredOn  string `json:"occurred_on"`
	Description string `json:"description"`
	AmountCents int64  `json:"amount_cents"` // the category side: spending positive, money in negative
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
}

// SuggestionCandidate is a transaction still wholly Uncategorized with no
// pending suggestion: what a categorizer should look at next.
type SuggestionCandidate struct {
	TransactionID string `json:"transaction_id"`
	OccurredOn    string `json:"occurred_on"`
	Description   string `json:"description"`
	AmountCents   int64  `json:"amount_cents"` // the category side: spending positive, money in negative
	AccountName   string `json:"account_name"`
	AccountKind   string `json:"account_kind"`
	PayeeName     string `json:"payee_name"`
}

// CategoryPaths names every category by its path, "Food > Groceries", for
// showing to a person or a model. Keyed by id.
func (s *Store) CategoryPaths() (map[string]string, error) {
	categories, err := s.ListCategories()
	if err != nil {
		return nil, err
	}
	byID := map[string]Category{}
	for _, c := range categories {
		byID[c.ID] = c
	}
	paths := map[string]string{}
	for _, c := range categories {
		parts := []string{c.Name}
		for parent := c.ParentID; parent != ""; parent = byID[parent].ParentID {
			parts = append([]string{byID[parent].Name}, parts...)
		}
		paths[c.ID] = strings.Join(parts, " > ")
	}
	return paths, nil
}

// wholeUncategorizedPosting is the Uncategorized posting of a transaction
// whose only category posting it is, or "" when the transaction is split or
// already sorted.
func wholeUncategorizedPosting(q queryer, transactionID, uncategorized string) (string, error) {
	rows, err := q.Query(`SELECT id, category_id FROM postings WHERE transaction_id = ? AND category_id IS NOT NULL`, transactionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var postingID, categoryID string
	count := 0
	for rows.Next() {
		if err := rows.Scan(&postingID, &categoryID); err != nil {
			return "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count != 1 || categoryID != uncategorized {
		return "", nil
	}
	return postingID, nil
}

// ListSuggestionCandidates returns up to limit transactions, newest first,
// that are wholly Uncategorized and have no pending suggestion.
func (s *Store) ListSuggestionCandidates(limit int) ([]SuggestionCandidate, error) {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	uncategorized, err := categoryIDForRole(s.db, "uncategorized")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT t.id, t.occurred_on, t.description, pc.amount_cents, a.name, a.kind, COALESCE(py.name,'')
		FROM transactions t
		JOIN postings pc ON pc.transaction_id = t.id AND pc.category_id = ?
		JOIN postings pa ON pa.transaction_id = t.id AND pa.account_id IS NOT NULL
		JOIN accounts a ON a.id = pa.account_id
		LEFT JOIN payees py ON py.id = t.payee_id
		WHERE t.status != 'void'
		AND (SELECT COUNT(*) FROM postings p WHERE p.transaction_id = t.id AND p.category_id IS NOT NULL) = 1
		AND NOT EXISTS (SELECT 1 FROM category_suggestions cs WHERE cs.transaction_id = t.id AND cs.status = 'pending')
		GROUP BY t.id ORDER BY t.occurred_on DESC, t.seq DESC LIMIT ?`, uncategorized, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SuggestionCandidate{}
	for rows.Next() {
		var c SuggestionCandidate
		if err := rows.Scan(&c.TransactionID, &c.OccurredOn, &c.Description, &c.AmountCents, &c.AccountName, &c.AccountKind, &c.PayeeName); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SuggestionInput is one suggestion as a categorizer reports it.
type SuggestionInput struct {
	TransactionID string  `json:"transaction_id"`
	CategoryID    string  `json:"category_id"`
	Confidence    float64 `json:"confidence"`
	Rationale     string  `json:"rationale"`
	NeedsReview   bool    `json:"needs_review"`
}

// SuggestionBatch is one categorizer run's suggestions.
type SuggestionBatch struct {
	OperationID string            `json:"operation_id"`
	Model       string            `json:"model"`
	Suggestions []SuggestionInput `json:"suggestions"`
}

// CreateSuggestions records a run's suggestions. A category must be an
// expense or income category; an empty one means a person must decide. Each
// supersedes any pending suggestion for the same transaction. All or nothing.
func (s *Store) CreateSuggestions(batch SuggestionBatch) (int, error) {
	if strings.TrimSpace(batch.OperationID) == "" || strings.TrimSpace(batch.Model) == "" {
		return 0, fmt.Errorf("%w: operation_id and model are required: say what produced the suggestions", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := s.unixNow()
	for i, in := range batch.Suggestions {
		if err := requireExists(tx, "transactions", fmt.Sprintf("suggestion %d transaction_id", i), in.TransactionID); err != nil {
			return 0, err
		}
		if in.Confidence < 0 || in.Confidence > 1 {
			return 0, fmt.Errorf("%w: suggestion %d confidence %v is not between 0 and 1", ErrInvalid, i, in.Confidence)
		}
		var categoryID any
		if in.CategoryID != "" {
			var kind string
			if err := tx.QueryRow(`SELECT kind FROM categories WHERE id = ?`, in.CategoryID).Scan(&kind); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return 0, fmt.Errorf("%w: suggestion %d category_id %q does not exist", ErrInvalid, i, in.CategoryID)
				}
				return 0, err
			}
			if kind != "expense" && kind != "income" {
				return 0, fmt.Errorf("%w: suggestion %d names a %s category; only expense and income categories may be suggested", ErrInvalid, i, kind)
			}
			categoryID = in.CategoryID
		}
		if _, err := tx.Exec(`UPDATE category_suggestions SET status = 'superseded', decided_at = ? WHERE transaction_id = ? AND status = 'pending'`, now, in.TransactionID); err != nil {
			return 0, err
		}
		seq, id, err := mintID(tx, "category_suggestions", "category_suggestion")
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO category_suggestions (seq, id, transaction_id, category_id, confidence, rationale, needs_review, model, operation_id, status, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,'pending',?)`,
			seq, id, in.TransactionID, categoryID, in.Confidence, in.Rationale, boolInt(in.NeedsReview), batch.Model, batch.OperationID, now); err != nil {
			return 0, err
		}
	}
	return len(batch.Suggestions), tx.Commit()
}

// ListSuggestions returns suggestions with the given status (all when
// empty), with each transaction's date, text, amount and account, ordered by
// suggested category and then date, so a reviewer reads like with like.
func (s *Store) ListSuggestions(status string, limit int) ([]CategorySuggestion, error) {
	if status != "" {
		if err := requireOneOf("status", status, suggestionStatuses); err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	paths, err := s.CategoryPaths()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT cs.id, cs.transaction_id, COALESCE(cs.category_id,''), cs.confidence, cs.rationale, cs.needs_review, cs.model, cs.operation_id,
		cs.status, COALESCE(cs.decided_category_id,''), cs.decided_at, cs.created_at, t.occurred_on, t.description,
		COALESCE((SELECT SUM(amount_cents) FROM postings WHERE transaction_id = t.id AND category_id IS NOT NULL), 0),
		COALESCE((SELECT a.id FROM postings p JOIN accounts a ON a.id = p.account_id WHERE p.transaction_id = t.id ORDER BY p.seq LIMIT 1), ''),
		COALESCE((SELECT a.name FROM postings p JOIN accounts a ON a.id = p.account_id WHERE p.transaction_id = t.id ORDER BY p.seq LIMIT 1), '')
		FROM category_suggestions cs JOIN transactions t ON t.id = cs.transaction_id
		WHERE (? = '' OR cs.status = ?) ORDER BY cs.seq LIMIT ?`, status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CategorySuggestion{}
	for rows.Next() {
		var c CategorySuggestion
		var needsReview int
		if err := rows.Scan(&c.ID, &c.TransactionID, &c.CategoryID, &c.Confidence, &c.Rationale, &needsReview, &c.Model, &c.OperationID,
			&c.Status, &c.DecidedCategoryID, &c.DecidedAt, &c.CreatedAt, &c.OccurredOn, &c.Description, &c.AmountCents, &c.AccountID, &c.AccountName); err != nil {
			return nil, err
		}
		c.NeedsReview = needsReview == 1
		c.CategoryPath = paths[c.CategoryID]
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SuggestionDecision is the operator's answer to one suggestion: accept, or
// reject. CategoryID on an accept overrides the model's category.
type SuggestionDecision struct {
	SuggestionID string `json:"suggestion_id"`
	Action       string `json:"action"` // accept | reject
	CategoryID   string `json:"category_id"`
}

// DecisionResult counts what DecideSuggestions did.
type DecisionResult struct {
	Accepted int      `json:"accepted"`
	Edited   int      `json:"edited"`
	Rejected int      `json:"rejected"`
	Stale    []string `json:"stale"` // suggestions whose transaction someone already sorted or split
}

// DecideSuggestions applies the operator's decisions. Accepting writes the
// category onto the transaction's Uncategorized posting; a transaction no
// longer wholly Uncategorized is left alone and its suggestion marked stale.
// A decision on a suggestion that is not pending is a 409. All or nothing.
func (s *Store) DecideSuggestions(decisions []SuggestionDecision) (DecisionResult, error) {
	result := DecisionResult{Stale: []string{}}
	if len(decisions) == 0 {
		return result, fmt.Errorf("%w: no decisions", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	uncategorized, err := categoryIDForRole(tx, "uncategorized")
	if err != nil {
		return result, err
	}
	now := s.unixNow()
	for i, d := range decisions {
		var transactionID, suggested, status string
		err := tx.QueryRow(`SELECT transaction_id, COALESCE(category_id,''), status FROM category_suggestions WHERE id = ?`, d.SuggestionID).Scan(&transactionID, &suggested, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("%w: decision %d: suggestion %q does not exist", ErrInvalid, i, d.SuggestionID)
		}
		if err != nil {
			return result, err
		}
		if status != "pending" {
			return result, fmt.Errorf("%w: %s is already %s", ErrConflict, d.SuggestionID, status)
		}
		switch d.Action {
		case "reject":
			if _, err := tx.Exec(`UPDATE category_suggestions SET status = 'rejected', decided_at = ? WHERE id = ?`, now, d.SuggestionID); err != nil {
				return result, err
			}
			result.Rejected++
			continue
		case "accept":
		default:
			return result, fmt.Errorf("%w: decision %d action %q is not accept or reject", ErrInvalid, i, d.Action)
		}
		chosen := suggested
		if d.CategoryID != "" {
			chosen = d.CategoryID
		}
		if chosen == "" {
			return result, fmt.Errorf("%w: %s has no suggested category; give category_id to accept it", ErrInvalid, d.SuggestionID)
		}
		var kind string
		if err := tx.QueryRow(`SELECT kind FROM categories WHERE id = ?`, chosen).Scan(&kind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return result, fmt.Errorf("%w: decision %d category_id %q does not exist", ErrInvalid, i, chosen)
			}
			return result, err
		}
		if kind != "expense" && kind != "income" {
			return result, fmt.Errorf("%w: decision %d: a transaction can only be sorted into an expense or income category, not %s", ErrInvalid, i, kind)
		}
		postingID, err := wholeUncategorizedPosting(tx, transactionID, uncategorized)
		if err != nil {
			return result, err
		}
		if postingID == "" {
			if _, err := tx.Exec(`UPDATE category_suggestions SET status = 'stale', decided_at = ? WHERE id = ?`, now, d.SuggestionID); err != nil {
				return result, err
			}
			result.Stale = append(result.Stale, d.SuggestionID)
			continue
		}
		if _, err := tx.Exec(`UPDATE postings SET category_id = ? WHERE id = ?`, chosen, postingID); err != nil {
			return result, err
		}
		if _, err := tx.Exec(`UPDATE transactions SET updated_at = ? WHERE id = ?`, now, transactionID); err != nil {
			return result, err
		}
		newStatus := "accepted"
		var decided any
		if chosen != suggested {
			newStatus, decided = "edited", chosen
			result.Edited++
		} else {
			result.Accepted++
		}
		if _, err := tx.Exec(`UPDATE category_suggestions SET status = ?, decided_category_id = ?, decided_at = ? WHERE id = ?`, newStatus, decided, now, d.SuggestionID); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}
