package budget

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A loan's schedule splits each payment into interest and principal.
//
// It assumes simple interest charged daily on the principal still owed, the
// kind of loan whose statement says "interest is calculated daily on the
// outstanding balance": a payment's interest is the principal owed times the
// annual rate times the days since the last payment, over 365. A statement's
// own split for a payment (loan_payment_facts) is used instead wherever one
// is recorded; where computed payments meet a statement, the last computed
// one takes up the difference so the principal owed lands on the statement's
// figure, and says so.

// paymentEffectiveWindowDays is how far a payment's posting date may be from
// the date the lender applied it: an autopay due on the 12th posts on the
// 13th or 14th.
const paymentEffectiveWindowDays = 7

// maximumProjectedPayments bounds the projection, so a payment smaller than
// the interest cannot run it forever.
const maximumProjectedPayments = 600

// LoanPaymentFact is what a loan statement says about one payment.
type LoanPaymentFact struct {
	ID                         string `json:"id"`
	EffectiveOn                string `json:"effective_on"`
	InterestCents              int64  `json:"interest_cents"`
	PrincipalCents             int64  `json:"principal_cents"`
	PrincipalBalanceAfterCents int64  `json:"principal_balance_after_cents"`
	Source                     string `json:"source"`
	CreatedAt                  int64  `json:"created_at"`
}

// LoanScheduleRow is one payment: made (with its transaction) or projected.
type LoanScheduleRow struct {
	TransactionID     string `json:"transaction_id"` // empty for a projected payment
	EffectiveOn       string `json:"effective_on"`
	PaymentCents      int64  `json:"payment_cents"`
	InterestCents     int64  `json:"interest_cents"`
	PrincipalCents    int64  `json:"principal_cents"`
	BalanceAfterCents int64  `json:"balance_after_cents"` // principal still owed, positive
	// Source: "statement" (a statement's own split), "computed" (daily
	// interest at the rate), "computed, adjusted to meet a statement", or
	// "projected".
	Source string `json:"source"`
	Note   string `json:"note"`
}

// LoanSchedule is a loan's whole history and what is left.
type LoanSchedule struct {
	AccountID            string            `json:"account_id"`
	APRBasisPoints       int               `json:"apr_basis_points"`
	DisbursedOn          string            `json:"disbursed_on"`
	DisbursedCents       int64             `json:"disbursed_cents"`
	Payments             []LoanScheduleRow `json:"payments"`
	Projected            []LoanScheduleRow `json:"projected"`
	PaidInterestCents    int64             `json:"paid_interest_cents"`
	PaidPrincipalCents   int64             `json:"paid_principal_cents"`
	PrincipalOwedCents   int64             `json:"principal_owed_cents"`
	ProjectedInterest    int64             `json:"projected_interest_cents"`
	ProjectedPayoffOn    string            `json:"projected_payoff_on"`
	InterestCategoryID   string            `json:"interest_category_id"`
	LedgerBalanceCents   int64             `json:"ledger_balance_cents"`
	ScheduleAppliedToAll bool              `json:"schedule_applied_to_all"` // every made payment's postings already carry this split
}

// RecordLoanPaymentFacts stores statement splits for a loan, replacing any
// already recorded for the same effective date.
func (s *Ledger) RecordLoanPaymentFacts(accountID string, facts []LoanPaymentFact) ([]LoanPaymentFact, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var kind string
	if err := tx.QueryRow(`SELECT kind FROM accounts WHERE id = $1`, accountID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if kind != "loan" {
		return nil, fmt.Errorf("%w: %s is a %s account, not a loan", ErrInvalid, accountID, kind)
	}
	if len(facts) == 0 {
		return nil, fmt.Errorf("%w: no facts", ErrInvalid)
	}
	now := s.unixNow()
	for i, f := range facts {
		if err := validDate(fmt.Sprintf("fact %d effective_on", i), f.EffectiveOn, true); err != nil {
			return nil, err
		}
		if f.InterestCents < 0 || f.PrincipalCents < 0 || f.PrincipalBalanceAfterCents < 0 {
			return nil, fmt.Errorf("%w: fact %d has a negative amount; give amounts as the statement prints them", ErrInvalid, i)
		}
		if strings.TrimSpace(f.Source) == "" {
			return nil, fmt.Errorf("%w: fact %d needs a source: which statement says it", ErrInvalid, i)
		}
		if _, err := tx.Exec(`DELETE FROM loan_payment_facts WHERE account_id = $1 AND effective_on = $2`, accountID, f.EffectiveOn); err != nil {
			return nil, err
		}
		seq, id, err := mintID(tx, "loan_payment_facts", "loan_payment_fact")
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO loan_payment_facts (seq, id, account_id, effective_on, interest_cents, principal_cents, principal_balance_after_cents, source, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, seq, id, accountID, f.EffectiveOn, f.InterestCents, f.PrincipalCents, f.PrincipalBalanceAfterCents, strings.TrimSpace(f.Source), now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListLoanPaymentFacts(accountID)
}

// ListLoanPaymentFacts returns a loan's statement splits, oldest first.
func (s *Ledger) ListLoanPaymentFacts(accountID string) ([]LoanPaymentFact, error) {
	rows, err := s.db.Query(`SELECT id, effective_on, interest_cents, principal_cents, principal_balance_after_cents, source, created_at
		FROM loan_payment_facts WHERE account_id = $1 ORDER BY effective_on`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoanPaymentFact{}
	for rows.Next() {
		var f LoanPaymentFact
		if err := rows.Scan(&f.ID, &f.EffectiveOn, &f.InterestCents, &f.PrincipalCents, &f.PrincipalBalanceAfterCents, &f.Source, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// dailyInterest is principal × rate × days / 365, rounded half up to the cent,
// in integer arithmetic.
func dailyInterest(principalCents int64, aprBasisPoints int, days int64) int64 {
	if principalCents <= 0 || days <= 0 {
		return 0
	}
	const denominator = 365 * 10000
	return (principalCents*int64(aprBasisPoints)*days + denominator/2) / denominator
}

func parseDay(value string) (time.Time, error) { return time.Parse("2006-01-02", value) }

// dueDateNear is the loan's due day in the month nearest to posted, when it
// lies within the window; else zero.
func dueDateNear(posted time.Time, dueDay int) time.Time {
	if dueDay <= 0 {
		return time.Time{}
	}
	best := time.Time{}
	for _, offset := range []int{-1, 0, 1} {
		first := time.Date(posted.Year(), posted.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC)
		lastDay := first.AddDate(0, 1, -1).Day()
		day := dueDay
		if day > lastDay {
			day = lastDay
		}
		candidate := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
		distance := posted.Sub(candidate)
		if distance < 0 {
			distance = -distance
		}
		if distance <= paymentEffectiveWindowDays*24*time.Hour && (best.IsZero() || distance < absDuration(posted.Sub(best))) {
			best = candidate
		}
	}
	return best
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

type loanMovement struct {
	transactionID string
	occurredOn    time.Time
	amountCents   int64 // what left (payment) or entered (payout) the other accounts
}

// loanMovements reads the payout and the payments of a loan: transactions
// that move money between the loan and another of the operator's accounts.
func loanMovements(q queryer, accountID string) (payout loanMovement, payments []loanMovement, err error) {
	rows, err := q.Query(`SELECT t.id, t.occurred_on,
			(SELECT COALESCE(SUM(p.amount_cents),0) FROM postings p WHERE p.transaction_id = t.id AND p.account_id IS NOT NULL AND p.account_id != $1)
		FROM transactions t
		WHERE t.status != 'void' AND t.id IN (SELECT transaction_id FROM postings WHERE account_id = $2)
		AND EXISTS (SELECT 1 FROM postings p WHERE p.transaction_id = t.id AND p.account_id IS NOT NULL AND p.account_id != $3)
		ORDER BY t.occurred_on, t.seq`, accountID, accountID, accountID)
	if err != nil {
		return payout, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m loanMovement
		var date string
		var otherAccounts int64
		if err := rows.Scan(&m.transactionID, &date, &otherAccounts); err != nil {
			return payout, nil, err
		}
		if m.occurredOn, err = parseDay(date); err != nil {
			return payout, nil, err
		}
		switch {
		case otherAccounts > 0: // money came out of the loan into another account
			if payout.transactionID != "" {
				return payout, nil, fmt.Errorf("%w: %s has two payouts (%s and %s); the schedule handles one", ErrConflict, accountID, payout.transactionID, m.transactionID)
			}
			m.amountCents = otherAccounts
			payout = m
		case otherAccounts < 0:
			m.amountCents = -otherAccounts
			payments = append(payments, m)
		}
	}
	if err := rows.Err(); err != nil {
		return payout, nil, err
	}
	if payout.transactionID == "" {
		return payout, nil, fmt.Errorf("%w: %s has no payout: record the transfer from the loan into an account first", ErrConflict, accountID)
	}
	return payout, payments, nil
}

// GetLoanSchedule works out a loan's schedule from its rate, its payout, its
// payments and its statement facts. It changes nothing.
func (s *Ledger) GetLoanSchedule(accountID string) (LoanSchedule, error) {
	return s.loanSchedule(s.db, accountID)
}

func (s *Ledger) loanSchedule(q queryer, accountID string) (LoanSchedule, error) {
	account, err := s.GetAccount(accountID)
	if err != nil {
		return LoanSchedule{}, err
	}
	if account.Kind != "loan" || account.Loan == nil || account.Loan.APRBasisPoints <= 0 {
		return LoanSchedule{}, fmt.Errorf("%w: %s needs to be a loan with loan.apr_basis_points set", ErrInvalid, accountID)
	}
	dueDay := 0
	if account.Loan.FirstPaymentOn != "" {
		first, err := parseDay(account.Loan.FirstPaymentOn)
		if err != nil {
			return LoanSchedule{}, err
		}
		dueDay = first.Day()
	}
	payout, payments, err := loanMovements(q, accountID)
	if err != nil {
		return LoanSchedule{}, err
	}
	facts, err := s.ListLoanPaymentFacts(accountID)
	if err != nil {
		return LoanSchedule{}, err
	}
	factOn := map[string]LoanPaymentFact{}
	for _, f := range facts {
		factOn[f.EffectiveOn] = f
	}
	schedule := LoanSchedule{
		AccountID: accountID, APRBasisPoints: account.Loan.APRBasisPoints,
		DisbursedOn: payout.occurredOn.Format("2006-01-02"), DisbursedCents: payout.amountCents,
		InterestCategoryID: account.Loan.InterestCategoryID, LedgerBalanceCents: account.BalanceCents,
		Payments: []LoanScheduleRow{}, Projected: []LoanScheduleRow{},
	}
	balance := payout.amountCents
	last := payout.occurredOn
	lastComputed := -1 // index of the last computed row since a statement
	used := map[string]bool{}
	for _, payment := range payments {
		effective := payment.occurredOn
		var fact *LoanPaymentFact
		for _, f := range facts {
			day, err := parseDay(f.EffectiveOn)
			if err != nil {
				return LoanSchedule{}, err
			}
			if !used[f.EffectiveOn] && absDuration(payment.occurredOn.Sub(day)) <= paymentEffectiveWindowDays*24*time.Hour {
				copied := f
				fact, effective = &copied, day
				used[f.EffectiveOn] = true
				break
			}
		}
		if fact == nil {
			if due := dueDateNear(payment.occurredOn, dueDay); !due.IsZero() {
				effective = due
			}
		}
		row := LoanScheduleRow{TransactionID: payment.transactionID, EffectiveOn: effective.Format("2006-01-02"), PaymentCents: payment.amountCents}
		if fact != nil {
			if fact.InterestCents+fact.PrincipalCents != payment.amountCents {
				return LoanSchedule{}, fmt.Errorf("%w: the statement splits the %s payment as %d + %d, but %s paid %d", ErrConflict,
					fact.EffectiveOn, fact.InterestCents, fact.PrincipalCents, payment.transactionID, payment.amountCents)
			}
			statedPrior := fact.PrincipalBalanceAfterCents + fact.PrincipalCents
			if difference := balance - statedPrior; difference != 0 {
				if lastComputed < 0 {
					return LoanSchedule{}, fmt.Errorf("%w: before the %s payment the schedule has %d owed and %s says %d, with no computed payment to take up the difference",
						ErrConflict, fact.EffectiveOn, balance, fact.Source, statedPrior)
				}
				adjusted := &schedule.Payments[lastComputed]
				adjusted.PrincipalCents += difference
				adjusted.InterestCents -= difference
				adjusted.Source = "computed, adjusted to meet a statement"
				adjusted.Note = fmt.Sprintf("moved %d cents from interest to principal so the principal owed before %s matches %s", difference, fact.EffectiveOn, fact.Source)
				for i := lastComputed; i < len(schedule.Payments); i++ {
					schedule.Payments[i].BalanceAfterCents -= difference
				}
				balance = statedPrior
			}
			row.InterestCents, row.PrincipalCents, row.Source, row.Note = fact.InterestCents, fact.PrincipalCents, "statement", fact.Source
			lastComputed = -1
		} else {
			days := int64(effective.Sub(last).Hours() / 24)
			row.InterestCents = dailyInterest(balance, account.Loan.APRBasisPoints, days)
			row.PrincipalCents = payment.amountCents - row.InterestCents
			row.Source = "computed"
			row.Note = fmt.Sprintf("%d days at %d.%02d%% on %d cents owed", days, account.Loan.APRBasisPoints/100, account.Loan.APRBasisPoints%100, balance)
			lastComputed = len(schedule.Payments)
		}
		balance -= row.PrincipalCents
		row.BalanceAfterCents = balance
		last = effective
		schedule.Payments = append(schedule.Payments, row)
	}
	for _, row := range schedule.Payments {
		schedule.PaidInterestCents += row.InterestCents
		schedule.PaidPrincipalCents += row.PrincipalCents
	}
	schedule.PrincipalOwedCents = balance

	// What is left, at the scheduled payment on each due day.
	if account.Loan.ScheduledPaymentCents > 0 && dueDay > 0 && balance > 0 {
		next := time.Date(last.Year(), last.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < maximumProjectedPayments && balance > 0; i++ {
			lastDay := next.AddDate(0, 1, -1).Day()
			day := min(dueDay, lastDay)
			due := time.Date(next.Year(), next.Month(), day, 0, 0, 0, 0, time.UTC)
			interest := dailyInterest(balance, account.Loan.APRBasisPoints, int64(due.Sub(last).Hours()/24))
			payment := account.Loan.ScheduledPaymentCents
			if payment-interest >= balance {
				payment = balance + interest
			}
			if payment <= interest {
				return LoanSchedule{}, fmt.Errorf("%w: the scheduled payment %d does not cover the interest %d", ErrConflict, payment, interest)
			}
			balance -= payment - interest
			schedule.Projected = append(schedule.Projected, LoanScheduleRow{EffectiveOn: due.Format("2006-01-02"), PaymentCents: payment,
				InterestCents: interest, PrincipalCents: payment - interest, BalanceAfterCents: balance, Source: "projected"})
			schedule.ProjectedInterest += interest
			schedule.ProjectedPayoffOn = due.Format("2006-01-02")
			last = due
			next = next.AddDate(0, 1, 0)
		}
	}

	// Whether every made payment's postings already carry this split.
	schedule.ScheduleAppliedToAll = len(schedule.Payments) > 0
	for _, row := range schedule.Payments {
		var onLoan, onInterest int64
		if err := q.QueryRow(`SELECT COALESCE(SUM(CASE WHEN account_id = $1 THEN amount_cents END),0),
			COALESCE(SUM(CASE WHEN category_id = $2 THEN amount_cents END),0) FROM postings WHERE transaction_id = $3`,
			accountID, account.Loan.InterestCategoryID, row.TransactionID).Scan(&onLoan, &onInterest); err != nil {
			return LoanSchedule{}, err
		}
		if onLoan != row.PrincipalCents || onInterest != row.InterestCents {
			schedule.ScheduleAppliedToAll = false
			break
		}
	}
	return schedule, nil
}

// ApplyLoanSchedule writes the schedule's split onto each payment: the loan
// gets the principal, and the loan's interest category the interest. Run it
// again after a new statement fact or payment; it replaces its own earlier
// split. The account needs loan.interest_category_id.
func (s *Ledger) ApplyLoanSchedule(accountID string) (LoanSchedule, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return LoanSchedule{}, err
	}
	defer tx.Rollback()
	schedule, err := s.loanSchedule(tx, accountID)
	if err != nil {
		return LoanSchedule{}, err
	}
	if schedule.InterestCategoryID == "" {
		return LoanSchedule{}, fmt.Errorf("%w: set loan.interest_category_id on %s first: where should the interest go", ErrInvalid, accountID)
	}
	now := s.unixNow()
	for _, row := range schedule.Payments {
		if row.InterestCents < 0 || row.PrincipalCents < 0 {
			return LoanSchedule{}, fmt.Errorf("%w: the %s payment splits as %d interest and %d principal; check the rate and the statement facts", ErrConflict, row.EffectiveOn, row.InterestCents, row.PrincipalCents)
		}
		if _, err := tx.Exec(`UPDATE postings SET amount_cents = $1 WHERE transaction_id = $2 AND account_id = $3`, row.PrincipalCents, row.TransactionID, accountID); err != nil {
			return LoanSchedule{}, err
		}
		var interestPostingID string
		err := tx.QueryRow(`SELECT id FROM postings WHERE transaction_id = $1 AND category_id = $2`, row.TransactionID, schedule.InterestCategoryID).Scan(&interestPostingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if row.InterestCents > 0 {
				if err := insertPosting(tx, row.TransactionID, Posting{CategoryID: schedule.InterestCategoryID, AmountCents: row.InterestCents, Memo: "interest part of the loan payment"}); err != nil {
					return LoanSchedule{}, err
				}
			}
		case err != nil:
			return LoanSchedule{}, err
		default:
			if _, err := tx.Exec(`UPDATE postings SET amount_cents = $1 WHERE id = $2`, row.InterestCents, interestPostingID); err != nil {
				return LoanSchedule{}, err
			}
		}
		var sum int64
		if err := tx.QueryRow(`SELECT SUM(amount_cents) FROM postings WHERE transaction_id = $1`, row.TransactionID).Scan(&sum); err != nil {
			return LoanSchedule{}, err
		}
		if sum != 0 {
			return LoanSchedule{}, fmt.Errorf("%w: %s would sum to %d after the split", ErrConflict, row.TransactionID, sum)
		}
		if _, err := tx.Exec(`UPDATE transactions SET updated_at = $1 WHERE id = $2`, now, row.TransactionID); err != nil {
			return LoanSchedule{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return LoanSchedule{}, err
	}
	return s.GetLoanSchedule(accountID)
}

// CategoryAccountMonthTotal is a category tree's postings in one month, by
// the account the money moved in.
type CategoryAccountMonthTotal struct {
	Month        string `json:"month"`
	AccountID    string `json:"account_id"`
	CategoryID   string `json:"category_id"`
	TotalCents   int64  `json:"total_cents"`
	Transactions int    `json:"transactions"`
}

// SpendingByAccountAndMonth totals the postings of the given categories and
// every category under them, per month, per category and per account of the
// same transaction. Card interest by card and month is this with the card
// interest category.
func (s *Ledger) SpendingByAccountAndMonth(categoryIDs []string, fromMonth, toMonth string) ([]CategoryAccountMonthTotal, error) {
	if len(categoryIDs) == 0 {
		return nil, fmt.Errorf("%w: name at least one category_id", ErrInvalid)
	}
	for name, value := range map[string]string{"from": fromMonth, "to": toMonth} {
		if value != "" {
			if err := validMonth(value); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	var args arguments
	rows, err := s.db.Query(`WITH RECURSIVE tree(id) AS (SELECT id FROM categories WHERE id = ANY(`+args.add(categoryIDs)+`::text[])
			UNION ALL SELECT c.id FROM categories c JOIN tree ON c.parent_id = tree.id)
		SELECT substr(t.occurred_on,1,7) AS month, pa.account_id, pc.category_id, SUM(pc.amount_cents), COUNT(DISTINCT t.id)
		FROM postings pc JOIN transactions t ON t.id = pc.transaction_id
		JOIN postings pa ON pa.transaction_id = t.id AND pa.account_id IS NOT NULL
		WHERE pc.category_id IN (SELECT id FROM tree) AND t.status != 'void'
		AND (`+args.add(fromMonth)+` = '' OR substr(t.occurred_on,1,7) >= `+args.add(fromMonth)+`) AND (`+args.add(toMonth)+` = '' OR substr(t.occurred_on,1,7) <= `+args.add(toMonth)+`)
		AND pa.id = (SELECT MIN(p2.id) FROM postings p2 WHERE p2.transaction_id = t.id AND p2.account_id IS NOT NULL)
		GROUP BY month, pa.account_id, pc.category_id ORDER BY month, pa.account_id`, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CategoryAccountMonthTotal{}
	for rows.Next() {
		var t CategoryAccountMonthTotal
		if err := rows.Scan(&t.Month, &t.AccountID, &t.CategoryID, &t.TotalCents, &t.Transactions); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Month < out[j].Month })
	return out, rows.Err()
}
