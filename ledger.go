package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Institution is a bank, card issuer or plan administrator.
type Institution struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Website   string `json:"website"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// CreditCardDetails are the facts only a card has.
type CreditCardDetails struct {
	Network                string `json:"network"`
	CreditLimitCents       int64  `json:"credit_limit_cents"`
	StatementDay           int    `json:"statement_day"`
	DueDay                 int    `json:"due_day"`
	PurchaseAPRBasisPoints int    `json:"purchase_apr_basis_points"`
	AnnualFeeCents         int64  `json:"annual_fee_cents"`
	Rewards                string `json:"rewards"`
	// InterestCategoryID and FeeCategoryID name where this card's interest
	// charges and fees are filed, so a report can find them by id.
	InterestCategoryID string `json:"interest_category_id"`
	FeeCategoryID      string `json:"fee_category_id"`
}

// LoanDetails are the facts only a loan has.
type LoanDetails struct {
	OriginalPrincipalCents int64  `json:"original_principal_cents"`
	APRBasisPoints         int    `json:"apr_basis_points"`
	TermMonths             int    `json:"term_months"`
	FirstPaymentOn         string `json:"first_payment_on"`
	MaturityOn             string `json:"maturity_on"`
	ScheduledPaymentCents  int64  `json:"scheduled_payment_cents"`
	SecuredBy              string `json:"secured_by"`
	// InterestCategoryID is where the interest part of each payment goes
	// when the loan schedule is applied.
	InterestCategoryID string `json:"interest_category_id"`
}

// RetirementDetails are the facts only a retirement account has.
type RetirementDetails struct {
	PlanKind     string `json:"plan_kind"`
	TaxTreatment string `json:"tax_treatment"`
	Employer     string `json:"employer"`
}

// DescriptionMarker is text that, in another account's transaction
// description, names this account.
type DescriptionMarker struct {
	ID     string `json:"id"`
	Marker string `json:"marker"`
	Note   string `json:"note"`
}

// Account is where money sits or is owed. BalanceCents is the sum of its
// non-void postings: positive for an asset, negative for a debt.
type Account struct {
	ID                 string              `json:"id"`
	InstitutionID      string              `json:"institution_id"`
	Name               string              `json:"name"`
	Kind               string              `json:"kind"`
	IsLiability        bool                `json:"is_liability"`
	LastFour           string              `json:"last_four"`
	OpenedOn           string              `json:"opened_on"`
	ClosedOn           string              `json:"closed_on"`
	OnBudget           bool                `json:"on_budget"`
	HasOwnFeed         bool                `json:"has_own_feed"`
	BalanceCents       int64               `json:"balance_cents"`
	CreditCard         *CreditCardDetails  `json:"credit_card,omitempty"`
	Loan               *LoanDetails        `json:"loan,omitempty"`
	Retirement         *RetirementDetails  `json:"retirement,omitempty"`
	DescriptionMarkers []DescriptionMarker `json:"description_markers"`
	CreatedAt          int64               `json:"created_at"`
	UpdatedAt          int64               `json:"updated_at"`
}

// AccountInput creates an account. Only the detail block matching Kind may be
// given.
type AccountInput struct {
	InstitutionID string             `json:"institution_id"`
	Name          string             `json:"name"`
	Kind          string             `json:"kind"`
	LastFour      string             `json:"last_four"`
	OpenedOn      string             `json:"opened_on"`
	ClosedOn      string             `json:"closed_on"`
	OnBudget      *bool              `json:"on_budget"`
	HasOwnFeed    *bool              `json:"has_own_feed"`
	CreditCard    *CreditCardDetails `json:"credit_card"`
	Loan          *LoanDetails       `json:"loan"`
	Retirement    *RetirementDetails `json:"retirement"`
}

// AccountPatch changes an account; a nil field is left alone. A detail block
// replaces the stored one whole.
type AccountPatch struct {
	InstitutionID *string            `json:"institution_id"`
	Name          *string            `json:"name"`
	LastFour      *string            `json:"last_four"`
	OpenedOn      *string            `json:"opened_on"`
	ClosedOn      *string            `json:"closed_on"`
	OnBudget      *bool              `json:"on_budget"`
	HasOwnFeed    *bool              `json:"has_own_feed"`
	CreditCard    *CreditCardDetails `json:"credit_card"`
	Loan          *LoanDetails       `json:"loan"`
	Retirement    *RetirementDetails `json:"retirement"`
}

// Category is where money comes from or goes.
type Category struct {
	ID         string `json:"id"`
	ParentID   string `json:"parent_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	ArchivedAt int64  `json:"archived_at"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// Payee is a normalized merchant or counterparty.
type Payee struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	DefaultCategoryID string   `json:"default_category_id"`
	Patterns          []string `json:"patterns"`
	CreatedAt         int64    `json:"created_at"`
	UpdatedAt         int64    `json:"updated_at"`
}

// Posting is one leg of a transaction: exactly one of AccountID and
// CategoryID is set.
type Posting struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	CategoryID  string `json:"category_id"`
	AmountCents int64  `json:"amount_cents"`
	Memo        string `json:"memo"`
	TaxYear     int    `json:"tax_year"`
}

// Transaction is one movement of money, as postings that sum to zero.
type Transaction struct {
	ID           string    `json:"id"`
	OccurredOn   string    `json:"occurred_on"`
	Status       string    `json:"status"`
	PayeeID      string    `json:"payee_id"`
	Description  string    `json:"description"`
	Memo         string    `json:"memo"`
	Postings     []Posting `json:"postings"`
	SourceRowIDs []string  `json:"source_row_ids"`
	// CategoryDecision is the newest decided category suggestion for this
	// transaction, when one exists: who decided and how sure the model was.
	// Nil means its category came another way (a payee rule, a hand edit) or
	// it has none yet.
	CategoryDecision *CategoryDecision `json:"category_decision"`
	CreatedAt        int64             `json:"created_at"`
	UpdatedAt        int64             `json:"updated_at"`
}

// CategoryDecision is how a transaction's category was decided through a
// suggestion.
type CategoryDecision struct {
	SuggestionID string  `json:"suggestion_id"`
	Status       string  `json:"status"` // accepted or edited
	DecidedBy    string  `json:"decided_by"`
	Confidence   float64 `json:"confidence"`
	Model        string  `json:"model"`
}

// TransactionInput creates a transaction by hand.
type TransactionInput struct {
	OccurredOn  string    `json:"occurred_on"`
	Status      string    `json:"status"`
	PayeeID     string    `json:"payee_id"`
	Description string    `json:"description"`
	Memo        string    `json:"memo"`
	Postings    []Posting `json:"postings"`
}

func validDate(field, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrInvalid, field)
		}
		return nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("%w: %s %q is not a YYYY-MM-DD date", ErrInvalid, field, value)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func exists(q queryer, table, id string) (bool, error) {
	var count int
	if err := q.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func requireExists(q queryer, table, field, id string) error {
	found, err := exists(q, table, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s %q does not exist", ErrInvalid, field, id)
	}
	return nil
}

// ---- institutions ----

// CreateInstitution adds a bank or issuer.
func (s *Store) CreateInstitution(in Institution) (Institution, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Institution{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Institution{}, err
	}
	defer tx.Rollback()
	seq, id, err := mintID(tx, "institutions", "institution")
	if err != nil {
		return Institution{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO institutions (seq, id, name, website, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		seq, id, in.Name, strings.TrimSpace(in.Website), now, now); err != nil {
		return Institution{}, err
	}
	if err := tx.Commit(); err != nil {
		return Institution{}, err
	}
	return Institution{ID: id, Name: in.Name, Website: strings.TrimSpace(in.Website), CreatedAt: now, UpdatedAt: now}, nil
}

// ListInstitutions returns every institution.
func (s *Store) ListInstitutions() ([]Institution, error) {
	rows, err := s.db.Query(`SELECT id, name, website, created_at, updated_at FROM institutions ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Institution{}
	for rows.Next() {
		var i Institution
		if err := rows.Scan(&i.ID, &i.Name, &i.Website, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ---- accounts ----

func validateAccountDetails(kind string, card *CreditCardDetails, loan *LoanDetails, retirement *RetirementDetails) error {
	if card != nil && kind != "credit_card" {
		return fmt.Errorf("%w: credit_card details given for a %s account", ErrInvalid, kind)
	}
	if loan != nil && kind != "loan" {
		return fmt.Errorf("%w: loan details given for a %s account", ErrInvalid, kind)
	}
	if retirement != nil {
		if kind != "retirement" {
			return fmt.Errorf("%w: retirement details given for a %s account", ErrInvalid, kind)
		}
		if err := requireOneOf("retirement.plan_kind", retirement.PlanKind, retirementPlanKinds); err != nil {
			return err
		}
		if err := requireOneOf("retirement.tax_treatment", retirement.TaxTreatment, taxTreatments); err != nil {
			return err
		}
	}
	if loan != nil {
		if err := validDate("loan.first_payment_on", loan.FirstPaymentOn, false); err != nil {
			return err
		}
		if err := validDate("loan.maturity_on", loan.MaturityOn, false); err != nil {
			return err
		}
	}
	return nil
}

func validLastFour(value string) error {
	if value == "" {
		return nil
	}
	if len(value) != 4 || strings.Trim(value, "0123456789") != "" {
		return fmt.Errorf("%w: last_four %q must be four digits (never store a full number)", ErrInvalid, value)
	}
	return nil
}

// CreateAccount adds an account. on_budget defaults to true for checking,
// savings, cash and credit cards; has_own_feed defaults to true.
func (s *Store) CreateAccount(in AccountInput) (Account, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Account{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if err := requireOneOf("kind", in.Kind, accountKinds); err != nil {
		return Account{}, err
	}
	if err := validLastFour(in.LastFour); err != nil {
		return Account{}, err
	}
	if err := validDate("opened_on", in.OpenedOn, false); err != nil {
		return Account{}, err
	}
	if err := validDate("closed_on", in.ClosedOn, false); err != nil {
		return Account{}, err
	}
	if err := validateAccountDetails(in.Kind, in.CreditCard, in.Loan, in.Retirement); err != nil {
		return Account{}, err
	}
	if in.Kind == "retirement" && in.Retirement == nil {
		return Account{}, fmt.Errorf("%w: a retirement account needs retirement.plan_kind and retirement.tax_treatment", ErrInvalid)
	}
	onBudget := slices.Contains([]string{"checking", "savings", "cash", "credit_card"}, in.Kind)
	if in.OnBudget != nil {
		onBudget = *in.OnBudget
	}
	hasOwnFeed := true
	if in.HasOwnFeed != nil {
		hasOwnFeed = *in.HasOwnFeed
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	var institutionID any
	if in.InstitutionID != "" {
		if err := requireExists(tx, "institutions", "institution_id", in.InstitutionID); err != nil {
			return Account{}, err
		}
		institutionID = in.InstitutionID
	}
	seq, id, err := mintID(tx, "accounts", "account")
	if err != nil {
		return Account{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO accounts (seq, id, institution_id, name, kind, last_four, opened_on, closed_on, on_budget, has_own_feed, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		seq, id, institutionID, in.Name, in.Kind, in.LastFour, in.OpenedOn, in.ClosedOn, boolInt(onBudget), boolInt(hasOwnFeed), now, now); err != nil {
		return Account{}, err
	}
	if err := writeAccountDetails(tx, id, in.CreditCard, in.Loan, in.Retirement); err != nil {
		return Account{}, err
	}
	if in.LastFour != "" {
		if err := insertMarker(tx, id, in.LastFour, "last four", now); err != nil {
			return Account{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.GetAccount(id)
}

func writeAccountDetails(tx queryer, accountID string, card *CreditCardDetails, loan *LoanDetails, retirement *RetirementDetails) error {
	if card != nil {
		categories := map[string]any{}
		for field, id := range map[string]string{"credit_card.interest_category_id": card.InterestCategoryID, "credit_card.fee_category_id": card.FeeCategoryID} {
			if id == "" {
				categories[field] = nil
				continue
			}
			var kind string
			if err := tx.QueryRow(`SELECT kind FROM categories WHERE id = ?`, id).Scan(&kind); err != nil || kind != "expense" {
				return fmt.Errorf("%w: %s %q is not an expense category", ErrInvalid, field, id)
			}
			categories[field] = id
		}
		if _, err := tx.Exec(`INSERT INTO credit_card_details (account_id, network, credit_limit_cents, statement_day, due_day, purchase_apr_basis_points, annual_fee_cents, rewards, interest_category_id, fee_category_id)
			VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET network=excluded.network, credit_limit_cents=excluded.credit_limit_cents,
			statement_day=excluded.statement_day, due_day=excluded.due_day, purchase_apr_basis_points=excluded.purchase_apr_basis_points,
			annual_fee_cents=excluded.annual_fee_cents, rewards=excluded.rewards, interest_category_id=excluded.interest_category_id, fee_category_id=excluded.fee_category_id`,
			accountID, card.Network, card.CreditLimitCents, card.StatementDay, card.DueDay, card.PurchaseAPRBasisPoints, card.AnnualFeeCents, card.Rewards,
			categories["credit_card.interest_category_id"], categories["credit_card.fee_category_id"]); err != nil {
			return err
		}
	}
	if loan != nil {
		var interestCategory any
		if loan.InterestCategoryID != "" {
			var kind string
			if err := tx.QueryRow(`SELECT kind FROM categories WHERE id = ?`, loan.InterestCategoryID).Scan(&kind); err != nil || kind != "expense" {
				return fmt.Errorf("%w: loan.interest_category_id %q is not an expense category", ErrInvalid, loan.InterestCategoryID)
			}
			interestCategory = loan.InterestCategoryID
		}
		if _, err := tx.Exec(`INSERT INTO loan_details (account_id, original_principal_cents, apr_basis_points, term_months, first_payment_on, maturity_on, scheduled_payment_cents, secured_by, interest_category_id)
			VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET original_principal_cents=excluded.original_principal_cents,
			apr_basis_points=excluded.apr_basis_points, term_months=excluded.term_months, first_payment_on=excluded.first_payment_on,
			maturity_on=excluded.maturity_on, scheduled_payment_cents=excluded.scheduled_payment_cents, secured_by=excluded.secured_by,
			interest_category_id=excluded.interest_category_id`,
			accountID, loan.OriginalPrincipalCents, loan.APRBasisPoints, loan.TermMonths, loan.FirstPaymentOn, loan.MaturityOn, loan.ScheduledPaymentCents, loan.SecuredBy, interestCategory); err != nil {
			return err
		}
	}
	if retirement != nil {
		if _, err := tx.Exec(`INSERT INTO retirement_details (account_id, plan_kind, tax_treatment, employer) VALUES (?,?,?,?)
			ON CONFLICT(account_id) DO UPDATE SET plan_kind=excluded.plan_kind, tax_treatment=excluded.tax_treatment, employer=excluded.employer`,
			accountID, retirement.PlanKind, retirement.TaxTreatment, retirement.Employer); err != nil {
			return err
		}
	}
	return nil
}

func insertMarker(tx queryer, accountID, marker, note string, now int64) error {
	marker = strings.TrimSpace(marker)
	if len(marker) < 4 {
		return fmt.Errorf("%w: a description marker needs at least 4 characters, so it cannot match by accident", ErrInvalid)
	}
	seq, id, err := mintID(tx, "account_description_markers", "account_description_marker")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO account_description_markers (seq, id, account_id, marker, note, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(account_id, marker) DO NOTHING`, seq, id, accountID, marker, note, now)
	return err
}

// AddDescriptionMarker records text that names accountID in other accounts'
// rows: an old card number, a loan reference.
func (s *Store) AddDescriptionMarker(accountID, marker, note string) (Account, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	found, err := exists(tx, "accounts", accountID)
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, ErrNotFound
	}
	if err := insertMarker(tx, accountID, marker, note, s.unixNow()); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.GetAccount(accountID)
}

// DeleteDescriptionMarker removes one marker from an account. Transfers
// already paired or moved by it stay as they are.
func (s *Store) DeleteDescriptionMarker(accountID, markerID string) (Account, error) {
	result, err := s.db.Exec(`DELETE FROM account_description_markers WHERE id = ? AND account_id = ?`, markerID, accountID)
	if err != nil {
		return Account{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Account{}, ErrNotFound
	}
	return s.GetAccount(accountID)
}

// PatchAccount changes the fields given.
func (s *Store) PatchAccount(id string, patch AccountPatch) (Account, error) {
	current, err := s.GetAccount(id)
	if err != nil {
		return Account{}, err
	}
	if err := validateAccountDetails(current.Kind, patch.CreditCard, patch.Loan, patch.Retirement); err != nil {
		return Account{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	set := func(column string, value any) error {
		_, err := tx.Exec(`UPDATE accounts SET `+column+` = ? WHERE id = ?`, value, id)
		return err
	}
	if patch.InstitutionID != nil {
		var value any
		if *patch.InstitutionID != "" {
			if err := requireExists(tx, "institutions", "institution_id", *patch.InstitutionID); err != nil {
				return Account{}, err
			}
			value = *patch.InstitutionID
		}
		if err := set("institution_id", value); err != nil {
			return Account{}, err
		}
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return Account{}, fmt.Errorf("%w: name cannot be empty", ErrInvalid)
		}
		if err := set("name", name); err != nil {
			return Account{}, err
		}
	}
	if patch.LastFour != nil {
		if err := validLastFour(*patch.LastFour); err != nil {
			return Account{}, err
		}
		if err := set("last_four", *patch.LastFour); err != nil {
			return Account{}, err
		}
		if *patch.LastFour != "" {
			if err := insertMarker(tx, id, *patch.LastFour, "last four", s.unixNow()); err != nil {
				return Account{}, err
			}
		}
	}
	for column, value := range map[string]*string{"opened_on": patch.OpenedOn, "closed_on": patch.ClosedOn} {
		if value == nil {
			continue
		}
		if err := validDate(column, *value, false); err != nil {
			return Account{}, err
		}
		if err := set(column, *value); err != nil {
			return Account{}, err
		}
	}
	if patch.OnBudget != nil {
		if err := set("on_budget", boolInt(*patch.OnBudget)); err != nil {
			return Account{}, err
		}
	}
	if patch.HasOwnFeed != nil {
		if err := set("has_own_feed", boolInt(*patch.HasOwnFeed)); err != nil {
			return Account{}, err
		}
	}
	if err := writeAccountDetails(tx, id, patch.CreditCard, patch.Loan, patch.Retirement); err != nil {
		return Account{}, err
	}
	if err := set("updated_at", s.unixNow()); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.GetAccount(id)
}

const accountColumns = `a.id, COALESCE(a.institution_id,''), a.name, a.kind, a.last_four, a.opened_on, a.closed_on, a.on_budget, a.has_own_feed, a.created_at, a.updated_at,
	COALESCE((SELECT SUM(p.amount_cents) FROM postings p JOIN transactions t ON t.id = p.transaction_id WHERE p.account_id = a.id AND t.status != 'void'), 0)`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var onBudget, hasOwnFeed int
	if err := row.Scan(&a.ID, &a.InstitutionID, &a.Name, &a.Kind, &a.LastFour, &a.OpenedOn, &a.ClosedOn, &onBudget, &hasOwnFeed, &a.CreatedAt, &a.UpdatedAt, &a.BalanceCents); err != nil {
		return Account{}, err
	}
	a.OnBudget = onBudget == 1
	a.HasOwnFeed = hasOwnFeed == 1
	a.IsLiability = slices.Contains(liabilityAccountKinds, a.Kind)
	return a, nil
}

func (s *Store) loadAccountExtras(a *Account) error {
	var card CreditCardDetails
	err := s.db.QueryRow(`SELECT network, credit_limit_cents, statement_day, due_day, purchase_apr_basis_points, annual_fee_cents, rewards,
		COALESCE(interest_category_id,''), COALESCE(fee_category_id,'') FROM credit_card_details WHERE account_id = ?`, a.ID).
		Scan(&card.Network, &card.CreditLimitCents, &card.StatementDay, &card.DueDay, &card.PurchaseAPRBasisPoints, &card.AnnualFeeCents, &card.Rewards, &card.InterestCategoryID, &card.FeeCategoryID)
	if err == nil {
		a.CreditCard = &card
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var loan LoanDetails
	err = s.db.QueryRow(`SELECT original_principal_cents, apr_basis_points, term_months, first_payment_on, maturity_on, scheduled_payment_cents, secured_by, COALESCE(interest_category_id,'') FROM loan_details WHERE account_id = ?`, a.ID).
		Scan(&loan.OriginalPrincipalCents, &loan.APRBasisPoints, &loan.TermMonths, &loan.FirstPaymentOn, &loan.MaturityOn, &loan.ScheduledPaymentCents, &loan.SecuredBy, &loan.InterestCategoryID)
	if err == nil {
		a.Loan = &loan
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var retirement RetirementDetails
	err = s.db.QueryRow(`SELECT plan_kind, tax_treatment, employer FROM retirement_details WHERE account_id = ?`, a.ID).
		Scan(&retirement.PlanKind, &retirement.TaxTreatment, &retirement.Employer)
	if err == nil {
		a.Retirement = &retirement
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := s.db.Query(`SELECT id, marker, note FROM account_description_markers WHERE account_id = ? ORDER BY seq`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.DescriptionMarkers = []DescriptionMarker{}
	for rows.Next() {
		var m DescriptionMarker
		if err := rows.Scan(&m.ID, &m.Marker, &m.Note); err != nil {
			return err
		}
		a.DescriptionMarkers = append(a.DescriptionMarkers, m)
	}
	return rows.Err()
}

// GetAccount returns one account with its balance and details.
func (s *Store) GetAccount(id string) (Account, error) {
	a, err := scanAccount(s.db.QueryRow(`SELECT `+accountColumns+` FROM accounts a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	if err := s.loadAccountExtras(&a); err != nil {
		return Account{}, err
	}
	return a, nil
}

// ListAccounts returns every account with its balance and details.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountColumns + ` FROM accounts a ORDER BY a.seq`)
	if err != nil {
		return nil, err
	}
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.loadAccountExtras(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- categories ----

// CreateCategory adds a category. Roles are the store's; a caller cannot set one.
func (s *Store) CreateCategory(in Category) (Category, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Category{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if in.Role != "" {
		return Category{}, fmt.Errorf("%w: role is set by the store, not by a caller", ErrInvalid)
	}
	if err := requireOneOf("kind", in.Kind, []string{"expense", "income", "equity"}); err != nil {
		return Category{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Category{}, err
	}
	defer tx.Rollback()
	var parentID any
	if in.ParentID != "" {
		var parentKind string
		if err := tx.QueryRow(`SELECT kind FROM categories WHERE id = ?`, in.ParentID).Scan(&parentKind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Category{}, fmt.Errorf("%w: parent_id %q does not exist", ErrInvalid, in.ParentID)
			}
			return Category{}, err
		}
		if parentKind != in.Kind {
			return Category{}, fmt.Errorf("%w: a %s category cannot sit under a %s one", ErrInvalid, in.Kind, parentKind)
		}
		parentID = in.ParentID
	}
	seq, id, err := mintID(tx, "categories", "category")
	if err != nil {
		return Category{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO categories (seq, id, parent_id, name, kind, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		seq, id, parentID, in.Name, in.Kind, now, now); err != nil {
		return Category{}, err
	}
	if err := tx.Commit(); err != nil {
		return Category{}, err
	}
	return s.GetCategory(id)
}

func scanCategory(row interface{ Scan(...any) error }) (Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.ParentID, &c.Name, &c.Kind, &c.Role, &c.ArchivedAt, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

const categoryColumns = `id, COALESCE(parent_id,''), name, kind, COALESCE(role,''), archived_at, created_at, updated_at`

// GetCategory returns one category.
func (s *Store) GetCategory(id string) (Category, error) {
	c, err := scanCategory(s.db.QueryRow(`SELECT `+categoryColumns+` FROM categories WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	return c, err
}

// ListCategories returns every category.
func (s *Store) ListCategories() ([]Category, error) {
	rows, err := s.db.Query(`SELECT ` + categoryColumns + ` FROM categories ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Category{}
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- payees ----

// CreatePayee adds a payee and the bank texts that name it.
func (s *Store) CreatePayee(in Payee) (Payee, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Payee{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Payee{}, err
	}
	defer tx.Rollback()
	var defaultCategory any
	if in.DefaultCategoryID != "" {
		if err := requireExists(tx, "categories", "default_category_id", in.DefaultCategoryID); err != nil {
			return Payee{}, err
		}
		defaultCategory = in.DefaultCategoryID
	}
	seq, id, err := mintID(tx, "payees", "payee")
	if err != nil {
		return Payee{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO payees (seq, id, name, default_category_id, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		seq, id, in.Name, defaultCategory, now, now); err != nil {
		return Payee{}, err
	}
	for _, pattern := range in.Patterns {
		if err := insertPayeePattern(tx, id, pattern, now); err != nil {
			return Payee{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Payee{}, err
	}
	return s.GetPayee(id)
}

func insertPayeePattern(tx queryer, payeeID, pattern string, now int64) error {
	pattern = strings.ToUpper(strings.TrimSpace(pattern))
	if len(pattern) < 3 {
		return fmt.Errorf("%w: a payee pattern needs at least 3 characters", ErrInvalid)
	}
	var owner string
	err := tx.QueryRow(`SELECT payee_id FROM payee_match_rules WHERE pattern = ?`, pattern).Scan(&owner)
	if err == nil {
		if owner == payeeID {
			return nil
		}
		return fmt.Errorf("%w: pattern %q already names payee %s", ErrConflict, pattern, owner)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	seq, id, err := mintID(tx, "payee_match_rules", "payee_match_rule")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO payee_match_rules (seq, id, payee_id, pattern, created_at) VALUES (?,?,?,?,?)`, seq, id, payeeID, pattern, now)
	return err
}

// GetPayee returns one payee with its patterns.
func (s *Store) GetPayee(id string) (Payee, error) {
	var p Payee
	err := s.db.QueryRow(`SELECT id, name, COALESCE(default_category_id,''), created_at, updated_at FROM payees WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.DefaultCategoryID, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Payee{}, ErrNotFound
	}
	if err != nil {
		return Payee{}, err
	}
	rows, err := s.db.Query(`SELECT pattern FROM payee_match_rules WHERE payee_id = ? ORDER BY seq`, id)
	if err != nil {
		return Payee{}, err
	}
	defer rows.Close()
	p.Patterns = []string{}
	for rows.Next() {
		var pattern string
		if err := rows.Scan(&pattern); err != nil {
			return Payee{}, err
		}
		p.Patterns = append(p.Patterns, pattern)
	}
	return p, rows.Err()
}

// ListPayees returns every payee.
func (s *Store) ListPayees() ([]Payee, error) {
	rows, err := s.db.Query(`SELECT id FROM payees ORDER BY seq`)
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
	out := []Payee{}
	for _, id := range ids {
		p, err := s.GetPayee(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

type payeeRule struct {
	payeeID           string
	defaultCategoryID string
	pattern           string
}

func loadPayeeRules(q queryer) ([]payeeRule, error) {
	rows, err := q.Query(`SELECT r.payee_id, COALESCE(p.default_category_id,''), r.pattern FROM payee_match_rules r JOIN payees p ON p.id = r.payee_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []payeeRule
	for rows.Next() {
		var r payeeRule
		if err := rows.Scan(&r.payeeID, &r.defaultCategoryID, &r.pattern); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	// Longest pattern first, so the most specific rule wins.
	slices.SortFunc(rules, func(a, b payeeRule) int { return len(b.pattern) - len(a.pattern) })
	return rules, rows.Err()
}

func matchPayee(rules []payeeRule, description string) (payeeRule, bool) {
	upper := strings.ToUpper(description)
	for _, r := range rules {
		if strings.Contains(upper, r.pattern) {
			return r, true
		}
	}
	return payeeRule{}, false
}

// ApplyPayeeRulesResult counts what ApplyPayeeRules changed.
type ApplyPayeeRulesResult struct {
	PayeesSet         int `json:"payees_set"`
	CategoriesChanged int `json:"categories_changed"`
}

// ApplyPayeeRules gives every transaction without a payee the payee its
// description matches, and moves a transaction whose only category posting is
// Uncategorized to that payee's default category. A category the operator
// chose is never overwritten.
func (s *Store) ApplyPayeeRules() (ApplyPayeeRulesResult, error) {
	var result ApplyPayeeRulesResult
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	rules, err := loadPayeeRules(tx)
	if err != nil {
		return result, err
	}
	uncategorized, err := categoryIDForRole(tx, "uncategorized")
	if err != nil {
		return result, err
	}
	rows, err := tx.Query(`SELECT id, description, COALESCE(payee_id,'') FROM transactions WHERE status != 'void'`)
	if err != nil {
		return result, err
	}
	type candidate struct{ id, description, payeeID string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.description, &c.payeeID); err != nil {
			rows.Close()
			return result, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	now := s.unixNow()
	for _, c := range candidates {
		rule, matched := matchPayee(rules, c.description)
		if !matched {
			continue
		}
		if c.payeeID == "" {
			if _, err := tx.Exec(`UPDATE transactions SET payee_id = ?, updated_at = ? WHERE id = ?`, rule.payeeID, now, c.id); err != nil {
				return result, err
			}
			result.PayeesSet++
		} else if c.payeeID != rule.payeeID {
			continue // someone chose another payee; leave it
		}
		if rule.defaultCategoryID == "" {
			continue
		}
		changed, err := tx.Exec(`UPDATE postings SET category_id = ? WHERE transaction_id = ? AND category_id = ?
			AND (SELECT COUNT(*) FROM postings WHERE transaction_id = ? AND category_id IS NOT NULL) = 1`,
			rule.defaultCategoryID, c.id, uncategorized, c.id)
		if err != nil {
			return result, err
		}
		if n, _ := changed.RowsAffected(); n > 0 {
			result.CategoriesChanged++
		}
	}
	return result, tx.Commit()
}

// ---- transactions ----

// insertTransaction validates and writes a transaction and its postings.
func (s *Store) insertTransaction(tx queryer, in TransactionInput) (string, error) {
	if err := validDate("occurred_on", in.OccurredOn, true); err != nil {
		return "", err
	}
	if in.Status == "" {
		in.Status = "posted"
	}
	if err := requireOneOf("status", in.Status, transactionStatuses); err != nil {
		return "", err
	}
	if err := validatePostings(tx, in.Postings); err != nil {
		return "", err
	}
	var payeeID any
	if in.PayeeID != "" {
		if err := requireExists(tx, "payees", "payee_id", in.PayeeID); err != nil {
			return "", err
		}
		payeeID = in.PayeeID
	}
	seq, id, err := mintID(tx, "transactions", "transaction")
	if err != nil {
		return "", err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO transactions (seq, id, occurred_on, status, payee_id, description, memo, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		seq, id, in.OccurredOn, in.Status, payeeID, in.Description, in.Memo, now, now); err != nil {
		return "", err
	}
	for _, p := range in.Postings {
		if err := insertPosting(tx, id, p); err != nil {
			return "", err
		}
	}
	return id, nil
}

func validatePostings(tx queryer, postings []Posting) error {
	if len(postings) < 2 {
		return fmt.Errorf("%w: a transaction needs at least two postings", ErrInvalid)
	}
	var sum int64
	for i, p := range postings {
		if (p.AccountID == "") == (p.CategoryID == "") {
			return fmt.Errorf("%w: posting %d must name exactly one of account_id and category_id", ErrInvalid, i)
		}
		if p.AccountID != "" {
			if err := requireExists(tx, "accounts", fmt.Sprintf("posting %d account_id", i), p.AccountID); err != nil {
				return err
			}
		} else if err := requireExists(tx, "categories", fmt.Sprintf("posting %d category_id", i), p.CategoryID); err != nil {
			return err
		}
		sum += p.AmountCents
	}
	if sum != 0 {
		return fmt.Errorf("%w: postings sum to %d cents, not 0", ErrInvalid, sum)
	}
	return nil
}

func insertPosting(tx queryer, transactionID string, p Posting) error {
	seq, id, err := mintID(tx, "postings", "posting")
	if err != nil {
		return err
	}
	var accountID, categoryID any
	if p.AccountID != "" {
		accountID = p.AccountID
	}
	if p.CategoryID != "" {
		categoryID = p.CategoryID
	}
	_, err = tx.Exec(`INSERT INTO postings (seq, id, transaction_id, account_id, category_id, amount_cents, memo, tax_year) VALUES (?,?,?,?,?,?,?,?)`,
		seq, id, transactionID, accountID, categoryID, p.AmountCents, p.Memo, p.TaxYear)
	return err
}

// CreateTransaction records a transaction by hand.
func (s *Store) CreateTransaction(in TransactionInput) (Transaction, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Transaction{}, err
	}
	defer tx.Rollback()
	id, err := s.insertTransaction(tx, in)
	if err != nil {
		return Transaction{}, err
	}
	if err := tx.Commit(); err != nil {
		return Transaction{}, err
	}
	return s.GetTransaction(id)
}

// GetTransaction returns one transaction with its postings and source rows.
// The id of a transaction folded into another by the transfer matcher
// answers with the one it was folded into.
func (s *Store) GetTransaction(id string) (Transaction, error) {
	var into string
	err := s.db.QueryRow(`SELECT into_transaction_id FROM transaction_merges WHERE merged_transaction_id = ?`, id).Scan(&into)
	if err == nil {
		id = into
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Transaction{}, err
	}
	var t Transaction
	err = s.db.QueryRow(`SELECT id, occurred_on, status, COALESCE(payee_id,''), description, memo, created_at, updated_at FROM transactions WHERE id = ?`, id).
		Scan(&t.ID, &t.OccurredOn, &t.Status, &t.PayeeID, &t.Description, &t.Memo, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Transaction{}, ErrNotFound
	}
	if err != nil {
		return Transaction{}, err
	}
	if err := s.loadTransactionParts(&t); err != nil {
		return Transaction{}, err
	}
	return t, nil
}

func (s *Store) loadTransactionParts(t *Transaction) error {
	rows, err := s.db.Query(`SELECT id, COALESCE(account_id,''), COALESCE(category_id,''), amount_cents, memo, tax_year FROM postings WHERE transaction_id = ? ORDER BY seq`, t.ID)
	if err != nil {
		return err
	}
	t.Postings = []Posting{}
	for rows.Next() {
		var p Posting
		if err := rows.Scan(&p.ID, &p.AccountID, &p.CategoryID, &p.AmountCents, &p.Memo, &p.TaxYear); err != nil {
			rows.Close()
			return err
		}
		t.Postings = append(t.Postings, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var decision CategoryDecision
	err = s.db.QueryRow(`SELECT id, status, decided_by, confidence, model FROM category_suggestions
		WHERE transaction_id = ? AND status IN ('accepted','edited') ORDER BY decided_at DESC, seq DESC LIMIT 1`, t.ID).
		Scan(&decision.SuggestionID, &decision.Status, &decision.DecidedBy, &decision.Confidence, &decision.Model)
	switch {
	case err == nil:
		t.CategoryDecision = &decision
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	rows, err = s.db.Query(`SELECT import_row_id FROM transaction_sources WHERE transaction_id = ? ORDER BY import_row_id`, t.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	t.SourceRowIDs = []string{}
	for rows.Next() {
		var rowID string
		if err := rows.Scan(&rowID); err != nil {
			return err
		}
		t.SourceRowIDs = append(t.SourceRowIDs, rowID)
	}
	return rows.Err()
}

// TransactionFilter narrows ListTransactions. Empty fields do not filter.
type TransactionFilter struct {
	AccountID  string
	CategoryID string
	// IncludeSubcategories widens CategoryID to every category under it.
	IncludeSubcategories bool
	Month                string // YYYY-MM
	Query                string // substring of description or memo
	Uncategorized        bool
	Limit                int
	Offset               int
}

// ListTransactions returns transactions newest first.
func (s *Store) ListTransactions(filter TransactionFilter) ([]Transaction, error) {
	where := []string{"1=1"}
	var args []any
	if filter.AccountID != "" {
		where = append(where, `t.id IN (SELECT transaction_id FROM postings WHERE account_id = ?)`)
		args = append(args, filter.AccountID)
	}
	if filter.CategoryID != "" && filter.IncludeSubcategories {
		where = append(where, `t.id IN (SELECT transaction_id FROM postings WHERE category_id IN (
			WITH RECURSIVE tree(id) AS (SELECT ? UNION ALL SELECT c.id FROM categories c JOIN tree ON c.parent_id = tree.id) SELECT id FROM tree))`)
		args = append(args, filter.CategoryID)
	} else if filter.CategoryID != "" {
		where = append(where, `t.id IN (SELECT transaction_id FROM postings WHERE category_id = ?)`)
		args = append(args, filter.CategoryID)
	}
	if filter.Uncategorized {
		where = append(where, `t.id IN (SELECT p.transaction_id FROM postings p JOIN categories c ON c.id = p.category_id WHERE c.role = 'uncategorized')`)
	}
	if filter.Month != "" {
		if _, err := time.Parse("2006-01", filter.Month); err != nil {
			return nil, fmt.Errorf("%w: month %q is not YYYY-MM", ErrInvalid, filter.Month)
		}
		where = append(where, `substr(t.occurred_on, 1, 7) = ?`)
		args = append(args, filter.Month)
	}
	if filter.Query != "" {
		where = append(where, `(t.description LIKE ? OR t.memo LIKE ?)`)
		like := "%" + filter.Query + "%"
		args = append(args, like, like)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args = append(args, limit, filter.Offset)
	rows, err := s.db.Query(`SELECT t.id, t.occurred_on, t.status, COALESCE(t.payee_id,''), t.description, t.memo, t.created_at, t.updated_at
		FROM transactions t WHERE `+strings.Join(where, " AND ")+` ORDER BY t.occurred_on DESC, t.seq DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	out := []Transaction{}
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.OccurredOn, &t.Status, &t.PayeeID, &t.Description, &t.Memo, &t.CreatedAt, &t.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.loadTransactionParts(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CategorySplit is one category's share of a transaction.
type CategorySplit struct {
	CategoryID  string `json:"category_id"`
	AmountCents int64  `json:"amount_cents"`
	Memo        string `json:"memo"`
	TaxYear     int    `json:"tax_year"`
}

// SetTransactionCategories replaces every category posting of a transaction.
// The splits must sum to what the category side summed to before, so the
// accounts' side never changes.
func (s *Store) SetTransactionCategories(id string, splits []CategorySplit) (Transaction, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Transaction{}, err
	}
	defer tx.Rollback()
	found, err := exists(tx, "transactions", id)
	if err != nil {
		return Transaction{}, err
	}
	if !found {
		return Transaction{}, ErrNotFound
	}
	if len(splits) == 0 {
		return Transaction{}, fmt.Errorf("%w: give at least one category", ErrInvalid)
	}
	var categorySideSum int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(amount_cents),0) FROM postings WHERE transaction_id = ? AND category_id IS NOT NULL`, id).Scan(&categorySideSum); err != nil {
		return Transaction{}, err
	}
	var newSum int64
	for i, split := range splits {
		if err := requireExists(tx, "categories", fmt.Sprintf("split %d category_id", i), split.CategoryID); err != nil {
			return Transaction{}, err
		}
		newSum += split.AmountCents
	}
	if newSum != categorySideSum {
		return Transaction{}, fmt.Errorf("%w: the splits sum to %d cents; the category side must stay %d", ErrInvalid, newSum, categorySideSum)
	}
	if _, err := tx.Exec(`DELETE FROM postings WHERE transaction_id = ? AND category_id IS NOT NULL`, id); err != nil {
		return Transaction{}, err
	}
	for _, split := range splits {
		if err := insertPosting(tx, id, Posting{CategoryID: split.CategoryID, AmountCents: split.AmountCents, Memo: split.Memo, TaxYear: split.TaxYear}); err != nil {
			return Transaction{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE transactions SET updated_at = ? WHERE id = ?`, s.unixNow(), id); err != nil {
		return Transaction{}, err
	}
	if err := tx.Commit(); err != nil {
		return Transaction{}, err
	}
	return s.GetTransaction(id)
}

// TransactionPatch changes a transaction's own fields; nil leaves a field alone.
type TransactionPatch struct {
	Status  *string `json:"status"`
	PayeeID *string `json:"payee_id"`
	Memo    *string `json:"memo"`
}

// PatchTransaction changes status, payee or memo.
func (s *Store) PatchTransaction(id string, patch TransactionPatch) (Transaction, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Transaction{}, err
	}
	defer tx.Rollback()
	found, err := exists(tx, "transactions", id)
	if err != nil {
		return Transaction{}, err
	}
	if !found {
		return Transaction{}, ErrNotFound
	}
	if patch.Status != nil {
		if err := requireOneOf("status", *patch.Status, transactionStatuses); err != nil {
			return Transaction{}, err
		}
		if _, err := tx.Exec(`UPDATE transactions SET status = ? WHERE id = ?`, *patch.Status, id); err != nil {
			return Transaction{}, err
		}
	}
	if patch.PayeeID != nil {
		var value any
		if *patch.PayeeID != "" {
			if err := requireExists(tx, "payees", "payee_id", *patch.PayeeID); err != nil {
				return Transaction{}, err
			}
			value = *patch.PayeeID
		}
		if _, err := tx.Exec(`UPDATE transactions SET payee_id = ? WHERE id = ?`, value, id); err != nil {
			return Transaction{}, err
		}
	}
	if patch.Memo != nil {
		if _, err := tx.Exec(`UPDATE transactions SET memo = ? WHERE id = ?`, *patch.Memo, id); err != nil {
			return Transaction{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE transactions SET updated_at = ? WHERE id = ?`, s.unixNow(), id); err != nil {
		return Transaction{}, err
	}
	if err := tx.Commit(); err != nil {
		return Transaction{}, err
	}
	return s.GetTransaction(id)
}
