package budgetstore

import (
	"net/http"
	"testing"
)

// The routes the other tests never reach still run on Postgres: each read
// and write below is one the standalone store ran on SQLite.
func TestTheLessUsedReadsAndWritesRunOnPostgres(t *testing.T) {
	store := newTestStore(t)
	s := openLedger(t, store, testOwnerPrincipalID)
	institution, err := s.CreateInstitution(Institution{Name: "Example Bank", Website: "https://bank.example"})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListInstitutions(); err != nil || len(list) != 1 {
		t.Fatalf("institutions = %d, %v", len(list), err)
	}
	interestCategory, err := s.CreateCategory(Category{Name: "Interest", Kind: "expense"})
	if err != nil {
		t.Fatal(err)
	}
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card", LastFour: "4444", InstitutionID: institution.ID})
	name, lastFour, on, off := "Renamed card", "5555", true, false
	patched, err := s.PatchAccount(card.ID, AccountPatch{Name: &name, LastFour: &lastFour, OnBudget: &off, HasOwnFeed: &on, InstitutionID: &institution.ID,
		CreditCard: &CreditCardDetails{Network: "visa", CreditLimitCents: 500000, InterestCategoryID: interestCategory.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Name != name || patched.OnBudget || patched.CreditCard == nil || len(patched.DescriptionMarkers) != 2 {
		t.Fatalf("patched account = %+v", patched)
	}
	if patched, err = s.DeleteDescriptionMarker(card.ID, patched.DescriptionMarkers[0].ID); err != nil || len(patched.DescriptionMarkers) != 1 {
		t.Fatalf("after deleting a marker: %d markers, %v", len(patched.DescriptionMarkers), err)
	}
	loan := mustAccount(t, s, AccountInput{Name: "Loan", Kind: "loan", Loan: &LoanDetails{APRBasisPoints: 700, InterestCategoryID: interestCategory.ID}})
	retirement := mustAccount(t, s, AccountInput{Name: "Plan", Kind: "retirement", Retirement: &RetirementDetails{PlanKind: retirementPlanKinds[0], TaxTreatment: taxTreatments[0]}})
	if list, err := s.ListAccounts(); err != nil || len(list) != 3 || list[1].ID != loan.ID || list[2].Retirement == nil {
		t.Fatalf("accounts = %d, %v", len(list), err)
	}
	_ = retirement

	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	batch := mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"11/02/2026","CORNER SHOP 1","-12.00","","Posted"`+"\n"+`"11/03/2026","CORNER SHOP 2","-3.00","","Posted"`+"\n")
	transactions, err := s.ListTransactions(TransactionFilter{Query: "corner shop", Month: "2026-11"})
	if err != nil || len(transactions) != 2 {
		t.Fatalf("transactions matching any case = %d, %v", len(transactions), err)
	}
	status, memo := "pending", "checked"
	if changed, err := s.PatchTransaction(transactions[0].ID, TransactionPatch{Status: &status, Memo: &memo}); err != nil || changed.Status != status || changed.Memo != memo {
		t.Fatalf("patched transaction = %+v, %v", changed, err)
	}
	if _, err := s.CreatePayee(Payee{Name: "Corner shop", Patterns: []string{"corner shop"}}); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListPayees(); err != nil || len(list) != 1 || list[0].Patterns[0] != "CORNER SHOP" {
		t.Fatalf("payees = %+v, %v", list, err)
	}
	assertion, err := s.CreateBalanceAssertion(BalanceAssertionInput{AccountID: checking.ID, AsOf: "2026-11-03", BalanceCents: -1500, Source: "statement"})
	if err != nil || assertion.DifferenceCents != 0 {
		t.Fatalf("assertion = %+v, %v", assertion, err)
	}
	if list, err := s.ListBalanceAssertions(checking.ID); err != nil || len(list) != 1 {
		t.Fatalf("assertions = %d, %v", len(list), err)
	}
	if _, err := s.CreateLink(Link{FromID: transactions[0].ID, EntityType: "note", EntityRef: "note-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(Link{FromID: transactions[0].ID, EntityType: "note", EntityRef: "note-1"}); err == nil {
		t.Error("the same link twice was accepted")
	}
	if list, err := s.ListLinks(transactions[0].ID); err != nil || len(list) != 1 {
		t.Fatalf("links = %d, %v", len(list), err)
	}
	if _, err := s.CreateInterest("Skating", ""); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListInterests(); err != nil || len(list) != 1 {
		t.Fatalf("interests = %d, %v", len(list), err)
	}
	if list, err := s.ListMarketplaceOrders(MarketplaceOrderFilter{TransactionID: transactions[0].ID, Unmatched: false, Limit: 10}); err != nil || len(list) != 0 {
		t.Fatalf("orders = %d, %v", len(list), err)
	}
	if list, err := s.ListMarketplaceOrders(MarketplaceOrderFilter{Unmatched: true}); err != nil || len(list) != 0 {
		t.Fatalf("unmatched orders = %d, %v", len(list), err)
	}
	if list, err := s.ListImportBatches(); err != nil || len(list) != 1 {
		t.Fatalf("batches = %d, %v", len(list), err)
	}
	undone, err := s.UndoImportBatch(batch.ID)
	if err != nil || undone.UndoneAt == 0 {
		t.Fatalf("undo = %+v, %v", undone, err)
	}
	if balance(t, s, checking.ID) != 0 {
		t.Errorf("checking after the undo = %d, want 0", balance(t, s, checking.ID))
	}

	if _, err := s.db.Exec(`INSERT INTO plaid_items (seq, id, plaid_item_id, access_credential_id, environment, created_at, updated_at) VALUES (900, 'plaid_item_000900', 'item-a', 'credential-a', 'sandbox', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO plaid_accounts (plaid_account_id, plaid_item_id, name, created_at, updated_at) VALUES ('plaid-account-a', 'plaid_item_000900', 'Checking', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if matched, err := s.MatchPlaidAccount("plaid-account-a", checking.ID, ""); err != nil || matched.AccountID != checking.ID {
		t.Fatalf("matched plaid account = %+v, %v", matched, err)
	}
	if items, err := s.ListPlaidItems(); err != nil || len(items) != 1 || len(items[0].Accounts) != 1 {
		t.Fatalf("plaid items = %+v, %v", items, err)
	}

	service := newOwnerTestService(t, store)
	if code := service.call(alice, http.MethodGet, "/vocabulary", "", nil); code != http.StatusOK {
		t.Errorf("vocabulary = %d", code)
	}
	if code := service.call(alice, http.MethodPost, "/plaid/sync", "", nil); code != http.StatusServiceUnavailable {
		t.Errorf("plaid sync with Plaid off = %d, want 503", code)
	}
}
