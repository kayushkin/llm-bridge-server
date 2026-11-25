package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// An interest gathers the money spent on one pursuit across categories:
// skating is the Skating category, but also skates bought under Shopping and
// a competition's entry fee. The operator's rules decide what belongs, so a
// rule's match is confirmed at once; a transaction the operator removes stays
// removed, and one they add stays added.

var placeRefPattern = regexp.MustCompile(`^place_\d{6,}$`)

// Interest is one pursuit with its rules and history sources.
type Interest struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Description    string                  `json:"description"`
	Rules          []InterestRule          `json:"rules"`
	HistorySources []InterestHistorySource `json:"history_sources"`
	// TotalCents is everything confirmed, ever; Transactions how many.
	TotalCents   int64 `json:"total_cents"`
	Transactions int   `json:"transactions"`
	CreatedAt    int64 `json:"created_at"`
	UpdatedAt    int64 `json:"updated_at"`
}

// InterestRule says what belongs to an interest.
type InterestRule struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// InterestHistorySource says where an interest's history lives in another store.
type InterestHistorySource struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Note string `json:"note"`
}

// InterestTransaction is one transaction of an interest.
type InterestTransaction struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	DecidedBy     string `json:"decided_by"`
	OccurredOn    string `json:"occurred_on"`
	Description   string `json:"description"`
	AmountCents   int64  `json:"amount_cents"` // spending positive, refunds negative
	CategoryID    string `json:"category_id"`
	AccountID     string `json:"account_id"`
}

// CreateInterest adds an interest.
func (s *Store) CreateInterest(name, description string) (Interest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Interest{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Interest{}, err
	}
	defer tx.Rollback()
	seq, id, err := mintID(tx, "interests", "interest")
	if err != nil {
		return Interest{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO interests (seq, id, name, description, created_at, updated_at) VALUES (?,?,?,?,?,?)`, seq, id, name, strings.TrimSpace(description), now, now); err != nil {
		return Interest{}, err
	}
	if err := tx.Commit(); err != nil {
		return Interest{}, err
	}
	return s.GetInterest(id)
}

// AddInterestRule adds a rule and confirms every transaction it matches.
func (s *Store) AddInterestRule(interestID, kind, value string) (Interest, error) {
	if err := requireOneOf("kind", kind, interestRuleKinds); err != nil {
		return Interest{}, err
	}
	value = strings.TrimSpace(value)
	tx, err := s.db.Begin()
	if err != nil {
		return Interest{}, err
	}
	defer tx.Rollback()
	if found, err := exists(tx, "interests", interestID); err != nil {
		return Interest{}, err
	} else if !found {
		return Interest{}, ErrNotFound
	}
	switch kind {
	case "category":
		if err := requireExists(tx, "categories", "value", value); err != nil {
			return Interest{}, err
		}
	case "payee":
		if err := requireExists(tx, "payees", "value", value); err != nil {
			return Interest{}, err
		}
	case "description":
		value = strings.ToUpper(value)
		if len(value) < 3 {
			return Interest{}, fmt.Errorf("%w: description text needs at least 3 characters", ErrInvalid)
		}
	}
	seq, id, err := mintID(tx, "interest_rules", "interest_rule")
	if err != nil {
		return Interest{}, err
	}
	if _, err := tx.Exec(`INSERT INTO interest_rules (seq, id, interest_id, kind, value, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(interest_id, kind, value) DO NOTHING`, seq, id, interestID, kind, value, s.unixNow()); err != nil {
		return Interest{}, err
	}
	if err := s.matchInterest(tx, interestID); err != nil {
		return Interest{}, err
	}
	if err := tx.Commit(); err != nil {
		return Interest{}, err
	}
	return s.GetInterest(interestID)
}

// AddInterestHistorySource records where an interest's history lives.
func (s *Store) AddInterestHistorySource(interestID, kind, ref, note string) (Interest, error) {
	if err := requireOneOf("kind", kind, interestHistoryKinds); err != nil {
		return Interest{}, err
	}
	ref = strings.TrimSpace(ref)
	if kind == "location_place" && !placeRefPattern.MatchString(ref) {
		return Interest{}, fmt.Errorf("%w: ref %q is not a location-store place id (place_000001)", ErrInvalid, ref)
	}
	if ref == "" {
		return Interest{}, fmt.Errorf("%w: ref is required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Interest{}, err
	}
	defer tx.Rollback()
	if found, err := exists(tx, "interests", interestID); err != nil {
		return Interest{}, err
	} else if !found {
		return Interest{}, ErrNotFound
	}
	seq, id, err := mintID(tx, "interest_history_sources", "interest_history_source")
	if err != nil {
		return Interest{}, err
	}
	if _, err := tx.Exec(`INSERT INTO interest_history_sources (seq, id, interest_id, kind, ref, note, created_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(interest_id, kind, ref) DO NOTHING`, seq, id, interestID, kind, ref, strings.TrimSpace(note), s.unixNow()); err != nil {
		return Interest{}, err
	}
	if err := tx.Commit(); err != nil {
		return Interest{}, err
	}
	return s.GetInterest(interestID)
}

// matchInterest confirms every transaction an interest's rules match that
// the operator has not removed.
func (s *Store) matchInterest(tx queryer, interestID string) error {
	rules, err := tx.Query(`SELECT kind, value FROM interest_rules WHERE interest_id = ?`, interestID)
	if err != nil {
		return err
	}
	type rule struct{ kind, value string }
	var list []rule
	for rules.Next() {
		var r rule
		if err := rules.Scan(&r.kind, &r.value); err != nil {
			rules.Close()
			return err
		}
		list = append(list, r)
	}
	rules.Close()
	now := s.unixNow()
	for _, r := range list {
		var query string
		var reason string
		switch r.kind {
		case "category":
			query = `SELECT DISTINCT p.transaction_id AS id FROM postings p JOIN transactions t ON t.id = p.transaction_id WHERE t.status != 'void' AND p.category_id IN (
				WITH RECURSIVE tree(id) AS (SELECT ? UNION ALL SELECT c.id FROM categories c JOIN tree ON c.parent_id = tree.id) SELECT id FROM tree)`
			reason = "category " + r.value
		case "payee":
			query = `SELECT id FROM transactions WHERE status != 'void' AND payee_id = ?`
			reason = "payee " + r.value
		case "description":
			query = `SELECT id FROM transactions WHERE status != 'void' AND instr(upper(description), ?) > 0`
			reason = "description holds " + r.value
		}
		if _, err := tx.Exec(`INSERT INTO interest_transactions (interest_id, transaction_id, status, reason, created_at, updated_at)
			SELECT ?, id, 'confirmed', ?, ?, ? FROM (`+query+`) AS matched WHERE true
			ON CONFLICT(interest_id, transaction_id) DO NOTHING`, interestID, "matched the rule: "+reason, now, now, r.value); err != nil {
			return err
		}
	}
	return nil
}

// MatchInterests runs every interest's rules again, for transactions that
// arrived since. Answers how many links it added.
func (s *Store) MatchInterests() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var before, after int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM interest_transactions`).Scan(&before); err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT id FROM interests`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.matchInterest(tx, id); err != nil {
			return 0, err
		}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM interest_transactions`).Scan(&after); err != nil {
		return 0, err
	}
	return after - before, tx.Commit()
}

// SetInterestTransaction adds a transaction to an interest or removes it.
func (s *Store) SetInterestTransaction(interestID, transactionID, status, decidedBy string) (InterestTransaction, error) {
	if err := requireOneOf("status", status, []string{"confirmed", "removed"}); err != nil {
		return InterestTransaction{}, err
	}
	decidedBy = strings.TrimSpace(decidedBy)
	if decidedBy == "" {
		return InterestTransaction{}, fmt.Errorf("%w: decided_by is required: say who decided", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return InterestTransaction{}, err
	}
	defer tx.Rollback()
	if found, err := exists(tx, "interests", interestID); err != nil {
		return InterestTransaction{}, err
	} else if !found {
		return InterestTransaction{}, ErrNotFound
	}
	if err := requireExists(tx, "transactions", "transaction_id", transactionID); err != nil {
		return InterestTransaction{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO interest_transactions (interest_id, transaction_id, status, reason, decided_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(interest_id, transaction_id) DO UPDATE SET status = excluded.status, decided_by = excluded.decided_by, updated_at = excluded.updated_at`,
		interestID, transactionID, status, "set by "+decidedBy, decidedBy, now, now); err != nil {
		return InterestTransaction{}, err
	}
	if err := tx.Commit(); err != nil {
		return InterestTransaction{}, err
	}
	list, err := s.ListInterestTransactions(interestID)
	if err != nil {
		return InterestTransaction{}, err
	}
	for _, t := range list {
		if t.TransactionID == transactionID {
			return t, nil
		}
	}
	return InterestTransaction{}, fmt.Errorf("%s vanished after it was written", transactionID)
}

// ListInterestTransactions returns an interest's transactions, newest first,
// removed ones included.
func (s *Store) ListInterestTransactions(interestID string) ([]InterestTransaction, error) {
	rows, err := s.db.Query(`SELECT it.transaction_id, it.status, it.reason, it.decided_by, t.occurred_on, t.description,
			COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN categories c ON c.id = p.category_id WHERE p.transaction_id = t.id AND c.kind IN ('expense','uncategorized')), 0),
			COALESCE((SELECT MIN(p.category_id) FROM postings p WHERE p.transaction_id = t.id AND p.category_id IS NOT NULL HAVING COUNT(*) = 1), ''),
			COALESCE((SELECT MIN(p.account_id) FROM postings p WHERE p.transaction_id = t.id AND p.account_id IS NOT NULL), '')
		FROM interest_transactions it JOIN transactions t ON t.id = it.transaction_id
		WHERE it.interest_id = ? ORDER BY t.occurred_on DESC, t.seq DESC`, interestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InterestTransaction{}
	for rows.Next() {
		var t InterestTransaction
		if err := rows.Scan(&t.TransactionID, &t.Status, &t.Reason, &t.DecidedBy, &t.OccurredOn, &t.Description, &t.AmountCents, &t.CategoryID, &t.AccountID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetInterest returns one interest with its rules, sources and totals.
func (s *Store) GetInterest(id string) (Interest, error) {
	var i Interest
	err := s.db.QueryRow(`SELECT id, name, description, created_at, updated_at FROM interests WHERE id = ?`, id).Scan(&i.ID, &i.Name, &i.Description, &i.CreatedAt, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Interest{}, ErrNotFound
	}
	if err != nil {
		return Interest{}, err
	}
	i.Rules = []InterestRule{}
	rows, err := s.db.Query(`SELECT id, kind, value FROM interest_rules WHERE interest_id = ? ORDER BY seq`, id)
	if err != nil {
		return Interest{}, err
	}
	for rows.Next() {
		var r InterestRule
		if err := rows.Scan(&r.ID, &r.Kind, &r.Value); err != nil {
			rows.Close()
			return Interest{}, err
		}
		i.Rules = append(i.Rules, r)
	}
	rows.Close()
	i.HistorySources = []InterestHistorySource{}
	rows, err = s.db.Query(`SELECT id, kind, ref, note FROM interest_history_sources WHERE interest_id = ? ORDER BY seq`, id)
	if err != nil {
		return Interest{}, err
	}
	for rows.Next() {
		var h InterestHistorySource
		if err := rows.Scan(&h.ID, &h.Kind, &h.Ref, &h.Note); err != nil {
			rows.Close()
			return Interest{}, err
		}
		i.HistorySources = append(i.HistorySources, h)
	}
	rows.Close()
	err = s.db.QueryRow(`SELECT COALESCE(SUM((SELECT COALESCE(SUM(p.amount_cents),0) FROM postings p JOIN categories c ON c.id = p.category_id
			WHERE p.transaction_id = it.transaction_id AND c.kind IN ('expense','uncategorized'))),0), COUNT(*)
		FROM interest_transactions it WHERE it.interest_id = ? AND it.status = 'confirmed'`, id).Scan(&i.TotalCents, &i.Transactions)
	return i, err
}

// ListInterests returns every interest.
func (s *Store) ListInterests() ([]Interest, error) {
	rows, err := s.db.Query(`SELECT id FROM interests ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Interest{}
	for _, id := range ids {
		i, err := s.GetInterest(id)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, nil
}
