package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Recurring bills are found, not entered: the detector groups payments by
// what they are paid to and looks for a steady rhythm. A payment is money
// spent (a positive expense or Uncategorized posting) or a payment into a
// loan from another account. Paying a credit card is not a bill: the
// purchases behind it are already counted where they were made.

// cadenceSpec is one rhythm: the band of days between payments it accepts,
// how many months one period spans, and how many in-band gaps it needs.
type cadenceSpec struct {
	name           string
	minimumDays    float64
	maximumDays    float64
	months         int // 0 for weekly
	periodsPerYear float64
	minimumInBand  int
	graceDays      int // how late a payment may be before the bill counts as ended
}

var cadenceSpecs = []cadenceSpec{
	{name: "weekly", minimumDays: 5, maximumDays: 9, periodsPerYear: 52, minimumInBand: 3, graceDays: 10},
	{name: "monthly", minimumDays: 25, maximumDays: 35, months: 1, periodsPerYear: 12, minimumInBand: 2, graceDays: 20},
	{name: "quarterly", minimumDays: 84, maximumDays: 100, months: 3, periodsPerYear: 4, minimumInBand: 2, graceDays: 30},
	{name: "yearly", minimumDays: 350, maximumDays: 380, months: 12, periodsPerYear: 1, minimumInBand: 1, graceDays: 45},
}

func cadenceNamed(name string) (cadenceSpec, bool) {
	for _, c := range cadenceSpecs {
		if c.name == name {
			return c, true
		}
	}
	return cadenceSpec{}, false
}

// RecurringBill is one bill with what the detector last saw.
type RecurringBill struct {
	ID            string `json:"id"`
	MatchKey      string `json:"match_key"`
	Name          string `json:"name"`
	PayeeID       string `json:"payee_id"`
	Cadence       string `json:"cadence"`
	ExpectedCents int64  `json:"expected_cents"`
	DayOfMonth    int    `json:"day_of_month"`
	Status        string `json:"status"`
	FirstPaidOn   string `json:"first_paid_on"`
	LastPaidOn    string `json:"last_paid_on"`
	NextDueOn     string `json:"next_due_on"`
	PaymentCount  int    `json:"payment_count"`
	// MonthlyEquivalentCents is the bill spread over a month: a yearly bill
	// counts a twelfth.
	MonthlyEquivalentCents int64              `json:"monthly_equivalent_cents"`
	DecidedBy              string             `json:"decided_by"`
	DecidedAt              int64              `json:"decided_at"`
	CreatedAt              int64              `json:"created_at"`
	UpdatedAt              int64              `json:"updated_at"`
	Payments               []RecurringPayment `json:"payments,omitempty"`
}

// RecurringPayment is one payment of a bill.
type RecurringPayment struct {
	TransactionID string `json:"transaction_id"`
	OccurredOn    string `json:"occurred_on"`
	AmountCents   int64  `json:"amount_cents"`
	Description   string `json:"description"`
	// ChangedFromExpectedCents is this payment minus the one before it, so a
	// price rise shows; 0 for the first.
	ChangedFromPreviousCents int64 `json:"changed_from_previous_cents"`
}

type billPayment struct {
	transactionID string
	occurredOn    time.Time
	amountCents   int64
	description   string
	payeeID       string
	payeeName     string
}

var (
	billNoisePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(PURCHASE|RECURRING PAYMENT|MONEY TRANSFER) AUTHORIZED ON \d{2}/\d{2}`),
		regexp.MustCompile(`REF ?#\s*\S+`),
		regexp.MustCompile(`\bCARD \d+`),
		regexp.MustCompile(`\bS\d{6,}`),
		regexp.MustCompile(`\b\d{2}/\d{2}(/\d{2,4})?\b`),
		regexp.MustCompile(`[#*]\S*`),
		regexp.MustCompile(`\S*\d{3,}\S*`),
	}
	billSpaces = regexp.MustCompile(`\s+`)
)

// billTextKey reduces a bank description to the words that name who was
// paid: "PURCHASE AUTHORIZED ON 10/08 MAPLE FUEL #704 SPRINGFIELD ST S586…" → "MAPLE FUEL
// SPRINGFIELD ST".
func billTextKey(description string) string {
	text := strings.ToUpper(description)
	for _, pattern := range billNoisePatterns {
		text = pattern.ReplaceAllString(text, " ")
	}
	words := strings.Fields(billSpaces.ReplaceAllString(text, " "))
	if len(words) > 4 {
		words = words[:4]
	}
	return strings.Join(words, " ")
}

// billMatchKey is what groups a bill's payments: the payee when there is one,
// else the words of the description that name who was paid.
func billMatchKey(payeeID, description string) string {
	if payeeID != "" {
		return "payee:" + payeeID
	}
	return "text:" + billTextKey(description)
}

// periodStart is the first day of the period a payment falls in, for a bill
// the operator named: a month, a quarter, a year, or a week from Monday.
func periodStart(day time.Time, spec cadenceSpec) time.Time {
	switch spec.months {
	case 0:
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	case 1:
		return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	case 3:
		return time.Date(day.Year(), ((day.Month()-1)/3)*3+1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(day.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
}

// periodPayments folds payments into one per period, summed and dated by the
// period's last payment: a server bill charged twice in a month is that
// month's bill.
func periodPayments(payments []billPayment, spec cadenceSpec) []billPayment {
	out := []billPayment{}
	for _, p := range payments {
		if n := len(out); n > 0 && periodStart(out[n-1].occurredOn, spec).Equal(periodStart(p.occurredOn, spec)) {
			out[n-1].amountCents += p.amountCents
			out[n-1].occurredOn = p.occurredOn
			continue
		}
		out = append(out, p)
	}
	return out
}

// billPayments reads every transaction that could be a bill payment, with
// its amount and the key that groups it.
func billPayments(q queryer) (map[string][]billPayment, error) {
	rows, err := q.Query(`SELECT t.id, t.occurred_on, t.description, COALESCE(t.payee_id,''), COALESCE(py.name,''),
			COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN categories c ON c.id = p.category_id
				WHERE p.transaction_id = t.id AND c.kind IN ('expense','uncategorized')), 0),
			COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN accounts a ON a.id = p.account_id
				WHERE p.transaction_id = t.id AND a.kind = 'loan' AND p.amount_cents > 0), 0),
			COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN accounts a ON a.id = p.account_id
				WHERE p.transaction_id = t.id AND a.kind != 'loan' AND p.amount_cents < 0), 0)
		FROM transactions t LEFT JOIN payees py ON py.id = t.payee_id
		WHERE t.status != 'void'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string][]billPayment{}
	for rows.Next() {
		var p billPayment
		var date string
		var spent, intoLoan, outOfOtherAccounts int64
		if err := rows.Scan(&p.transactionID, &date, &p.description, &p.payeeID, &p.payeeName, &spent, &intoLoan, &outOfOtherAccounts); err != nil {
			return nil, err
		}
		if p.occurredOn, err = parseDay(date); err != nil {
			return nil, err
		}
		switch {
		case intoLoan > 0:
			p.amountCents = -outOfOtherAccounts // the whole payment, interest included
		case spent > 0:
			p.amountCents = spent
		default:
			continue
		}
		key := billMatchKey(p.payeeID, p.description)
		if key == "text:" {
			continue
		}
		groups[key] = append(groups[key], p)
	}
	return groups, rows.Err()
}

// paymentDays folds payments to one payee on one day into one, summed:
// rent and the portal's convenience fee charged together are one payment.
func paymentDays(payments []billPayment) []billPayment {
	out := []billPayment{}
	for _, p := range payments {
		if n := len(out); n > 0 && out[n-1].occurredOn.Equal(p.occurredOn) {
			out[n-1].amountCents += p.amountCents
			continue
		}
		out = append(out, p)
	}
	return out
}

// detectCadence finds the rhythm of a group of payments, oldest first, at
// most one a day.
func detectCadence(payments []billPayment) (cadenceSpec, bool) {
	if len(payments) < 2 {
		return cadenceSpec{}, false
	}
	gaps := make([]float64, 0, len(payments)-1)
	for i := 1; i < len(payments); i++ {
		gaps = append(gaps, payments[i].occurredOn.Sub(payments[i-1].occurredOn).Hours()/24)
	}
	for _, spec := range cadenceSpecs {
		inBand := 0
		for _, gap := range gaps {
			if gap >= spec.minimumDays && gap <= spec.maximumDays {
				inBand++
			}
		}
		// Most gaps must fit: two payments on one day, or one skipped month,
		// may not.
		if inBand >= spec.minimumInBand && float64(inBand) >= 0.6*float64(len(gaps)) {
			return spec, true
		}
	}
	return cadenceSpec{}, false
}

func medianInt64(values []int64) int64 {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func addCadence(from time.Time, spec cadenceSpec, dayOfMonth int) time.Time {
	if spec.months == 0 {
		return from.AddDate(0, 0, 7)
	}
	first := time.Date(from.Year(), from.Month()+time.Month(spec.months), 1, 0, 0, 0, 0, time.UTC)
	day := dayOfMonth
	if day <= 0 {
		day = from.Day()
	}
	return time.Date(first.Year(), first.Month(), min(day, first.AddDate(0, 1, -1).Day()), 0, 0, 0, 0, time.UTC)
}

// DetectResult counts what DetectRecurringBills did.
type DetectResult struct {
	Suggested int `json:"suggested"` // new bills waiting for the operator
	Updated   int `json:"updated"`   // known bills whose payments or amounts changed
	Ended     int `json:"ended"`     // confirmed bills with no payment for a period and its grace
	Resumed   int `json:"resumed"`   // ended bills paid again
}

// DetectRecurringBills groups payments, finds the ones with a steady rhythm
// and records them. A new one is suggested only while it is still being paid.
// A known bill keeps the operator's decision; its payments, usual amount and
// dates are refreshed, a confirmed one that stopped becomes ended, and an
// ended one paid again becomes confirmed.
func (s *Ledger) DetectRecurringBills() (DetectResult, error) {
	var result DetectResult
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	groups, err := billPayments(tx)
	if err != nil {
		return result, err
	}
	today := time.Date(s.now().Year(), s.now().Month(), s.now().Day(), 0, 0, 0, 0, time.UTC)
	now := s.unixNow()
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		payments := groups[key]
		sort.Slice(payments, func(i, j int) bool { return payments[i].occurredOn.Before(payments[j].occurredOn) })
		var operatorCadence string
		var addedByOperator int
		switch err := tx.QueryRow(`SELECT cadence, added_by_operator FROM recurring_bills WHERE match_key = $1`, key).Scan(&operatorCadence, &addedByOperator); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return result, err
		}
		var spec cadenceSpec
		var days []billPayment
		if addedByOperator == 1 {
			// The operator said what this is and how often; no rhythm test,
			// and several charges in one period are that period's payment.
			var found bool
			if spec, found = cadenceNamed(operatorCadence); !found {
				return result, fmt.Errorf("bill %s has unknown cadence %q", key, operatorCadence)
			}
			days = periodPayments(paymentDays(payments), spec)
		} else {
			var found bool
			if spec, found = detectCadence(paymentDays(payments)); !found {
				continue
			}
			// Usual amount and day from whole days: rent and the portal's fee
			// on the same day are one payment of the bill.
			days = paymentDays(payments)
		}
		recent := days
		if len(recent) > 3 {
			recent = recent[len(recent)-3:]
		}
		amounts := []int64{}
		for _, p := range recent {
			amounts = append(amounts, p.amountCents)
		}
		expected := medianInt64(amounts)
		dayOfMonth := 0
		if spec.months > 0 {
			daysOfMonth := []int64{}
			for _, p := range recent {
				daysOfMonth = append(daysOfMonth, int64(p.occurredOn.Day()))
			}
			dayOfMonth = int(medianInt64(daysOfMonth))
		}
		last := payments[len(payments)-1].occurredOn
		next := addCadence(last, spec, dayOfMonth)
		stillPaid := !today.After(next.AddDate(0, 0, spec.graceDays))
		name := payments[len(payments)-1].payeeName
		if name == "" {
			name = strings.TrimPrefix(key, "text:")
		}
		var payeeID any
		if payments[0].payeeID != "" {
			payeeID = payments[0].payeeID
		}

		var id, status string
		err := tx.QueryRow(`SELECT id, status FROM recurring_bills WHERE match_key = $1`, key).Scan(&id, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !stillPaid {
				continue
			}
			var seq int64
			if seq, id, err = mintID(tx, "recurring_bills", "recurring_bill"); err != nil {
				return result, err
			}
			if _, err := tx.Exec(`INSERT INTO recurring_bills (seq, id, match_key, name, payee_id, cadence, expected_cents, day_of_month, status, first_paid_on, last_paid_on, next_due_on, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'suggested',$9,$10,$11,$12,$13)`, seq, id, key, name, payeeID, spec.name, expected, dayOfMonth,
				payments[0].occurredOn.Format("2006-01-02"), last.Format("2006-01-02"), next.Format("2006-01-02"), now, now); err != nil {
				return result, err
			}
			result.Suggested++
		case err != nil:
			return result, err
		default:
			newStatus := status
			if status == "confirmed" && !stillPaid {
				newStatus = "ended"
				result.Ended++
			} else if status == "ended" && stillPaid {
				newStatus = "confirmed"
				result.Resumed++
			}
			if _, err := tx.Exec(`UPDATE recurring_bills SET cadence = $1, expected_cents = $2, day_of_month = $3, status = $4, first_paid_on = $5, last_paid_on = $6, next_due_on = $7, updated_at = $8 WHERE id = $9`,
				spec.name, expected, dayOfMonth, newStatus, payments[0].occurredOn.Format("2006-01-02"), last.Format("2006-01-02"), next.Format("2006-01-02"), now, id); err != nil {
				return result, err
			}
			result.Updated++
		}
		if _, err := tx.Exec(`DELETE FROM recurring_bill_payments WHERE recurring_bill_id = $1`, id); err != nil {
			return result, err
		}
		for _, p := range payments {
			if _, err := tx.Exec(`INSERT INTO recurring_bill_payments (recurring_bill_id, transaction_id) VALUES ($1,$2)
				ON CONFLICT(transaction_id) DO UPDATE SET recurring_bill_id = excluded.recurring_bill_id`, id, p.transactionID); err != nil {
				return result, err
			}
		}
	}
	return result, tx.Commit()
}

const billColumns = `b.id, b.match_key, b.name, COALESCE(b.payee_id,''), b.cadence, b.expected_cents, b.day_of_month, b.status,
	b.first_paid_on, b.last_paid_on, b.next_due_on, b.decided_by, b.decided_at, b.created_at, b.updated_at,
	(SELECT COUNT(*) FROM recurring_bill_payments rp WHERE rp.recurring_bill_id = b.id)`

func scanBill(row interface{ Scan(...any) error }) (RecurringBill, error) {
	var b RecurringBill
	err := row.Scan(&b.ID, &b.MatchKey, &b.Name, &b.PayeeID, &b.Cadence, &b.ExpectedCents, &b.DayOfMonth, &b.Status,
		&b.FirstPaidOn, &b.LastPaidOn, &b.NextDueOn, &b.DecidedBy, &b.DecidedAt, &b.CreatedAt, &b.UpdatedAt, &b.PaymentCount)
	if err != nil {
		return b, err
	}
	if spec, found := cadenceNamed(b.Cadence); found {
		b.MonthlyEquivalentCents = int64(float64(b.ExpectedCents)*spec.periodsPerYear/12 + 0.5)
	}
	return b, nil
}

// ListRecurringBills returns bills, soonest due first; status filters.
func (s *Ledger) ListRecurringBills(status string) ([]RecurringBill, error) {
	if status != "" {
		if err := requireOneOf("status", status, billStatuses); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.Query(`SELECT `+billColumns+` FROM recurring_bills b WHERE ($1 = '' OR b.status = $2) ORDER BY b.next_due_on, b.seq`, status, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecurringBill{}
	for rows.Next() {
		b, err := scanBill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetRecurringBill returns one bill with every payment, oldest first.
func (s *Ledger) GetRecurringBill(id string) (RecurringBill, error) {
	b, err := scanBill(s.db.QueryRow(`SELECT `+billColumns+` FROM recurring_bills b WHERE b.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RecurringBill{}, ErrNotFound
	}
	if err != nil {
		return RecurringBill{}, err
	}
	groups, err := billPayments(s.db)
	if err != nil {
		return RecurringBill{}, err
	}
	linked := map[string]bool{}
	rows, err := s.db.Query(`SELECT transaction_id FROM recurring_bill_payments WHERE recurring_bill_id = $1`, id)
	if err != nil {
		return RecurringBill{}, err
	}
	for rows.Next() {
		var transactionID string
		if err := rows.Scan(&transactionID); err != nil {
			rows.Close()
			return RecurringBill{}, err
		}
		linked[transactionID] = true
	}
	rows.Close()
	b.Payments = []RecurringPayment{}
	for _, group := range groups {
		for _, p := range group {
			if linked[p.transactionID] {
				b.Payments = append(b.Payments, RecurringPayment{TransactionID: p.transactionID, OccurredOn: p.occurredOn.Format("2006-01-02"), AmountCents: p.amountCents, Description: p.description})
			}
		}
	}
	sort.Slice(b.Payments, func(i, j int) bool { return b.Payments[i].OccurredOn < b.Payments[j].OccurredOn })
	for i := 1; i < len(b.Payments); i++ {
		b.Payments[i].ChangedFromPreviousCents = b.Payments[i].AmountCents - b.Payments[i-1].AmountCents
	}
	return b, nil
}

// RecurringBillPatch is the operator's change to a bill.
type RecurringBillPatch struct {
	Status    *string `json:"status"`
	Name      *string `json:"name"`
	DecidedBy string  `json:"decided_by"`
}

// PatchRecurringBill confirms, rejects or renames a bill. A status change
// needs decided_by.
func (s *Ledger) PatchRecurringBill(id string, patch RecurringBillPatch) (RecurringBill, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return RecurringBill{}, err
	}
	defer tx.Rollback()
	found, err := exists(tx, "recurring_bills", id)
	if err != nil {
		return RecurringBill{}, err
	}
	if !found {
		return RecurringBill{}, ErrNotFound
	}
	now := s.unixNow()
	if patch.Status != nil {
		if err := requireOneOf("status", *patch.Status, []string{"confirmed", "rejected"}); err != nil {
			return RecurringBill{}, err
		}
		if strings.TrimSpace(patch.DecidedBy) == "" {
			return RecurringBill{}, fmt.Errorf("%w: decided_by is required with a status: say who decided", ErrInvalid)
		}
		if _, err := tx.Exec(`UPDATE recurring_bills SET status = $1, decided_by = $2, decided_at = $3 WHERE id = $4`, *patch.Status, strings.TrimSpace(patch.DecidedBy), now, id); err != nil {
			return RecurringBill{}, err
		}
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return RecurringBill{}, fmt.Errorf("%w: name cannot be empty", ErrInvalid)
		}
		if _, err := tx.Exec(`UPDATE recurring_bills SET name = $1 WHERE id = $2`, name, id); err != nil {
			return RecurringBill{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE recurring_bills SET updated_at = $1 WHERE id = $2`, now, id); err != nil {
		return RecurringBill{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecurringBill{}, err
	}
	return s.GetRecurringBill(id)
}

// RecurringTotals is what confirmed bills cost.
type RecurringTotals struct {
	Bills int `json:"bills"` // confirmed bills still being paid
	// Expected: the usual payments spread over a month, six and twelve.
	ExpectedPerMonthCents    int64 `json:"expected_per_month_cents"`
	ExpectedPerSixMonthCents int64 `json:"expected_per_six_months_cents"`
	ExpectedPerYearCents     int64 `json:"expected_per_year_cents"`
	// Paid: what confirmed bills' payments actually came to in the last six
	// and twelve months.
	PaidLastSixMonthsCents    int64 `json:"paid_last_six_months_cents"`
	PaidLastTwelveMonthsCents int64 `json:"paid_last_twelve_months_cents"`
}

// GetRecurringTotals adds up the confirmed bills.
func (s *Ledger) GetRecurringTotals() (RecurringTotals, error) {
	var totals RecurringTotals
	bills, err := s.ListRecurringBills("confirmed")
	if err != nil {
		return totals, err
	}
	for _, b := range bills {
		totals.Bills++
		totals.ExpectedPerMonthCents += b.MonthlyEquivalentCents
	}
	totals.ExpectedPerSixMonthCents = totals.ExpectedPerMonthCents * 6
	totals.ExpectedPerYearCents = totals.ExpectedPerMonthCents * 12
	today := s.now()
	sixAgo := today.AddDate(0, -6, 0).Format("2006-01-02")
	twelveAgo := today.AddDate(-1, 0, 0).Format("2006-01-02")
	for _, b := range bills {
		full, err := s.GetRecurringBill(b.ID)
		if err != nil {
			return totals, err
		}
		for _, p := range full.Payments {
			if p.OccurredOn > twelveAgo {
				totals.PaidLastTwelveMonthsCents += p.AmountCents
			}
			if p.OccurredOn > sixAgo {
				totals.PaidLastSixMonthsCents += p.AmountCents
			}
		}
	}
	return totals, nil
}

// AddRecurringBill makes a bill of the payments a transaction belongs to: the
// same payee, or with none, the same words naming who was paid. The operator
// gives the cadence, so no rhythm is tested and several charges in one period
// count as that period's payment. status is confirmed (the operator says it
// is a bill) or suggested (an agent proposes it for the operator to decide).
// An existing bill for the same payments is refused.
func (s *Ledger) AddRecurringBill(transactionID, cadence, status, name, decidedBy string) (RecurringBill, error) {
	if _, found := cadenceNamed(cadence); !found {
		return RecurringBill{}, requireOneOf("cadence", cadence, billCadences)
	}
	if err := requireOneOf("status", status, []string{"confirmed", "suggested"}); err != nil {
		return RecurringBill{}, err
	}
	decidedBy = strings.TrimSpace(decidedBy)
	if decidedBy == "" {
		return RecurringBill{}, fmt.Errorf("%w: decided_by is required: say who named it a bill", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RecurringBill{}, err
	}
	defer tx.Rollback()
	var description, payeeID, payeeName string
	err = tx.QueryRow(`SELECT t.description, COALESCE(t.payee_id,''), COALESCE(p.name,'') FROM transactions t LEFT JOIN payees p ON p.id = t.payee_id WHERE t.id = $1`, transactionID).
		Scan(&description, &payeeID, &payeeName)
	if errors.Is(err, sql.ErrNoRows) {
		return RecurringBill{}, fmt.Errorf("%w: transaction_id %q does not exist", ErrInvalid, transactionID)
	}
	if err != nil {
		return RecurringBill{}, err
	}
	key := billMatchKey(payeeID, description)
	if key == "text:" {
		return RecurringBill{}, fmt.Errorf("%w: %s's description has no words to match other payments by", ErrInvalid, transactionID)
	}
	var existing string
	if err := tx.QueryRow(`SELECT id FROM recurring_bills WHERE match_key = $1`, key).Scan(&existing); err == nil {
		return RecurringBill{}, fmt.Errorf("%w: these payments are already bill %s; change that one", ErrConflict, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return RecurringBill{}, err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = payeeName
	}
	if name == "" {
		name = strings.TrimPrefix(key, "text:")
	}
	var payee any
	if payeeID != "" {
		payee = payeeID
	}
	seq, id, err := mintID(tx, "recurring_bills", "recurring_bill")
	if err != nil {
		return RecurringBill{}, err
	}
	now := s.unixNow()
	var decidedAt int64
	if status == "confirmed" {
		decidedAt = now
	}
	if _, err := tx.Exec(`INSERT INTO recurring_bills (seq, id, match_key, name, payee_id, cadence, expected_cents, status, first_paid_on, last_paid_on, added_by_operator, decided_by, decided_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,0,$7,'','',1,$8,$9,$10,$11)`, seq, id, key, name, payee, cadence, status, decidedBy, decidedAt, now, now); err != nil {
		return RecurringBill{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecurringBill{}, err
	}
	// The detector fills in its payments, usual amount and dates.
	if _, err := s.DetectRecurringBills(); err != nil {
		return RecurringBill{}, err
	}
	return s.GetRecurringBill(id)
}
