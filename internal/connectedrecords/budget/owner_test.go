package budget

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

const bobPrincipalID = otherOwnerPrincipalID

// ownerTestService serves the routes of one store to callers the test names.
type ownerTestService struct {
	t   *testing.T
	mux *http.ServeMux
}

func newOwnerTestService(t *testing.T, store *Store) *ownerTestService {
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	return &ownerTestService{t: t, mux: mux}
}

// call answers method path with body as caller (nil: no caller at all), and
// decodes a 2xx answer into into when it is not nil.
func (service *ownerTestService) call(caller *ownership.Caller, method, path, body string, into any) int {
	service.t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if caller != nil {
		request = request.WithContext(ownership.ContextWithCaller(request.Context(), *caller))
	}
	recorder := httptest.NewRecorder()
	service.mux.ServeHTTP(recorder, request)
	if into != nil && recorder.Code < 300 {
		if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
			service.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return recorder.Code
}

var (
	alice         = &ownership.Caller{PrincipalID: testOwnerPrincipalID}
	bob           = &ownership.Caller{PrincipalID: bobPrincipalID}
	administrator = &ownership.Caller{PrincipalID: administratorID, SeesEveryOwner: true}
	serviceAlone  = &ownership.Caller{SeesEveryOwner: true}
)

// aliceBooks is what Alice keeps: a checking account with one imported row,
// a category and a payee.
type aliceBooks struct {
	accountID, transactionID, categoryID, payeeID, batchID string
}

func makeAliceBooks(t *testing.T, store *Store) aliceBooks {
	t.Helper()
	books := openLedger(t, store, testOwnerPrincipalID)
	account := mustAccount(t, books, AccountInput{Name: "Checking", Kind: "checking"})
	category, err := books.CreateCategory(Category{Name: "Groceries", Kind: "expense"})
	if err != nil {
		t.Fatal(err)
	}
	payee, err := books.CreatePayee(Payee{Name: "Corner shop", Patterns: []string{"CORNER SHOP"}})
	if err != nil {
		t.Fatal(err)
	}
	batch := mustImport(t, books, "wells_fargo_csv", account.ID, wellsFargoHeader+`"11/02/2026","CORNER SHOP","-12.00","","Posted"`+"\n")
	list, err := books.ListTransactions(TransactionFilter{AccountID: account.ID})
	if err != nil || len(list) != 1 {
		t.Fatalf("alice's transactions = %d, %v", len(list), err)
	}
	return aliceBooks{accountID: account.ID, transactionID: list[0].ID, categoryID: category.ID, payeeID: payee.ID, batchID: batch.ID}
}

func TestAnotherOwnersRecordsAreNotFoundForReadOrWrite(t *testing.T) {
	store := newTestStore(t)
	records := makeAliceBooks(t, store)
	service := newOwnerTestService(t, store)

	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/accounts/" + records.accountID, ""},
		{http.MethodPatch, "/accounts/" + records.accountID, `{"name":"Bob's now"}`},
		{http.MethodPost, "/accounts/" + records.accountID + "/description-markers", `{"marker":"BOBS MARKER"}`},
		{http.MethodGet, "/accounts/" + records.accountID + "/loan-schedule", ""},
		{http.MethodGet, "/transactions/" + records.transactionID, ""},
		{http.MethodPatch, "/transactions/" + records.transactionID, `{"memo":"bob was here"}`},
		{http.MethodPut, "/transactions/" + records.transactionID + "/categories", `{"splits":[{"category_id":"` + records.categoryID + `","amount_cents":1200}]}`},
		{http.MethodGet, "/categories/" + records.categoryID, ""},
		{http.MethodGet, "/payees/" + records.payeeID, ""},
		{http.MethodGet, "/import-batches/" + records.batchID, ""},
		{http.MethodPost, "/import-batches/" + records.batchID + "/undo", ""},
	} {
		if code := service.call(bob, request.method, request.path, request.body, nil); code != http.StatusNotFound {
			t.Errorf("bob %s %s = %d, want 404", request.method, request.path, code)
		}
	}

	// Alice's record is untouched, and still hers.
	var transaction Transaction
	if code := service.call(alice, http.MethodGet, "/transactions/"+records.transactionID, "", &transaction); code != http.StatusOK || transaction.Memo != "" {
		t.Errorf("alice's transaction = %d, memo %q", code, transaction.Memo)
	}

	// A posting cannot land on Alice's account or category from Bob's books.
	var bobAccount Account
	if code := service.call(bob, http.MethodPost, "/accounts", `{"name":"Bob checking","kind":"checking"}`, &bobAccount); code != http.StatusCreated {
		t.Fatalf("bob's account = %d", code)
	}
	for _, body := range []string{
		`{"occurred_on":"2026-11-02","postings":[{"account_id":"` + records.accountID + `","amount_cents":-100},{"account_id":"` + bobAccount.ID + `","amount_cents":100}]}`,
		`{"occurred_on":"2026-11-02","postings":[{"account_id":"` + bobAccount.ID + `","amount_cents":-100},{"category_id":"` + records.categoryID + `","amount_cents":100}]}`,
	} {
		if code := service.call(bob, http.MethodPost, "/transactions", body, nil); code != http.StatusBadRequest {
			t.Errorf("bob posting into alice's books = %d, want 400", code)
		}
	}
	if code := service.call(bob, http.MethodPost, "/links", `{"from_id":"`+records.transactionID+`","entity_type":"note","entity_ref":"x"}`, nil); code != http.StatusBadRequest {
		t.Errorf("bob linking from alice's transaction = %d, want 400", code)
	}
}

func TestEachListAndReportHoldsOnlyTheCallersOwnBooks(t *testing.T) {
	store := newTestStore(t)
	records := makeAliceBooks(t, store)
	service := newOwnerTestService(t, store)

	var bobAccounts []Account
	if code := service.call(bob, http.MethodGet, "/accounts", "", &bobAccounts); code != http.StatusOK || len(bobAccounts) != 0 {
		t.Errorf("bob's accounts = %d, %d rows; want none", code, len(bobAccounts))
	}
	var bobTransactions []Transaction
	if code := service.call(bob, http.MethodGet, "/transactions", "", &bobTransactions); code != http.StatusOK || len(bobTransactions) != 0 {
		t.Errorf("bob's transactions = %d, %d rows; want none", code, len(bobTransactions))
	}
	var bobMonthly []MonthCategoryTotal
	if code := service.call(bob, http.MethodGet, "/reports/monthly", "", &bobMonthly); code != http.StatusOK || len(bobMonthly) != 0 {
		t.Errorf("bob's monthly report = %d, %d rows; want none", code, len(bobMonthly))
	}
	var bobBatches []ImportBatch
	if code := service.call(bob, http.MethodGet, "/import-batches", "", &bobBatches); code != http.StatusOK || len(bobBatches) != 0 {
		t.Errorf("bob's import batches = %d, %d rows; want none", code, len(bobBatches))
	}

	// Each owner has their own Uncategorized and Opening balances.
	var aliceCategories, bobCategories []Category
	service.call(alice, http.MethodGet, "/categories", "", &aliceCategories)
	service.call(bob, http.MethodGet, "/categories", "", &bobCategories)
	if len(aliceCategories) != 3 || len(bobCategories) != 2 {
		t.Fatalf("categories: alice %d, bob %d; want 3 and 2", len(aliceCategories), len(bobCategories))
	}
	for _, category := range bobCategories {
		if category.Role == "" {
			t.Errorf("bob holds %s, which is no role category", category.ID)
		}
		for _, other := range aliceCategories {
			if other.ID == category.ID {
				t.Errorf("bob and alice share category %s", category.ID)
			}
		}
	}

	var aliceAccounts []Account
	if code := service.call(alice, http.MethodGet, "/accounts", "", &aliceAccounts); code != http.StatusOK || len(aliceAccounts) != 1 || aliceAccounts[0].ID != records.accountID {
		t.Errorf("alice's accounts = %d, %+v", code, aliceAccounts)
	}
	if code := service.call(bob, http.MethodGet, "/accounts?owner_principal_id="+testOwnerPrincipalID, "", nil); code != http.StatusForbidden {
		t.Errorf("bob naming alice's books = %d, want 403", code)
	}
}

func TestAnAdministratorReachesAnyOwnersBooksOneAtATime(t *testing.T) {
	store := newTestStore(t)
	records := makeAliceBooks(t, store)
	service := newOwnerTestService(t, store)

	var account Account
	if code := service.call(administrator, http.MethodGet, "/accounts/"+records.accountID, "", &account); code != http.StatusOK || account.BalanceCents != -1200 {
		t.Errorf("the administrator reading alice's account = %d, balance %d", code, account.BalanceCents)
	}
	var transaction Transaction
	if code := service.call(administrator, http.MethodPatch, "/transactions/"+records.transactionID, `{"memo":"checked"}`, &transaction); code != http.StatusOK || transaction.Memo != "checked" {
		t.Errorf("the administrator changing alice's transaction = %d, memo %q", code, transaction.Memo)
	}
	var accounts []Account
	if code := service.call(administrator, http.MethodGet, "/accounts?owner_principal_id="+testOwnerPrincipalID, "", &accounts); code != http.StatusOK || len(accounts) != 1 {
		t.Errorf("the administrator listing alice's accounts = %d, %d rows", code, len(accounts))
	}
	if code := service.call(administrator, http.MethodGet, "/accounts", "", &accounts); code != http.StatusOK || len(accounts) != 0 {
		t.Errorf("the administrator's own accounts = %d, %d rows; want none", code, len(accounts))
	}
	if code := service.call(administrator, http.MethodGet, "/accounts/account_999999", "", nil); code != http.StatusNotFound {
		t.Errorf("a missing account = %d, want 404", code)
	}
	// The service acting as nobody names the books it works in.
	if code := service.call(serviceAlone, http.MethodGet, "/accounts", "", nil); code != http.StatusForbidden {
		t.Errorf("the service naming no owner = %d, want 403", code)
	}
	if code := service.call(serviceAlone, http.MethodGet, "/accounts?owner_principal_id="+testOwnerPrincipalID, "", &accounts); code != http.StatusOK || len(accounts) != 1 {
		t.Errorf("the service naming alice = %d, %d rows", code, len(accounts))
	}
	if code := service.call(nil, http.MethodGet, "/accounts", "", nil); code != http.StatusUnauthorized {
		t.Errorf("no caller = %d, want 401", code)
	}
}

func TestTheDatabaseRefusesAPostingInAnotherOwnersBooksAndAnUnbalancedTransaction(t *testing.T) {
	store := newTestStore(t)
	records := makeAliceBooks(t, store)
	bobBooks := openLedger(t, store, bobPrincipalID)

	// Bob's connection cannot see Alice's rows at all.
	var visible int
	if err := bobBooks.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("accounts bob's connection sees = %d, %v", visible, err)
	}
	insertTransaction := func(books *Ledger, postings [][2]any) error {
		tx, err := books.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		seq, id, err := mintID(tx, "transactions", "transaction")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO transactions (seq, id, occurred_on, status, created_at, updated_at) VALUES ($1,$2,'2026-11-02','posted',0,0)`, seq, id); err != nil {
			return err
		}
		for _, posting := range postings {
			postingSeq, postingID, err := mintID(tx, "postings", "posting")
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO postings (seq, id, transaction_id, account_id, amount_cents) VALUES ($1,$2,$3,$4,$5)`,
				postingSeq, postingID, id, posting[0], posting[1]); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if err := insertTransaction(bobBooks, [][2]any{{records.accountID, -100}, {records.accountID, 100}}); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("a posting from bob's books on alice's account = %v, want a foreign key refusal", err)
	}
	aliceLedger := openLedger(t, store, testOwnerPrincipalID)
	if err := insertTransaction(aliceLedger, [][2]any{{records.accountID, -100}, {records.accountID, 99}}); err == nil || !strings.Contains(err.Error(), "does not balance") {
		t.Errorf("an unbalanced transaction = %v, want it refused at commit", err)
	}
	if err := insertTransaction(aliceLedger, [][2]any{{records.accountID, 0}}); err == nil || !strings.Contains(err.Error(), "does not balance") {
		t.Errorf("a transaction of one posting = %v, want it refused at commit", err)
	}
	if err := insertTransaction(aliceLedger, [][2]any{{records.accountID, -100}, {records.accountID, 100}}); err != nil {
		t.Errorf("a balanced transaction = %v", err)
	}
}
