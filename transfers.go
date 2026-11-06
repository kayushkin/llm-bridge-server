package budgetstore

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// transferWindowDays is how far apart the two sides of one transfer may be
// dated: a payment leaves checking on Friday and reaches the card on Monday.
const transferWindowDays = 5

// TransferMatchResult counts what MatchTransfers did.
type TransferMatchResult struct {
	// Paired: two rows, one in each account's file, folded into one transaction.
	Paired int `json:"paired"`
	// AssignedToAccountWithoutFeed: a row that names an account whose rows
	// never arrive by import (a loan with no export) moved its Uncategorized
	// leg onto that account.
	AssignedToAccountWithoutFeed int `json:"assigned_to_account_without_feed"`
}

type transferCandidate struct {
	transactionID     string
	occurredOn        time.Time
	description       string
	accountID         string
	amountCents       int64
	accountPostingID  string
	categoryPostingID string
}

// MatchTransfers finds money that moved between two of the operator's own
// accounts and records it once, as a transfer, instead of as spending in one
// account and income in the other.
//
// A candidate is a transaction still in its imported shape: one account
// posting and one Uncategorized posting. Two candidates pair when their
// amounts are opposite, they are dated within transferWindowDays, they are in
// different accounts, and one's description carries a description marker of
// the other's account. Without that last test a $2,000 rent payment and a
// $2,000 card payment the same week would pair. The pair becomes the money-out
// side's transaction with both account postings and both source rows; the
// other id is kept in transaction_merges.
//
// A candidate that names an account with has_own_feed = 0 has no partner row
// to wait for, so its Uncategorized leg becomes a posting on that account.
func (s *Store) MatchTransfers() (TransferMatchResult, error) {
	var result TransferMatchResult
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	uncategorized, err := categoryIDForRole(tx, "uncategorized")
	if err != nil {
		return result, err
	}
	rows, err := tx.Query(`SELECT t.id, t.occurred_on, t.description, pa.account_id, pa.amount_cents, pa.id, pc.id
		FROM transactions t
		JOIN postings pa ON pa.transaction_id = t.id AND pa.account_id IS NOT NULL
		JOIN postings pc ON pc.transaction_id = t.id AND pc.category_id = ?
		WHERE t.status != 'void' AND (SELECT COUNT(*) FROM postings p WHERE p.transaction_id = t.id) = 2
		ORDER BY t.seq`, uncategorized)
	if err != nil {
		return result, err
	}
	var candidates []transferCandidate
	for rows.Next() {
		var c transferCandidate
		var date string
		if err := rows.Scan(&c.transactionID, &date, &c.description, &c.accountID, &c.amountCents, &c.accountPostingID, &c.categoryPostingID); err != nil {
			rows.Close()
			return result, err
		}
		if c.occurredOn, err = time.Parse("2006-01-02", date); err != nil {
			rows.Close()
			return result, err
		}
		c.description = strings.ToUpper(c.description)
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}

	markers := map[string][]string{}
	withoutFeed := map[string]bool{}
	markerRows, err := tx.Query(`SELECT m.account_id, m.marker, a.has_own_feed FROM account_description_markers m JOIN accounts a ON a.id = m.account_id`)
	if err != nil {
		return result, err
	}
	for markerRows.Next() {
		var accountID, marker string
		var hasOwnFeed int
		if err := markerRows.Scan(&accountID, &marker, &hasOwnFeed); err != nil {
			markerRows.Close()
			return result, err
		}
		markers[accountID] = append(markers[accountID], strings.ToUpper(marker))
		withoutFeed[accountID] = hasOwnFeed == 0
	}
	markerRows.Close()
	names := func(description, accountID string) bool {
		for _, marker := range markers[accountID] {
			if strings.Contains(description, marker) {
				return true
			}
		}
		return false
	}

	type pair struct {
		out, in  int // indexes into candidates; out is the money-out side
		distance time.Duration
	}
	var pairs []pair
	for i := range candidates {
		for j := range candidates {
			a, b := candidates[i], candidates[j]
			if a.amountCents >= 0 || a.amountCents != -b.amountCents || a.accountID == b.accountID {
				continue
			}
			distance := a.occurredOn.Sub(b.occurredOn)
			if distance < 0 {
				distance = -distance
			}
			if distance > transferWindowDays*24*time.Hour {
				continue
			}
			if !names(a.description, b.accountID) && !names(b.description, a.accountID) {
				continue
			}
			pairs = append(pairs, pair{out: i, in: j, distance: distance})
		}
	}
	slices.SortStableFunc(pairs, func(x, y pair) int { return int(x.distance - y.distance) })
	used := map[int]bool{}
	now := s.unixNow()
	for _, p := range pairs {
		if used[p.out] || used[p.in] {
			continue
		}
		used[p.out], used[p.in] = true, true
		out, in := candidates[p.out], candidates[p.in]
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM postings WHERE id IN (?, ?)`, []any{out.categoryPostingID, in.categoryPostingID}},
			{`UPDATE postings SET transaction_id = ? WHERE id = ?`, []any{out.transactionID, in.accountPostingID}},
			{`UPDATE transaction_sources SET transaction_id = ? WHERE transaction_id = ?`, []any{out.transactionID, in.transactionID}},
			{`UPDATE transaction_merges SET into_transaction_id = ? WHERE into_transaction_id = ?`, []any{out.transactionID, in.transactionID}},
			{`INSERT INTO transaction_merges (merged_transaction_id, into_transaction_id, reason, merged_at) VALUES (?,?,?,?)`,
				[]any{in.transactionID, out.transactionID, fmt.Sprintf("transfer: %s in %s is the other side of %s in %s", in.transactionID, in.accountID, out.transactionID, out.accountID), now}},
			{`DELETE FROM transactions WHERE id = ?`, []any{in.transactionID}},
			{`UPDATE transactions SET updated_at = ? WHERE id = ?`, []any{now, out.transactionID}},
		} {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				return result, fmt.Errorf("pair %s with %s: %w", out.transactionID, in.transactionID, err)
			}
		}
		result.Paired++
	}

	for i, c := range candidates {
		if used[i] {
			continue
		}
		var target string
		for accountID, isWithoutFeed := range withoutFeed {
			if !isWithoutFeed || accountID == c.accountID || !names(c.description, accountID) {
				continue
			}
			if target != "" {
				return result, fmt.Errorf("%w: %s names two accounts without a feed (%s and %s); make their markers more specific", ErrConflict, c.transactionID, target, accountID)
			}
			target = accountID
		}
		if target == "" {
			continue
		}
		if _, err := tx.Exec(`UPDATE postings SET category_id = NULL, account_id = ? WHERE id = ?`, target, c.categoryPostingID); err != nil {
			return result, err
		}
		if _, err := tx.Exec(`UPDATE transactions SET updated_at = ? WHERE id = ?`, now, c.transactionID); err != nil {
			return result, err
		}
		result.AssignedToAccountWithoutFeed++
	}
	return result, tx.Commit()
}
