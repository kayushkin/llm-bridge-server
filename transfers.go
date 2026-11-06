package budgetstore

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// minimumMarkerLengthWithoutPartner is the shortest marker that may move a
// row onto an account without a feed. With no partner row there is no amount
// to check, and four digits turn up inside unrelated card-terminal numbers.
const minimumMarkerLengthWithoutPartner = 8

// transferWindowDays is how far apart the two sides of one transfer may be
// dated: a payment leaves checking on Friday and reaches the card on Monday.
const transferWindowDays = 5

// TransferMatchResult counts what MatchTransfers did.
type TransferMatchResult struct {
	// Paired: two rows, one in each account's file, folded into one transaction.
	Paired int `json:"paired"`
	// AssignedWithoutPartner: a row that names an account whose own rows
	// cannot hold its other side, because the account has no feed (a loan
	// with no export) or its feed starts after the row, moved its
	// Uncategorized leg onto that account. Each is kept in
	// transfers_without_partner.
	AssignedWithoutPartner int `json:"assigned_without_partner"`
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
// A candidate that names an account whose own rows cannot hold its other side,
// because the account has has_own_feed = 0 or its imported rows start after
// the candidate's date, has no partner row to wait for, so its Uncategorized
// leg becomes a posting on that account.
// Only markers of minimumMarkerLengthWithoutPartner characters or more count
// here, since nothing else confirms the match.
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
	feedStarts := map[string]string{}
	coverageRows, err := tx.Query(`SELECT account_id, MIN(first_row_on) FROM import_batches WHERE undone_at = 0 AND first_row_on != '' GROUP BY account_id`)
	if err != nil {
		return result, err
	}
	for coverageRows.Next() {
		var accountID, firstRowOn string
		if err := coverageRows.Scan(&accountID, &firstRowOn); err != nil {
			coverageRows.Close()
			return result, err
		}
		feedStarts[accountID] = firstRowOn
	}
	coverageRows.Close()
	names := func(description, accountID string, minimumLength int) bool {
		for _, marker := range markers[accountID] {
			if len(marker) >= minimumLength && strings.Contains(description, marker) {
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
			if !names(a.description, b.accountID, 0) && !names(b.description, a.accountID, 0) {
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
		var target, reason string
		for accountID, isWithoutFeed := range withoutFeed {
			if accountID == c.accountID || !names(c.description, accountID, minimumMarkerLengthWithoutPartner) {
				continue
			}
			date := c.occurredOn.Format("2006-01-02")
			var why string
			switch {
			case isWithoutFeed:
				why = "the account has no feed of its own"
			case feedStarts[accountID] != "" && date < feedStarts[accountID]:
				why = "the account's imported rows start on " + feedStarts[accountID] + ", after this row"
			default:
				continue
			}
			if target != "" {
				return result, fmt.Errorf("%w: %s names two accounts that cannot hold its other side (%s and %s); make their markers more specific", ErrConflict, c.transactionID, target, accountID)
			}
			target, reason = accountID, why
		}
		if target == "" {
			continue
		}
		if _, err := tx.Exec(`UPDATE postings SET category_id = NULL, account_id = ? WHERE id = ?`, target, c.categoryPostingID); err != nil {
			return result, err
		}
		if _, err := tx.Exec(`INSERT INTO transfers_without_partner (posting_id, transaction_id, account_id, reason, created_at) VALUES (?,?,?,?,?)`,
			c.categoryPostingID, c.transactionID, target, reason, now); err != nil {
			return result, err
		}
		if _, err := tx.Exec(`UPDATE transactions SET updated_at = ? WHERE id = ?`, now, c.transactionID); err != nil {
			return result, err
		}
		result.AssignedWithoutPartner++
	}
	return result, tx.Commit()
}

// UndoTransferWithoutPartner puts a posting the matcher moved without a
// partner back on Uncategorized, so a file covering its date can be imported
// and paired properly.
func (s *Store) UndoTransferWithoutPartner(transactionID string) (Transaction, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Transaction{}, err
	}
	defer tx.Rollback()
	uncategorized, err := categoryIDForRole(tx, "uncategorized")
	if err != nil {
		return Transaction{}, err
	}
	rows, err := tx.Query(`SELECT posting_id FROM transfers_without_partner WHERE transaction_id = ?`, transactionID)
	if err != nil {
		return Transaction{}, err
	}
	var postingIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Transaction{}, err
		}
		postingIDs = append(postingIDs, id)
	}
	rows.Close()
	if len(postingIDs) == 0 {
		return Transaction{}, fmt.Errorf("%w: %s holds no transfer moved without a partner", ErrNotFound, transactionID)
	}
	for _, id := range postingIDs {
		if _, err := tx.Exec(`UPDATE postings SET account_id = NULL, category_id = ? WHERE id = ?`, uncategorized, id); err != nil {
			return Transaction{}, err
		}
		if _, err := tx.Exec(`DELETE FROM transfers_without_partner WHERE posting_id = ?`, id); err != nil {
			return Transaction{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE transactions SET updated_at = ? WHERE id = ?`, s.unixNow(), transactionID); err != nil {
		return Transaction{}, err
	}
	if err := tx.Commit(); err != nil {
		return Transaction{}, err
	}
	return s.GetTransaction(transactionID)
}
