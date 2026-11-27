package budget

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// budgetViews are the two ways GET /budget reads the same assignments.
var budgetViews = []string{"limit", "envelope"}

func balanceAsOf(q queryer, accountID, asOf string) (int64, error) {
	var balance int64
	err := q.QueryRow(`SELECT COALESCE(SUM(p.amount_cents),0) FROM postings p JOIN transactions t ON t.id = p.transaction_id
		WHERE p.account_id = $1 AND t.status != 'void' AND t.occurred_on <= $2`, accountID, asOf).Scan(&balance)
	return balance, err
}

// BalanceAssertion is a balance a statement or a site reported, and how far
// the ledger is from it.
type BalanceAssertion struct {
	ID                 string `json:"id"`
	AccountID          string `json:"account_id"`
	AsOf               string `json:"as_of"`
	BalanceCents       int64  `json:"balance_cents"`
	Source             string `json:"source"`
	LedgerBalanceCents int64  `json:"ledger_balance_cents"`
	DifferenceCents    int64  `json:"difference_cents"` // ledger minus reported; 0 = the books agree
	CreatedAt          int64  `json:"created_at"`
}

// BalanceAssertionInput records a reported balance.
type BalanceAssertionInput struct {
	AccountID    string `json:"account_id"`
	AsOf         string `json:"as_of"`
	BalanceCents int64  `json:"balance_cents"`
	Source       string `json:"source"`
}

func validateAssertion(q queryer, in BalanceAssertionInput) error {
	if err := validDate("as_of", in.AsOf, true); err != nil {
		return err
	}
	if strings.TrimSpace(in.Source) == "" {
		return fmt.Errorf("%w: source is required: say where the balance came from", ErrInvalid)
	}
	return requireExists(q, "accounts", "account_id", in.AccountID)
}

func insertAssertion(tx queryer, in BalanceAssertionInput, now int64) (string, error) {
	seq, id, err := mintID(tx, "balance_assertions", "balance_assertion")
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO balance_assertions (seq, id, account_id, as_of, balance_cents, source, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		seq, id, in.AccountID, in.AsOf, in.BalanceCents, strings.TrimSpace(in.Source), now)
	return id, err
}

// CreateBalanceAssertion records a reported balance and answers how far the
// ledger is from it.
func (s *Ledger) CreateBalanceAssertion(in BalanceAssertionInput) (BalanceAssertion, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return BalanceAssertion{}, err
	}
	defer tx.Rollback()
	if err := validateAssertion(tx, in); err != nil {
		return BalanceAssertion{}, err
	}
	id, err := insertAssertion(tx, in, s.unixNow())
	if err != nil {
		return BalanceAssertion{}, err
	}
	if err := tx.Commit(); err != nil {
		return BalanceAssertion{}, err
	}
	return s.getBalanceAssertion(id)
}

func (s *Ledger) getBalanceAssertion(id string) (BalanceAssertion, error) {
	var a BalanceAssertion
	err := s.db.QueryRow(`SELECT id, account_id, as_of, balance_cents, source, created_at FROM balance_assertions WHERE id = $1`, id).
		Scan(&a.ID, &a.AccountID, &a.AsOf, &a.BalanceCents, &a.Source, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BalanceAssertion{}, ErrNotFound
	}
	if err != nil {
		return BalanceAssertion{}, err
	}
	if a.LedgerBalanceCents, err = balanceAsOf(s.db, a.AccountID, a.AsOf); err != nil {
		return BalanceAssertion{}, err
	}
	a.DifferenceCents = a.LedgerBalanceCents - a.BalanceCents
	return a, nil
}

// ListBalanceAssertions returns every assertion, each checked against the
// ledger as it stands now.
func (s *Ledger) ListBalanceAssertions(accountID string) ([]BalanceAssertion, error) {
	query := `SELECT id FROM balance_assertions`
	var args []any
	if accountID != "" {
		query += ` WHERE account_id = $1`
		args = append(args, accountID)
	}
	rows, err := s.db.Query(query+` ORDER BY as_of, seq`, args...)
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
	out := []BalanceAssertion{}
	for _, id := range ids {
		a, err := s.getBalanceAssertion(id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// SetOpeningBalance makes the account's ledger agree with a reported balance
// by giving it an opening-balance transaction, dated the day before its first
// other posting (or as_of when it has none), against the Opening balances
// category. An existing opening-balance transaction for the account is
// replaced. The reported balance is also kept as an assertion.
func (s *Ledger) SetOpeningBalance(in BalanceAssertionInput) (BalanceAssertion, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return BalanceAssertion{}, err
	}
	defer tx.Rollback()
	if err := validateAssertion(tx, in); err != nil {
		return BalanceAssertion{}, err
	}
	openingCategory, err := categoryIDForRole(tx, "opening_balances")
	if err != nil {
		return BalanceAssertion{}, err
	}
	if _, err := tx.Exec(`DELETE FROM transactions WHERE id IN (
		SELECT pa.transaction_id FROM postings pa JOIN postings pc ON pc.transaction_id = pa.transaction_id
		WHERE pa.account_id = $1 AND pc.category_id = $2)`, in.AccountID, openingCategory); err != nil {
		return BalanceAssertion{}, err
	}
	ledger, err := balanceAsOf(tx, in.AccountID, in.AsOf)
	if err != nil {
		return BalanceAssertion{}, err
	}
	var firstDate string
	if err := tx.QueryRow(`SELECT COALESCE(MIN(t.occurred_on),'') FROM postings p JOIN transactions t ON t.id = p.transaction_id
		WHERE p.account_id = $1 AND t.status != 'void'`, in.AccountID).Scan(&firstDate); err != nil {
		return BalanceAssertion{}, err
	}
	openingDate := in.AsOf
	if firstDate != "" && firstDate <= in.AsOf {
		first, err := time.Parse("2006-01-02", firstDate)
		if err != nil {
			return BalanceAssertion{}, err
		}
		openingDate = first.AddDate(0, 0, -1).Format("2006-01-02")
	}
	if opening := in.BalanceCents - ledger; opening != 0 {
		if _, err := s.insertTransaction(tx, TransactionInput{
			OccurredOn:  openingDate,
			Description: "Opening balance, from " + strings.TrimSpace(in.Source),
			Postings: []Posting{
				{AccountID: in.AccountID, AmountCents: opening},
				{CategoryID: openingCategory, AmountCents: -opening},
			},
		}); err != nil {
			return BalanceAssertion{}, err
		}
	}
	id, err := insertAssertion(tx, in, s.unixNow())
	if err != nil {
		return BalanceAssertion{}, err
	}
	if err := tx.Commit(); err != nil {
		return BalanceAssertion{}, err
	}
	return s.getBalanceAssertion(id)
}

// CategoryTotal is one category's postings in a month.
type CategoryTotal struct {
	CategoryID   string `json:"category_id"`
	ParentID     string `json:"parent_id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	TotalCents   int64  `json:"total_cents"` // spending positive, income negative
	Transactions int    `json:"transactions"`
}

func validMonth(month string) error {
	if _, err := time.Parse("2006-01", month); err != nil {
		return fmt.Errorf("%w: month %q is not YYYY-MM", ErrInvalid, month)
	}
	return nil
}

// SpendingByCategory totals each category's postings in one month.
func (s *Ledger) SpendingByCategory(month string) ([]CategoryTotal, error) {
	if err := validMonth(month); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT c.id, COALESCE(c.parent_id,''), c.name, c.kind, SUM(p.amount_cents), COUNT(DISTINCT t.id)
		FROM postings p JOIN transactions t ON t.id = p.transaction_id JOIN categories c ON c.id = p.category_id
		WHERE t.status != 'void' AND substr(t.occurred_on,1,7) = $1
		GROUP BY c.id ORDER BY c.kind, SUM(p.amount_cents) DESC, c.seq`, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CategoryTotal{}
	for rows.Next() {
		var c CategoryTotal
		if err := rows.Scan(&c.CategoryID, &c.ParentID, &c.Name, &c.Kind, &c.TotalCents, &c.Transactions); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BudgetAssignment is the amount given to one category for one month.
type BudgetAssignment struct {
	ID          string `json:"id"`
	CategoryID  string `json:"category_id"`
	Month       string `json:"month"`
	AmountCents int64  `json:"amount_cents"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// SetBudgetAssignment creates or replaces the amount for a category and month.
func (s *Ledger) SetBudgetAssignment(in BudgetAssignment) (BudgetAssignment, error) {
	if err := validMonth(in.Month); err != nil {
		return BudgetAssignment{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return BudgetAssignment{}, err
	}
	defer tx.Rollback()
	var kind string
	if err := tx.QueryRow(`SELECT kind FROM categories WHERE id = $1`, in.CategoryID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BudgetAssignment{}, fmt.Errorf("%w: category_id %q does not exist", ErrInvalid, in.CategoryID)
		}
		return BudgetAssignment{}, err
	}
	if kind != "expense" {
		return BudgetAssignment{}, fmt.Errorf("%w: only expense categories take a budget; %s is %s", ErrInvalid, in.CategoryID, kind)
	}
	now := s.unixNow()
	var id string
	err = tx.QueryRow(`SELECT id FROM budget_assignments WHERE category_id = $1 AND month = $2`, in.CategoryID, in.Month).Scan(&id)
	switch {
	case err == nil:
		if _, err := tx.Exec(`UPDATE budget_assignments SET amount_cents = $1, updated_at = $2 WHERE id = $3`, in.AmountCents, now, id); err != nil {
			return BudgetAssignment{}, err
		}
	case errors.Is(err, sql.ErrNoRows):
		var seq int64
		if seq, id, err = mintID(tx, "budget_assignments", "budget_assignment"); err != nil {
			return BudgetAssignment{}, err
		}
		if _, err := tx.Exec(`INSERT INTO budget_assignments (seq, id, category_id, month, amount_cents, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			seq, id, in.CategoryID, in.Month, in.AmountCents, now, now); err != nil {
			return BudgetAssignment{}, err
		}
	default:
		return BudgetAssignment{}, err
	}
	if err := tx.Commit(); err != nil {
		return BudgetAssignment{}, err
	}
	var out BudgetAssignment
	err = s.db.QueryRow(`SELECT id, category_id, month, amount_cents, created_at, updated_at FROM budget_assignments WHERE id = $1`, id).
		Scan(&out.ID, &out.CategoryID, &out.Month, &out.AmountCents, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

// BudgetLine is one expense category in a budget view.
type BudgetLine struct {
	CategoryID    string `json:"category_id"`
	ParentID      string `json:"parent_id"`
	Name          string `json:"name"`
	AssignedCents int64  `json:"assigned_cents"` // this month
	SpentCents    int64  `json:"spent_cents"`    // this month
	// AvailableCents: in the limit view, assigned minus spent this month; in
	// the envelope view, everything ever assigned minus everything ever spent
	// through this month, so what is left rolls forward.
	AvailableCents int64 `json:"available_cents"`
}

// Budget is one month read one way.
type Budget struct {
	Month string       `json:"month"`
	View  string       `json:"view"`
	Lines []BudgetLine `json:"lines"`
	// ReadyToAssignCents, envelope view only: income and opening balances that
	// reached on-budget accounts through this month, minus everything assigned
	// through this month. Negative means more was assigned than came in.
	ReadyToAssignCents *int64 `json:"ready_to_assign_cents,omitempty"`
	// UncategorizedCents is this month's spending nobody sorted yet; neither
	// view can count it against a category until someone does.
	UncategorizedCents int64 `json:"uncategorized_cents"`
}

// GetBudget reads one month's assignments in the limit or the envelope view.
func (s *Ledger) GetBudget(month, view string) (Budget, error) {
	if err := validMonth(month); err != nil {
		return Budget{}, err
	}
	if err := requireOneOf("view", view, budgetViews); err != nil {
		return Budget{}, err
	}
	budget := Budget{Month: month, View: view, Lines: []BudgetLine{}}
	monthEnd := month + "-31" // dates compare as text; no month has a 32nd
	rows, err := s.db.Query(`SELECT c.id, COALESCE(c.parent_id,''), c.name,
		COALESCE((SELECT amount_cents FROM budget_assignments WHERE category_id = c.id AND month = $1), 0),
		COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN transactions t ON t.id = p.transaction_id
			WHERE p.category_id = c.id AND t.status != 'void' AND substr(t.occurred_on,1,7) = $2), 0),
		COALESCE((SELECT SUM(amount_cents) FROM budget_assignments WHERE category_id = c.id AND month <= $3), 0),
		COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN transactions t ON t.id = p.transaction_id
			WHERE p.category_id = c.id AND t.status != 'void' AND t.occurred_on <= $4), 0)
		FROM categories c WHERE c.kind = 'expense' AND c.archived_at = 0 ORDER BY c.seq`, month, month, month, monthEnd)
	if err != nil {
		return Budget{}, err
	}
	for rows.Next() {
		var line BudgetLine
		var assignedToDate, spentToDate int64
		if err := rows.Scan(&line.CategoryID, &line.ParentID, &line.Name, &line.AssignedCents, &line.SpentCents, &assignedToDate, &spentToDate); err != nil {
			rows.Close()
			return Budget{}, err
		}
		if view == "envelope" {
			line.AvailableCents = assignedToDate - spentToDate
		} else {
			line.AvailableCents = line.AssignedCents - line.SpentCents
		}
		budget.Lines = append(budget.Lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Budget{}, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(p.amount_cents),0) FROM postings p JOIN transactions t ON t.id = p.transaction_id
		JOIN categories c ON c.id = p.category_id WHERE c.role = 'uncategorized' AND t.status != 'void' AND substr(t.occurred_on,1,7) = $1`, month).
		Scan(&budget.UncategorizedCents); err != nil {
		return Budget{}, err
	}
	if view == "envelope" {
		var inflow, assigned int64
		// Income and equity postings are negative when money comes in, so the
		// inflow is their sum negated. Only transactions that touch an
		// on-budget account count: interest inside the IRA is not spendable.
		if err := s.db.QueryRow(`SELECT COALESCE(-SUM(p.amount_cents),0) FROM postings p JOIN transactions t ON t.id = p.transaction_id
			JOIN categories c ON c.id = p.category_id
			WHERE c.kind IN ('income','equity') AND t.status != 'void' AND t.occurred_on <= $1
			AND EXISTS (SELECT 1 FROM postings pa JOIN accounts a ON a.id = pa.account_id WHERE pa.transaction_id = t.id AND a.on_budget = 1)`, monthEnd).
			Scan(&inflow); err != nil {
			return Budget{}, err
		}
		if err := s.db.QueryRow(`SELECT COALESCE(SUM(amount_cents),0) FROM budget_assignments WHERE month <= $1`, month).Scan(&assigned); err != nil {
			return Budget{}, err
		}
		ready := inflow - assigned
		budget.ReadyToAssignCents = &ready
	}
	return budget, nil
}

// Link points one of this store's records at a record another store owns.
type Link struct {
	ID         string `json:"id"`
	FromID     string `json:"from_id"`
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Note       string `json:"note"`
	CreatedAt  int64  `json:"created_at"`
}

// linkableTables maps each id prefix this store hands out to its table, so a
// link can only start at a record that exists.
var linkableTables = map[string]string{
	"account_":     "accounts",
	"transaction_": "transactions",
	"payee_":       "payees",
	"category_":    "categories",
	"statement_":   "statements",
	// A line links to the possession it bought.
	"marketplace_order_":      "marketplace_orders",
	"marketplace_order_line_": "marketplace_order_lines",
}

// CreateLink records a pointer from one of this store's records to another
// store's record, by that store's id.
func (s *Ledger) CreateLink(in Link) (Link, error) {
	table := ""
	for prefix, candidate := range linkableTables {
		if strings.HasPrefix(in.FromID, prefix) && strings.Trim(strings.TrimPrefix(in.FromID, prefix), "0123456789") == "" {
			table = candidate
		}
	}
	if table == "" {
		return Link{}, fmt.Errorf("%w: from_id %q is not an id this store links from", ErrInvalid, in.FromID)
	}
	if strings.TrimSpace(in.EntityType) == "" || strings.TrimSpace(in.EntityRef) == "" {
		return Link{}, fmt.Errorf("%w: entity_type and entity_ref are required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	if err := requireExists(tx, table, "from_id", in.FromID); err != nil {
		return Link{}, err
	}
	seq, id, err := mintID(tx, "links", "link")
	if err != nil {
		return Link{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO links (seq, id, from_id, entity_type, entity_ref, note, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		seq, id, in.FromID, in.EntityType, in.EntityRef, in.Note, now); err != nil {
		if isUniqueViolation(err) {
			return Link{}, fmt.Errorf("%w: that link exists", ErrConflict)
		}
		return Link{}, err
	}
	if err := tx.Commit(); err != nil {
		return Link{}, err
	}
	in.ID, in.CreatedAt = id, now
	return in, nil
}

// ListLinks returns the links from one record.
func (s *Ledger) ListLinks(fromID string) ([]Link, error) {
	rows, err := s.db.Query(`SELECT id, from_id, entity_type, entity_ref, note, created_at FROM links WHERE from_id = $1 ORDER BY seq`, fromID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.FromID, &l.EntityType, &l.EntityRef, &l.Note, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MonthCategoryTotal is one category's postings in one month.
type MonthCategoryTotal struct {
	Month        string `json:"month"` // YYYY-MM
	CategoryID   string `json:"category_id"`
	Kind         string `json:"kind"`
	TotalCents   int64  `json:"total_cents"` // spending positive, income negative
	Transactions int    `json:"transactions"`
}

// SpendingByMonth totals each category's postings in every month from
// fromMonth to toMonth inclusive (YYYY-MM; either may be empty for no bound).
// Transfers between the operator's accounts have no category posting, so they
// never appear.
func (s *Ledger) SpendingByMonth(fromMonth, toMonth string) ([]MonthCategoryTotal, error) {
	for name, value := range map[string]string{"from": fromMonth, "to": toMonth} {
		if value != "" {
			if err := validMonth(value); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	rows, err := s.db.Query(`SELECT substr(t.occurred_on,1,7) AS month, c.id, c.kind, SUM(p.amount_cents), COUNT(DISTINCT t.id)
		FROM postings p JOIN transactions t ON t.id = p.transaction_id JOIN categories c ON c.id = p.category_id
		WHERE t.status != 'void' AND ($1 = '' OR substr(t.occurred_on,1,7) >= $2) AND ($3 = '' OR substr(t.occurred_on,1,7) <= $4)
		GROUP BY month, c.id ORDER BY month, c.seq`, fromMonth, fromMonth, toMonth, toMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MonthCategoryTotal{}
	for rows.Next() {
		var m MonthCategoryTotal
		if err := rows.Scan(&m.Month, &m.CategoryID, &m.Kind, &m.TotalCents, &m.Transactions); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
