package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// tripRefPattern is location-store's trip id. This store checks the shape
// only; location-store owns the trip.
var tripRefPattern = regexp.MustCompile(`^trip_\d{6,}$`)

// tripPostingLagDays is how long after a trip a card charge made on it may
// still post.
const tripPostingLagDays = 3

// TripTransaction is one transaction linked to a trip.
type TripTransaction struct {
	TripRef       string `json:"trip_ref"`
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	DecidedBy     string `json:"decided_by"`
	OccurredOn    string `json:"occurred_on"`
	Description   string `json:"description"`
	AmountCents   int64  `json:"amount_cents"` // spending, positive
	CategoryID    string `json:"category_id"`  // the transaction's one category, when it has one
	AccountID     string `json:"account_id"`
}

// TripTotal is what one trip cost.
type TripTotal struct {
	TripRef        string `json:"trip_ref"`
	ConfirmedCents int64  `json:"confirmed_cents"`
	SuggestedCents int64  `json:"suggested_cents"`
	Confirmed      int    `json:"confirmed"`
	Suggested      int    `json:"suggested"`
}

func validTripRef(ref string) error {
	if !tripRefPattern.MatchString(ref) {
		return fmt.Errorf("%w: trip_ref %q is not a location-store trip id (trip_000001)", ErrInvalid, ref)
	}
	return nil
}

// TripSuggestRequest says which days a trip covers and what text marks a
// charge made at home.
type TripSuggestRequest struct {
	FirstDay string `json:"first_day"` // the operator's local date the trip began
	LastDay  string `json:"last_day"`  // and ended
	// HomeMarkers is text that marks a charge as made at home ("RENO NV");
	// the caller supplies it, since it is the operator's data.
	HomeMarkers []string `json:"home_markers"`
}

// SuggestTripTransactions links to a trip, as suggestions, the spending dated
// from its first day to its last plus tripPostingLagDays, leaving out
// payments of confirmed or suggested recurring bills, charges whose text
// carries a home marker, and transactions already linked to this trip or
// confirmed for another. Answers how many it suggested.
func (s *Store) SuggestTripTransactions(tripRef string, request TripSuggestRequest) (int, error) {
	if err := validTripRef(tripRef); err != nil {
		return 0, err
	}
	if err := validDate("first_day", request.FirstDay, true); err != nil {
		return 0, err
	}
	if err := validDate("last_day", request.LastDay, true); err != nil {
		return 0, err
	}
	last, err := parseDay(request.LastDay)
	if err != nil {
		return 0, err
	}
	until := last.AddDate(0, 0, tripPostingLagDays).Format("2006-01-02")
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT t.id, t.description FROM transactions t
		WHERE t.status != 'void' AND t.occurred_on BETWEEN ? AND ?
		AND (SELECT COALESCE(SUM(p.amount_cents),0) FROM postings p JOIN categories c ON c.id = p.category_id
			WHERE p.transaction_id = t.id AND c.kind IN ('expense','uncategorized')) > 0
		AND t.id NOT IN (SELECT rp.transaction_id FROM recurring_bill_payments rp JOIN recurring_bills b ON b.id = rp.recurring_bill_id WHERE b.status IN ('confirmed','suggested'))
		AND t.id NOT IN (SELECT transaction_id FROM trip_transactions WHERE trip_ref = ?)
		AND t.id NOT IN (SELECT transaction_id FROM trip_transactions WHERE status = 'confirmed')`,
		request.FirstDay, until, tripRef)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, description string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.description); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	now := s.unixNow()
	suggested := 0
	for _, c := range candidates {
		atHome := false
		for _, marker := range request.HomeMarkers {
			if marker = strings.ToUpper(strings.TrimSpace(marker)); marker != "" && strings.Contains(strings.ToUpper(c.description), marker) {
				atHome = true
				break
			}
		}
		if atHome {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO trip_transactions (trip_ref, transaction_id, status, reason, created_at, updated_at) VALUES (?,?,'suggested',?,?,?)`,
			tripRef, c.id, fmt.Sprintf("dated %s to %s (the trip plus %d days for posting)", request.FirstDay, until, tripPostingLagDays), now, now); err != nil {
			return 0, err
		}
		suggested++
	}
	return suggested, tx.Commit()
}

// SetTripTransaction confirms, removes or adds a transaction on a trip.
// Confirming drops the same transaction's suggestions on other trips.
// Adding one not yet linked needs status confirmed; decided_by is required.
func (s *Store) SetTripTransaction(tripRef, transactionID, status, decidedBy, reason string) (TripTransaction, error) {
	if err := validTripRef(tripRef); err != nil {
		return TripTransaction{}, err
	}
	if err := requireOneOf("status", status, []string{"confirmed", "removed"}); err != nil {
		return TripTransaction{}, err
	}
	decidedBy = strings.TrimSpace(decidedBy)
	if decidedBy == "" {
		return TripTransaction{}, fmt.Errorf("%w: decided_by is required: say who decided", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return TripTransaction{}, err
	}
	defer tx.Rollback()
	if err := requireExists(tx, "transactions", "transaction_id", transactionID); err != nil {
		return TripTransaction{}, err
	}
	if status == "confirmed" {
		var other string
		err := tx.QueryRow(`SELECT trip_ref FROM trip_transactions WHERE transaction_id = ? AND status = 'confirmed' AND trip_ref != ?`, transactionID, tripRef).Scan(&other)
		switch {
		case err == nil:
			return TripTransaction{}, fmt.Errorf("%w: %s is already confirmed on %s; remove it there first", ErrConflict, transactionID, other)
		case !errors.Is(err, sql.ErrNoRows):
			return TripTransaction{}, err
		}
	}
	if strings.TrimSpace(reason) == "" {
		reason = "added by " + decidedBy
	}
	now := s.unixNow()
	if status == "confirmed" {
		// Trips a day apart suggest the same late-posting charge for both;
		// once it is confirmed on one, the other's suggestion is moot.
		if _, err := tx.Exec(`DELETE FROM trip_transactions WHERE transaction_id = ? AND trip_ref != ? AND status = 'suggested'`, transactionID, tripRef); err != nil {
			return TripTransaction{}, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO trip_transactions (trip_ref, transaction_id, status, reason, decided_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(trip_ref, transaction_id) DO UPDATE SET status = excluded.status, decided_by = excluded.decided_by, updated_at = excluded.updated_at`,
		tripRef, transactionID, status, strings.TrimSpace(reason), decidedBy, now, now); err != nil {
		return TripTransaction{}, err
	}
	if err := tx.Commit(); err != nil {
		return TripTransaction{}, err
	}
	list, err := s.ListTripTransactions(tripRef)
	if err != nil {
		return TripTransaction{}, err
	}
	for _, t := range list {
		if t.TransactionID == transactionID {
			return t, nil
		}
	}
	return TripTransaction{}, fmt.Errorf("%s vanished after it was written", transactionID)
}

// ListTripTransactions returns a trip's linked transactions, oldest first,
// removed ones included so the page can show what was taken out.
func (s *Store) ListTripTransactions(tripRef string) ([]TripTransaction, error) {
	if err := validTripRef(tripRef); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT tt.trip_ref, tt.transaction_id, tt.status, tt.reason, tt.decided_by, t.occurred_on, t.description,
			COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN categories c ON c.id = p.category_id WHERE p.transaction_id = t.id AND c.kind IN ('expense','uncategorized')), 0),
			COALESCE((SELECT MIN(p.category_id) FROM postings p WHERE p.transaction_id = t.id AND p.category_id IS NOT NULL HAVING COUNT(*) = 1), ''),
			COALESCE((SELECT MIN(p.account_id) FROM postings p WHERE p.transaction_id = t.id AND p.account_id IS NOT NULL), '')
		FROM trip_transactions tt JOIN transactions t ON t.id = tt.transaction_id
		WHERE tt.trip_ref = ? ORDER BY t.occurred_on, t.seq`, tripRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TripTransaction{}
	for rows.Next() {
		var t TripTransaction
		if err := rows.Scan(&t.TripRef, &t.TransactionID, &t.Status, &t.Reason, &t.DecidedBy, &t.OccurredOn, &t.Description, &t.AmountCents, &t.CategoryID, &t.AccountID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTripTotals returns what each trip with linked spending cost.
func (s *Store) ListTripTotals() ([]TripTotal, error) {
	rows, err := s.db.Query(`SELECT tt.trip_ref,
			COALESCE(SUM(CASE WHEN tt.status = 'confirmed' THEN amount END),0), COALESCE(SUM(CASE WHEN tt.status = 'suggested' THEN amount END),0),
			SUM(tt.status = 'confirmed'), SUM(tt.status = 'suggested')
		FROM (SELECT tt.*, (SELECT COALESCE(SUM(p.amount_cents),0) FROM postings p JOIN categories c ON c.id = p.category_id
				WHERE p.transaction_id = tt.transaction_id AND c.kind IN ('expense','uncategorized')) AS amount FROM trip_transactions tt) tt
		GROUP BY tt.trip_ref ORDER BY tt.trip_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TripTotal{}
	for rows.Next() {
		var t TripTotal
		if err := rows.Scan(&t.TripRef, &t.ConfirmedCents, &t.SuggestedCents, &t.Confirmed, &t.Suggested); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
