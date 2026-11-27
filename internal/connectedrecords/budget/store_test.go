package budget

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustAccount(t *testing.T, s *Ledger, in AccountInput) Account {
	t.Helper()
	a, err := s.CreateAccount(in)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustImport(t *testing.T, s *Ledger, source, accountID, content string) ImportBatch {
	t.Helper()
	b, err := s.Import(ImportRequest{Source: source, AccountID: accountID, FileName: "test.csv"}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func balance(t *testing.T, s *Ledger, id string) int64 {
	t.Helper()
	a, err := s.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	return a.BalanceCents
}

const wellsFargoHeader = `"DATE","DESCRIPTION","AMOUNT","CHECK #","STATUS"` + "\n"

func TestParseCentsReadsAmountsExactly(t *testing.T) {
	for input, want := range map[string]int64{"-18.09": -1809, "1234.56": 123456, "0.26": 26, "-35": -3500, "1,234.5": 123450, "(12.00)": -1200, "$5.01": 501} {
		got, err := parseCents(input)
		if err != nil || got != want {
			t.Errorf("parseCents(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, bad := range []string{"", "1.234", "abc"} {
		if _, err := parseCents(bad); err == nil {
			t.Errorf("parseCents(%q) succeeded", bad)
		}
	}
}

func TestImportMakesOneBalancedTransactionPerRowAndCatchesOverlaps(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking", LastFour: "1111"})
	first := wellsFargoHeader +
		`"11/03/2026","COFFEE SHOP","-3.00","","Posted"` + "\n" +
		`"11/03/2026","COFFEE SHOP","-3.00","","Posted"` + "\n" +
		`"11/01/2026","EMPLOYER PAYROLL","2000.00","","Posted"` + "\n"
	batch := mustImport(t, s, "wells_fargo_csv", checking.ID, first)
	if batch.Outcomes["created"] != 3 {
		t.Fatalf("outcomes = %v, want 3 created (two identical coffees are two purchases)", batch.Outcomes)
	}
	if got := balance(t, s, checking.ID); got != 199400 {
		t.Fatalf("balance = %d, want 199400", got)
	}
	if _, err := s.Import(ImportRequest{Source: "wells_fargo_csv", AccountID: checking.ID}, strings.NewReader(first)); !errors.Is(err, ErrConflict) {
		t.Fatalf("importing the same file twice = %v, want a conflict", err)
	}
	overlap := wellsFargoHeader +
		`"11/04/2026","GROCERY","-50.00","","Posted"` + "\n" +
		`"11/03/2026","COFFEE SHOP","-3.00","","Posted"` + "\n" +
		`"11/03/2026","COFFEE SHOP","-3.00","","Posted"` + "\n"
	second := mustImport(t, s, "wells_fargo_csv", checking.ID, overlap)
	if second.Outcomes["created"] != 1 || second.Outcomes["duplicate"] != 2 {
		t.Fatalf("overlap outcomes = %v, want 1 created and 2 duplicate", second.Outcomes)
	}
	if got := balance(t, s, checking.ID); got != 194400 {
		t.Fatalf("balance after overlap = %d, want 194400", got)
	}
}

func TestTransfersPairOnlyWhenOneSideNamesTheOtherAccount(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking", LastFour: "1111"})
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card", LastFour: "2222"})
	if _, err := s.AddDescriptionMarker(card.ID, "XXXXXXXXXXXX3333", "the card's number before it was reissued"); err != nil {
		t.Fatal(err)
	}
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"11/02/2026","ONLINE TRANSFER REF #IB1 TO WELLS FARGO ACTIVE CASH VISA CARD XXXXXXXXXXXX2222 ON 11/02/26","-1234.56","","Posted"`+"\n"+
		`"12/18/2024","ONLINE TRANSFER REF #IB2 TO WELLS FARGO ACTIVE CASH VISA CARD XXXXXXXXXXXX3333 ON 12/17/24","-2000.00","","Posted"`+"\n"+
		`"12/17/2024","ZELLE TO LANDLORD RENT","-2000.00","","Posted"`+"\n")
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+
		`"11/03/2026","ONLINE PAYMENT THANK YOU","1234.56",,"Posted"`+"\n"+
		`"12/17/2024","ONLINE PAYMENT THANK YOU","2000.00",,"Posted"`+"\n"+
		`"12/16/2024","GROCERY","-2000.00",,"Posted"`+"\n")
	result, err := s.MatchTransfers()
	if err != nil {
		t.Fatal(err)
	}
	if result.Paired != 2 {
		t.Fatalf("paired = %d, want 2 (the rent must not pair with a card payment)", result.Paired)
	}
	spending, err := s.SpendingByCategory("2024-12")
	if err != nil {
		t.Fatal(err)
	}
	// Left uncategorized: the rent (+2000) and the grocery (+2000). Nothing
	// from either card payment.
	if len(spending) != 1 || spending[0].TotalCents != 400000 || spending[0].Transactions != 2 {
		t.Fatalf("December 2024 spending = %+v, want only the rent and the grocery", spending)
	}
	// The rent is dated closer to the card payment than the real transfer is,
	// so only the marker test keeps the rent out of the pair.
	rent, err := s.ListTransactions(TransactionFilter{Query: "LANDLORD", Uncategorized: true})
	if err != nil || len(rent) != 1 {
		t.Fatalf("the rent is no longer an uncategorized purchase: %+v, %v", rent, err)
	}
	november, err := s.ListTransactions(TransactionFilter{Month: "2026-11"})
	if err != nil {
		t.Fatal(err)
	}
	if len(november) != 1 || len(november[0].Postings) != 2 || len(november[0].SourceRowIDs) != 2 {
		t.Fatalf("November = %+v, want one transfer with two account postings and two source rows", november)
	}
	for _, p := range november[0].Postings {
		if p.AccountID == "" {
			t.Fatalf("the transfer kept a category posting: %+v", p)
		}
	}
	// Payments +1234.56 and +2000.00, grocery -2000.00.
	if got := balance(t, s, card.ID); got != 123456 {
		t.Fatalf("card balance = %d, want 123456", got)
	}
	batches, err := s.ListImportBatches()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoImportBatch(batches[0].ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("undoing a batch whose rows are paired = %v, want a conflict", err)
	}
}

func TestFoldedTransactionIDStillResolves(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card", LastFour: "4444"})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+`"10/22/2026","WF Credit Card AUTO PAY 261022 90000000004444","-54.00","","Posted"`+"\n")
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+`"10/22/2026","AUTOMATIC PAYMENT - THANK YOU","54.00",,"Posted"`+"\n")
	cardSide, err := s.ListTransactions(TransactionFilter{AccountID: card.ID})
	if err != nil || len(cardSide) != 1 {
		t.Fatalf("card side = %v, %v", cardSide, err)
	}
	foldedID := cardSide[0].ID
	if _, err := s.MatchTransfers(); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTransaction(foldedID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == foldedID || len(got.SourceRowIDs) != 2 {
		t.Fatalf("GetTransaction(%s) = %+v, want the checking-side transaction holding both rows", foldedID, got)
	}
}

func TestRowsNamingAnAccountWithoutAFeedMoveOntoIt(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	noFeed := false
	loan := mustAccount(t, s, AccountInput{Name: "Car loan", Kind: "loan", HasOwnFeed: &noFeed})
	if _, err := s.AddDescriptionMarker(loan.ID, "77770001", "loan reference in autopay rows"); err != nil {
		t.Fatal(err)
	}
	loan, err := s.PatchAccount(loan.ID, AccountPatch{LastFour: ptr("7777")})
	if err != nil {
		t.Fatal(err)
	}
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"10/13/2026","WF LOAN/LINE AUTO PAY 261009 XXXXXXX77770001","-412.50","","Posted"`+"\n"+
		// The loan's last four inside a card-terminal number: four digits
		// alone must not move a purchase onto the loan.
		`"05/30/2026","PURCHASE AUTHORIZED ON 05/30 LUNCH SHOP S306157777490 CARD 1234","-12.00","","Posted"`+"\n")
	result, err := s.MatchTransfers()
	if err != nil {
		t.Fatal(err)
	}
	if result.AssignedWithoutPartner != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got := balance(t, s, loan.ID); got != 41250 {
		t.Fatalf("loan balance = %d, want 41250 (the debt shrank)", got)
	}
}

func ptr[T any](value T) *T { return &value }

func TestTransactionsMustBalance(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	categories, _ := s.ListCategories()
	_, err := s.CreateTransaction(TransactionInput{OccurredOn: "2026-11-01", Postings: []Posting{
		{AccountID: checking.ID, AmountCents: -500},
		{CategoryID: categories[0].ID, AmountCents: 400},
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unbalanced transaction = %v, want invalid", err)
	}
}

func TestOpeningBalanceMakesTheLedgerAgreeWithAStatement(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"11/03/2026","GROCERY","-50.00","","Posted"`+"\n"+
		`"10/07/2024","PAYROLL","100.00","","Posted"`+"\n")
	assertion, err := s.SetOpeningBalance(BalanceAssertionInput{AccountID: checking.ID, AsOf: "2026-11-06", BalanceCents: 350000, Source: "Rocket Money 2026-11-06"})
	if err != nil {
		t.Fatal(err)
	}
	if assertion.DifferenceCents != 0 || balance(t, s, checking.ID) != 350000 {
		t.Fatalf("assertion = %+v, balance %d", assertion, balance(t, s, checking.ID))
	}
	// Setting it again replaces the opening transaction rather than adding a second.
	if _, err := s.SetOpeningBalance(BalanceAssertionInput{AccountID: checking.ID, AsOf: "2026-11-06", BalanceCents: 500000, Source: "statement"}); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, s, checking.ID); got != 500000 {
		t.Fatalf("balance after a second opening balance = %d, want 500000", got)
	}
	opening, _ := s.ListTransactions(TransactionFilter{Query: "Opening balance"})
	if len(opening) != 1 || opening[0].OccurredOn != "2024-10-06" {
		t.Fatalf("opening transactions = %+v, want one dated 2024-10-06", opening)
	}
}

func TestInspiraSweepRowsChangeNothingAndTheRestMatchTheBalance(t *testing.T) {
	s := openTestStore(t)
	ira := mustAccount(t, s, AccountInput{Name: "Rollover IRA", Kind: "retirement", Retirement: &RetirementDetails{PlanKind: "ira", TaxTreatment: "traditional"}})
	batch := mustImport(t, s, "inspira_csv", ira.ID, "Trade Date,Symbol,Settle Date,Cusip,Transaction Type,Description,Comment,Quantity,Price,Amount,Transaction Fee,SEC Fee,Activity Type\n"+
		"11/03/2026 04:20:06,,11/02/2026 06:00:00,MMARP5555,Other Cash Activity,Cash Sweep Account,,0.26,1,-0.26,0,0,SWBUY\n"+
		"10/31/2026 22:36:27,,11/02/2026 06:00:00,MMARP5555,Dividends and Interest,Cash Sweep Account,INTEREST,0.26,1,0.26,0,0,SWCREDIT\n"+
		"01/22/2025 03:36:36,,,,Fees and Charges,Annual Maintenance Fee,,,,-35,0,0,FEEPDACT\n"+
		"01/21/2025 17:42:26,,,,Deposits and Withdrawals,Wire Deposit - Rollover Contribution,ROLLOVER FROM AN EMPLOYER PLAN,,,900.00,0,0,WIREDEP\n")
	if batch.Outcomes["skipped_internal"] != 1 || batch.Outcomes["created"] != 3 {
		t.Fatalf("outcomes = %v", batch.Outcomes)
	}
	if got := balance(t, s, ira.ID); got != 86526 {
		t.Fatalf("IRA balance = %d, want 86526", got)
	}
}

func TestBudgetViewsReadTheSameAssignmentsTwoWays(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	groceries, err := s.CreateCategory(Category{Name: "Groceries", Kind: "expense"})
	if err != nil {
		t.Fatal(err)
	}
	salary, err := s.CreateCategory(Category{Name: "Salary", Kind: "income"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []TransactionInput{
		{OccurredOn: "2026-09-01", Postings: []Posting{{AccountID: checking.ID, AmountCents: 300000}, {CategoryID: salary.ID, AmountCents: -300000}}},
		{OccurredOn: "2026-09-10", Postings: []Posting{{AccountID: checking.ID, AmountCents: -30000}, {CategoryID: groceries.ID, AmountCents: 30000}}},
		{OccurredOn: "2026-10-10", Postings: []Posting{{AccountID: checking.ID, AmountCents: -45000}, {CategoryID: groceries.ID, AmountCents: 45000}}},
	} {
		if _, err := s.CreateTransaction(in); err != nil {
			t.Fatal(err)
		}
	}
	for _, month := range []string{"2026-09", "2026-10"} {
		if _, err := s.SetBudgetAssignment(BudgetAssignment{CategoryID: groceries.ID, Month: month, AmountCents: 40000}); err != nil {
			t.Fatal(err)
		}
	}
	limit, err := s.GetBudget("2026-10", "limit")
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := s.GetBudget("2026-10", "envelope")
	if err != nil {
		t.Fatal(err)
	}
	line := func(b Budget) BudgetLine {
		for _, l := range b.Lines {
			if l.CategoryID == groceries.ID {
				return l
			}
		}
		t.Fatalf("no groceries line in %+v", b)
		return BudgetLine{}
	}
	// Limit: October alone, 400 - 450 = -50 over.
	if got := line(limit).AvailableCents; got != -5000 {
		t.Errorf("limit available = %d, want -5000", got)
	}
	// Envelope: September's unspent 100 rolls in: 800 - 750 = +50.
	if got := line(envelope).AvailableCents; got != 5000 {
		t.Errorf("envelope available = %d, want 5000", got)
	}
	// Ready to assign: 3000 income - 800 assigned.
	if envelope.ReadyToAssignCents == nil || *envelope.ReadyToAssignCents != 220000 {
		t.Errorf("ready to assign = %v, want 220000", envelope.ReadyToAssignCents)
	}
	if limit.ReadyToAssignCents != nil {
		t.Errorf("the limit view reported ready to assign")
	}
}

func TestPayeeRulesNameAndSortOnlyUnsortedTransactions(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"11/03/2026","PURCHASE AUTHORIZED ON 11/02 WINCO FOODS #123 ANYTOWN","-80.00","","Posted"`+"\n")
	groceries, _ := s.CreateCategory(Category{Name: "Groceries", Kind: "expense"})
	if _, err := s.CreatePayee(Payee{Name: "WinCo", DefaultCategoryID: groceries.ID, Patterns: []string{"winco foods"}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyPayeeRules()
	if err != nil {
		t.Fatal(err)
	}
	if result.PayeesSet != 1 || result.CategoriesChanged != 1 {
		t.Fatalf("result = %+v", result)
	}
	spending, _ := s.SpendingByCategory("2026-11")
	if len(spending) != 1 || spending[0].CategoryID != groceries.ID {
		t.Fatalf("spending = %+v, want all of it under Groceries", spending)
	}
}

func TestRefusals(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateAccount(AccountInput{Name: "Card", Kind: "credit_card", LastFour: "4111111111111111"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a full card number = %v, want invalid", err)
	}
	if _, err := s.CreateAccount(AccountInput{Name: "IRA", Kind: "retirement"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a retirement account without plan details = %v, want invalid", err)
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, testStoreOf(t, s))
	request := asCaller(httptest.NewRequest("POST", "/accounts", strings.NewReader(`{"name":"Checking","kind":"checking","last4":"1234"}`)), testOwnerPrincipalID)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("an unknown JSON key = %d, want 400", recorder.Code)
	}
}

func TestRowsBeforeAnAccountsFeedStartsMoveOntoItAndBlockAnOverlappingImport(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card", LastFour: "2222"})
	if _, err := s.AddDescriptionMarker(card.ID, "XXXXXXXXXXXX3333", "old number"); err != nil {
		t.Fatal(err)
	}
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"12/18/2024","ONLINE TRANSFER TO VISA CARD XXXXXXXXXXXX3333","-2000.00","","Posted"`+"\n"+
		`"11/02/2026","ONLINE TRANSFER TO VISA CARD XXXXXXXXXXXX2222","-500.00","","Posted"`+"\n")
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+
		`"11/02/2026","ONLINE PAYMENT THANK YOU","500.00",,"Posted"`+"\n"+
		`"08/25/2025","GROCERY","-40.00",,"Posted"`+"\n")
	result, err := s.MatchTransfers()
	if err != nil {
		t.Fatal(err)
	}
	if result.Paired != 1 || result.AssignedWithoutPartner != 1 {
		t.Fatalf("result = %+v, want the 2026 payment paired and the 2024 one moved onto the card", result)
	}
	older := wellsFargoHeader + `"12/17/2024","ONLINE PAYMENT THANK YOU","2000.00",,"Posted"` + "\n"
	if _, err := s.Import(ImportRequest{Source: "wells_fargo_csv", AccountID: card.ID}, strings.NewReader(older)); !errors.Is(err, ErrConflict) {
		t.Fatalf("importing card rows that cover the moved transfer = %v, want a conflict", err)
	}
	moved, err := s.ListTransactions(TransactionFilter{Month: "2024-12"})
	if err != nil || len(moved) != 1 {
		t.Fatalf("December 2024 = %v, %v", moved, err)
	}
	if _, err := s.UndoTransferWithoutPartner(moved[0].ID); err != nil {
		t.Fatal(err)
	}
	mustImport(t, s, "wells_fargo_csv", card.ID, older)
	result, err = s.MatchTransfers()
	if err != nil {
		t.Fatal(err)
	}
	if result.Paired != 1 || result.AssignedWithoutPartner != 0 {
		t.Fatalf("after undo and import, result = %+v, want the 2024 payment paired with its card row", result)
	}
	// Card: -40 grocery, +500 and +2000 payments, counted once each.
	if got := balance(t, s, card.ID); got != 246000 {
		t.Fatalf("card balance = %d, want 246000", got)
	}
}

func TestACategorizedRowCanBecomeATransferToAnAccount(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	noFeed := false
	loan := mustAccount(t, s, AccountInput{Name: "Loan", Kind: "loan", HasOwnFeed: &noFeed})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+`"08/28/2025","DEPOSIT IN BRANCH","5000.00","","Posted"`+"\n")
	deposit, _ := s.ListTransactions(TransactionFilter{})
	if _, err := s.MoveCategoryPostingToAccount(deposit[0].ID, loan.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a move with no reason = %v, want invalid", err)
	}
	moved, err := s.MoveCategoryPostingToAccount(deposit[0].ID, loan.ID, "the loan paid out into checking")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range moved.Postings {
		if p.CategoryID != "" {
			t.Fatalf("a category posting is left: %+v", moved.Postings)
		}
	}
	if got := balance(t, s, loan.ID); got != -500000 {
		t.Fatalf("loan balance = %d, want -500000", got)
	}
	if spending, _ := s.SpendingByCategory("2025-08"); len(spending) != 0 {
		t.Fatalf("the loan still counts as income or spending: %+v", spending)
	}
	if _, err := s.MoveCategoryPostingToAccount(deposit[0].ID, loan.ID, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("moving a transfer again = %v, want a conflict", err)
	}
	if _, err := s.UndoTransferWithoutPartner(deposit[0].ID); err != nil {
		t.Fatalf("the move cannot be undone: %v", err)
	}
}

func TestACardNamesItsInterestAndFeeCategoriesByID(t *testing.T) {
	s := openTestStore(t)
	interest, _ := s.CreateCategory(Category{Name: "Card interest", Kind: "expense"})
	salary, _ := s.CreateCategory(Category{Name: "Salary", Kind: "income"})
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card", CreditCard: &CreditCardDetails{Network: "visa", InterestCategoryID: interest.ID}})
	if card.CreditCard.InterestCategoryID != interest.ID || card.CreditCard.FeeCategoryID != "" {
		t.Fatalf("card details = %+v", card.CreditCard)
	}
	if _, err := s.PatchAccount(card.ID, AccountPatch{CreditCard: &CreditCardDetails{FeeCategoryID: salary.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an income category as the fee category = %v, want invalid", err)
	}
}
