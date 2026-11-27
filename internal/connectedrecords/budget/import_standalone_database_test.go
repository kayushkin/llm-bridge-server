package budget

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// standaloneFixture is a small standalone store: two accounts, a paired
// transfer folded into one transaction, a card purchase on a payee's
// category, a budget, a suggestion and a link. Made up.
const standaloneFixture = `
INSERT INTO institutions VALUES (1, 'institution_000001', 'Example Bank', '', 10, 10);
INSERT INTO categories VALUES (1, 'category_000001', NULL, 'Uncategorized', 'uncategorized', 'uncategorized', 0, 10, 10);
INSERT INTO categories VALUES (2, 'category_000002', NULL, 'Opening balances', 'equity', 'opening_balances', 0, 10, 10);
INSERT INTO categories VALUES (3, 'category_000003', 'category_000004', 'Groceries', 'expense', NULL, 0, 10, 10);
INSERT INTO categories VALUES (4, 'category_000004', NULL, 'Food', 'expense', NULL, 0, 10, 10);
INSERT INTO accounts VALUES (1, 'account_000001', 'institution_000001', 'Checking', 'checking', '1111', '', '', 1, 1, 10, 10);
INSERT INTO accounts VALUES (2, 'account_000002', 'institution_000001', 'Card', 'credit_card', '2222', '', '', 1, 1, 10, 10);
INSERT INTO account_description_markers VALUES (1, 'account_description_marker_000001', 'account_000002', '2222', 'last four', 10);
INSERT INTO credit_card_details VALUES ('account_000002', 'visa', 100000, 3, 28, 2499, 0, '', NULL, NULL);
INSERT INTO payees VALUES (1, 'payee_000001', 'Corner shop', 'category_000003', 10, 10);
INSERT INTO payee_match_rules VALUES (1, 'payee_match_rule_000001', 'payee_000001', 'CORNER SHOP', 10);
INSERT INTO import_batches VALUES (1, 'import_batch_000001', 'wells_fargo_csv', 'account_000001', 'a.csv', '', 'sha-a', '2026-11-01', '2026-11-02', 10, 0);
INSERT INTO import_batches VALUES (2, 'import_batch_000002', 'wells_fargo_csv', 'account_000002', 'b.csv', '', 'sha-b', '2026-11-01', '2026-11-03', 10, 0);
INSERT INTO import_rows VALUES (1, 'import_row_000001', 'import_batch_000001', 1, 'key-1', 'created', '{"DESCRIPTION":"PAYMENT TO CARD 2222"}');
INSERT INTO import_rows VALUES (2, 'import_row_000002', 'import_batch_000002', 1, 'key-2', 'created', '{"DESCRIPTION":"PAYMENT THANK YOU"}');
INSERT INTO import_rows VALUES (3, 'import_row_000003', 'import_batch_000002', 2, 'key-3', 'created', '{"DESCRIPTION":"CORNER SHOP"}');
INSERT INTO transactions VALUES (1, 'transaction_000001', '2026-11-01', 'posted', NULL, 'PAYMENT TO CARD 2222', '', 10, 10);
INSERT INTO transactions VALUES (3, 'transaction_000003', '2026-11-03', 'posted', 'payee_000001', 'CORNER SHOP', '', 10, 10);
INSERT INTO transaction_sources VALUES ('transaction_000001', 'import_row_000001');
INSERT INTO transaction_sources VALUES ('transaction_000001', 'import_row_000002');
INSERT INTO transaction_sources VALUES ('transaction_000003', 'import_row_000003');
INSERT INTO postings VALUES (1, 'posting_000001', 'transaction_000001', 'account_000001', NULL, -5000, '', 0);
INSERT INTO postings VALUES (4, 'posting_000004', 'transaction_000001', 'account_000002', NULL, 5000, '', 0);
INSERT INTO postings VALUES (5, 'posting_000005', 'transaction_000003', 'account_000002', NULL, -1234, '', 0);
INSERT INTO postings VALUES (6, 'posting_000006', 'transaction_000003', NULL, 'category_000003', 1234, '', 0);
INSERT INTO transaction_merges VALUES ('transaction_000002', 'transaction_000001', 'transfer', 10);
INSERT INTO budget_assignments VALUES (1, 'budget_assignment_000001', 'category_000003', '2026-11', 30000, 10, 10);
INSERT INTO links VALUES (1, 'link_000001', 'transaction_000003', 'note', 'note-1', '', 10);
INSERT INTO category_suggestions VALUES (1, 'category_suggestion_000001', 'transaction_000003', 'category_000003', 0.9, 'a shop', 0, 'model-a', 'operation-a', 'accepted', NULL, 20, 'the operator', 15);
INSERT INTO trip_transactions VALUES ('trip_000001', 'transaction_000003', 'suggested', 'dated during the trip', '', 10, 10);
`

// writeStandaloneStore makes a standalone store's SQLite file holding the
// fixture, then statements, and answers its path.
func writeStandaloneStore(t *testing.T, statements string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "budget-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, script := range []string{string(schema), standaloneFixture, statements} {
		if _, err := db.Exec(script); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestTheImportKeepsEveryIDAndAmountAndOwnsEachRow(t *testing.T) {
	store := newTestStore(t)
	path := writeStandaloneStore(t, "")
	counts, err := ImportStandaloneDatabase(t.Context(), store.db, path, testOwnerPrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"categories": 4, "accounts": 2, "transactions": 2, "postings": 4, "import_rows": 3, "transaction_merges": 1, "category_suggestions": 1, "trip_transactions": 1} {
		if counts[table] != want {
			t.Errorf("%s copied = %d, want %d", table, counts[table], want)
		}
	}
	if !strings.Contains(counts.Summary(), "4 postings") {
		t.Errorf("summary = %q", counts.Summary())
	}
	books := openLedger(t, store, testOwnerPrincipalID)
	if got := balance(t, books, "account_000001"); got != -5000 {
		t.Errorf("checking = %d, want -5000", got)
	}
	if got := balance(t, books, "account_000002"); got != 5000-1234 {
		t.Errorf("card = %d, want %d", got, 5000-1234)
	}
	folded, err := books.GetTransaction("transaction_000002")
	if err != nil || folded.ID != "transaction_000001" || len(folded.SourceRowIDs) != 2 {
		t.Errorf("the folded id = %+v, %v; want transaction_000001 with both rows", folded, err)
	}
	purchase, err := books.GetTransaction("transaction_000003")
	if err != nil || purchase.CategoryDecision == nil || purchase.CategoryDecision.Confidence != 0.9 || len(purchase.Links) != 1 {
		t.Errorf("the purchase = %+v, %v", purchase, err)
	}
	budget, err := books.GetBudget("2026-11", "limit")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range budget.Lines {
		if line.CategoryID == "category_000003" && (line.AssignedCents != 30000 || line.SpentCents != 1234) {
			t.Errorf("groceries budget = %+v", line)
		}
	}
	// New rows are numbered after the copied ones, and the role categories
	// are the copied ones.
	account := mustAccount(t, books, AccountInput{Name: "Savings", Kind: "savings"})
	if account.ID != "account_000003" {
		t.Errorf("the next account = %s, want account_000003", account.ID)
	}
	if uncategorized, err := categoryIDForRole(books.db, "uncategorized"); err != nil || uncategorized != "category_000001" {
		t.Errorf("uncategorized = %s, %v", uncategorized, err)
	}
	// Another owner sees none of it.
	others := openLedger(t, store, otherOwnerPrincipalID)
	if list, err := others.ListAccounts(); err != nil || len(list) != 0 {
		t.Errorf("another owner's accounts = %d, %v", len(list), err)
	}
	// A second import of the same owner is refused.
	if _, err := ImportStandaloneDatabase(t.Context(), store.db, path, testOwnerPrincipalID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import = %v, want it refused", err)
	}
}

func TestTheImportRefusesAnAmountThatIsNotWholeCentsAndAnUnbalancedTransaction(t *testing.T) {
	for name, statements := range map[string]string{
		"a decimal amount":         `UPDATE postings SET amount_cents = 1234.5 WHERE id = 'posting_000006';`,
		"an unbalanced one":        `UPDATE postings SET amount_cents = 1233 WHERE id = 'posting_000006';`,
		"an unknown column":        `ALTER TABLE accounts ADD COLUMN nickname TEXT;`,
		"a row of another account": `INSERT INTO postings VALUES (7, 'posting_000007', 'transaction_000003', 'account_000009', NULL, 0, '', 0);`,
	} {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			path := writeStandaloneStore(t, statements)
			if _, err := ImportStandaloneDatabase(t.Context(), store.db, path, testOwnerPrincipalID); err == nil {
				t.Fatal("the import was accepted")
			}
			books := openLedger(t, store, testOwnerPrincipalID)
			if list, err := books.ListAccounts(); err != nil || len(list) != 0 {
				t.Errorf("after a refused import the owner holds %d accounts, %v", len(list), err)
			}
		})
	}
}

func TestTheImportRefusesBooksTheServerAlreadyOpened(t *testing.T) {
	store := newTestStore(t)
	openLedger(t, store, testOwnerPrincipalID)
	path := writeStandaloneStore(t, "")
	if _, err := ImportStandaloneDatabase(t.Context(), store.db, path, testOwnerPrincipalID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("an import into opened books = %v, want it refused", err)
	}
	// An import whose ids another owner already holds is refused whole.
	if _, err := ImportStandaloneDatabase(t.Context(), store.db, path, otherOwnerPrincipalID); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("an import whose ids another owner holds = %v, want a duplicate key refusal", err)
	}
}
