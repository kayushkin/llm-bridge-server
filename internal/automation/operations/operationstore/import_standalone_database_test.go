package operationstore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore/operationstoretest"
	"github.com/kayushkin/llm-bridge/msg"
)

// standaloneOperationsSchema is the schema the SQLite operations file had when
// this server kept operations in it.
const standaloneOperationsSchema = `
CREATE TABLE operations (
	id TEXT PRIMARY KEY, type TEXT NOT NULL, organization_id TEXT NOT NULL, principal_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL, intent_fingerprint TEXT NOT NULL, parent_operation_id TEXT NOT NULL,
	root_operation_id TEXT NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL, intent_json TEXT NOT NULL,
	receipt_json TEXT NOT NULL, lease_owner TEXT NOT NULL DEFAULT '', lease_token INTEGER NOT NULL DEFAULT 0,
	lease_expires_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE operation_events (operation_id TEXT NOT NULL REFERENCES operations(id), sequence INTEGER NOT NULL,
	kind TEXT NOT NULL, event_json TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY (operation_id, sequence));
CREATE TABLE operation_spend (operation_id TEXT NOT NULL REFERENCES operations(id), root_operation_id TEXT NOT NULL,
	organization_id TEXT NOT NULL, model TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
	cache_read_tokens INTEGER NOT NULL, cache_write_tokens INTEGER NOT NULL, cost_usd REAL NOT NULL,
	cost_known INTEGER NOT NULL, at INTEGER NOT NULL);
CREATE TABLE organization_budgets (organization_id TEXT PRIMARY KEY, monthly_limit_usd REAL NOT NULL,
	updated_by TEXT NOT NULL, updated_at INTEGER NOT NULL);
`

func writeStandaloneOperationsFile(t *testing.T, extraStatements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operations.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	at := time.Date(2026, 10, 1, 0, 0, 0, 987654321, time.UTC)
	receipt := msg.OperationReceipt{ID: "operation_01JTESTOPERATION0000000000", Type: "llm.completion", OrganizationID: "principal_000006",
		PrincipalID: "principal_000004", State: msg.OperationStateSucceeded, Revision: 2, CreatedAt: at, UpdatedAt: at}
	if _, err := source.Exec(standaloneOperationsSchema); err != nil {
		t.Fatal(err)
	}
	// The SQLite store wrote its JSON as Go byte slices, which SQLite keeps as
	// blobs, so the file is written the same way.
	receiptJSON := []byte(`{"id":"operation_01JTESTOPERATION0000000000","state":"succeeded","revision":2}`)
	if _, err := source.Exec(`INSERT INTO operations VALUES (?, 'llm.completion', 'principal_000006', 'principal_000004', 'key-1', 'fingerprint',
		'', ?, 'succeeded', 2, ?, ?, '', 3, 0, ?, ?)`, receipt.ID, receipt.ID, []byte(`{"type":"llm.completion"}`), receiptJSON,
		at.UnixNano(), at.UnixNano()+5); err != nil {
		t.Fatal(err)
	}
	for sequence, kind := range []string{"accepted", "succeeded"} {
		if _, err := source.Exec(`INSERT INTO operation_events VALUES (?, ?, ?, ?, ?)`, receipt.ID, sequence+1, kind,
			[]byte(`{"operation_id":"operation_01JTESTOPERATION0000000000","sequence":`+string(rune('1'+sequence))+`,"kind":"`+kind+`"}`), at.UnixNano()+int64(sequence)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.Exec(`INSERT INTO operation_spend VALUES (?, ?, 'principal_000006', 'test-model', 10, 2, 0, 0, 0.25, 1, ?)`,
		receipt.ID, receipt.ID, at.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`INSERT INTO organization_budgets VALUES ('principal_000006', 100, '', ?)`, at.UnixNano()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range extraStatements {
		if _, err := source.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestImportCopiesEveryOperationEventSpendRowAndBudgetAsTheyAre(t *testing.T) {
	path := writeStandaloneOperationsFile(t)
	store, database := operationstoretest.NewStoreAndDatabase(t)
	module, err := operationstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	db := database.Open(module)
	counts, err := operationstore.ImportStandaloneDatabase(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Operations != 1 || counts.Events != 2 || counts.SpendRows != 1 || counts.OrganizationBudgets != 1 || counts.OperationsByState["succeeded"] != 1 {
		t.Fatalf("counts: %+v", counts)
	}
	operation, err := store.Get("operation_01JTESTOPERATION0000000000")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Receipt.Revision != 2 || operation.LeaseToken != 3 {
		t.Fatalf("operation: %+v", operation)
	}
	var createdAt, updatedAt int64
	var receiptJSON string
	if err := db.QueryRow(`SELECT created_at, updated_at, receipt_json FROM operations`).Scan(&createdAt, &updatedAt, &receiptJSON); err != nil {
		t.Fatal(err)
	}
	if createdAt%1000 != 321 || updatedAt != createdAt+5 || receiptJSON != `{"id":"operation_01JTESTOPERATION0000000000","state":"succeeded","revision":2}` {
		t.Fatalf("stored row: %d %d %s", createdAt, updatedAt, receiptJSON)
	}
	events, err := store.EventsAfter(operation.Receipt.ID, 1)
	if err != nil || len(events) != 1 || events[0].Kind != msg.OperationEventSucceeded {
		t.Fatalf("events after 1: %+v %v", events, err)
	}
	spent, err := store.TreeSpentUSD(operation.Receipt.ID)
	if err != nil || spent != 0.25 {
		t.Fatalf("spend: %v %v", spent, err)
	}
	budget, found, err := store.OrganizationBudget("principal_000006", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || !found || budget.MonthlyLimitUSD != 100 || budget.SpentUSD != 0.25 {
		t.Fatalf("budget: %+v %v %v", budget, found, err)
	}

	if _, err := operationstore.ImportStandaloneDatabase(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	cases := map[string]struct {
		statements []string
		want       string
	}{
		"an unknown table":  {[]string{`CREATE TABLE operation_notes (id TEXT)`}, "operation_notes"},
		"an unknown column": {[]string{`ALTER TABLE operations ADD COLUMN note TEXT`}, "note"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeStandaloneOperationsFile(t, test.statements...)
			_, database := operationstoretest.NewStoreAndDatabase(t)
			module, err := operationstore.Module()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := operationstore.ImportStandaloneDatabase(context.Background(), database.Open(module), path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("import: %v, want an error naming %s", err, test.want)
			}
		})
	}

	t.Run("a revision with no event", func(t *testing.T) {
		path := writeStandaloneOperationsFile(t)
		source, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Exec(`DELETE FROM operation_events WHERE sequence = 2`); err != nil {
			t.Fatal(err)
		}
		source.Close()
		_, database := operationstoretest.NewStoreAndDatabase(t)
		module, err := operationstore.Module()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := operationstore.ImportStandaloneDatabase(context.Background(), database.Open(module), path); err == nil || !strings.Contains(err.Error(), "last event") {
			t.Fatalf("import: %v", err)
		}
	})
}
