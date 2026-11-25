package budgetstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneTables are the standalone store's tables in an order where every
// row a row points at is copied first, each with the order its rows are read
// in. A category may sit under a later one; that key is checked at commit.
var standaloneTables = []struct{ name, orderBy string }{
	{"institutions", "seq"},
	{"categories", "seq"},
	{"accounts", "seq"},
	{"account_description_markers", "seq"},
	{"credit_card_details", "account_id"},
	{"loan_details", "account_id"},
	{"retirement_details", "account_id"},
	{"payees", "seq"},
	{"payee_match_rules", "seq"},
	{"import_batches", "seq"},
	{"import_rows", "seq"},
	{"transactions", "seq"},
	{"transaction_sources", "transaction_id, import_row_id"},
	{"postings", "seq"},
	{"transaction_merges", "merged_transaction_id"},
	{"transfers_without_partner", "posting_id"},
	{"statements", "seq"},
	{"balance_assertions", "seq"},
	{"recurring_charges", "seq"},
	{"budget_assignments", "seq"},
	{"links", "seq"},
	{"category_suggestions", "seq"},
	{"loan_payment_facts", "seq"},
	{"recurring_bills", "seq"},
	{"recurring_bill_payments", "recurring_bill_id, transaction_id"},
	{"trip_transactions", "trip_ref, transaction_id"},
	{"interests", "seq"},
	{"interest_rules", "seq"},
	{"interest_history_sources", "seq"},
	{"interest_transactions", "interest_id, transaction_id"},
	{"marketplace_orders", "seq"},
	{"marketplace_order_lines", "seq"},
	{"plaid_items", "seq"},
	{"plaid_accounts", "plaid_account_id"},
}

// StandaloneDatabaseImportCounts is how many rows of each table an import
// copied.
type StandaloneDatabaseImportCounts map[string]int64

// Summary names every table and its count, in copy order.
func (counts StandaloneDatabaseImportCounts) Summary() string {
	parts := make([]string, 0, len(standaloneTables))
	for _, table := range standaloneTables {
		parts = append(parts, fmt.Sprintf("%d %s", counts[table.name], strings.ReplaceAll(table.name, "_", " ")))
	}
	return strings.Join(parts, ", ")
}

// moduleColumn is one column of a module table, as the copy fills it.
type moduleColumn struct{ name, dataType string }

// ImportStandaloneDatabase copies a standalone budget-store's SQLite file at
// sqlitePath into the module on db, in one transaction, every row owned by
// ownerPrincipalID and every id kept. It copies only every column of every
// table: a table with a column the module does not have is refused, not
// trimmed. Every value must already have the type its column holds — money
// an integer number of cents, never a decimal — or the import stops naming
// the row. The owner's books must be empty: a server that already opened
// them made their two role categories, and the standalone store's own are
// the ones its postings point at. Every transaction must balance when the
// copy commits, as the store has always required.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (StandaloneDatabaseImportCounts, error) {
	counts := StandaloneDatabaseImportCounts{}
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return nil, err
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, ownerSetting, ownerPrincipalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "budgetstore.owner."+ownerPrincipalID); err != nil {
			return err
		}
		columnsOf := map[string][]moduleColumn{}
		for _, table := range standaloneTables {
			columns, err := moduleColumns(ctx, tx, table.name)
			if err != nil {
				return err
			}
			names := make([]string, len(columns))
			for index, column := range columns {
				names[index] = column.name
			}
			if err := standalonesqlite.CheckColumns(ctx, source, "budget-store", table.name, names); err != nil {
				return fmt.Errorf("%s: %w", sqlitePath, err)
			}
			columnsOf[table.name] = columns
		}
		// Row level security shows this transaction the owner's rows only, so
		// this asks whether the owner's books are empty.
		tables := make([]string, len(standaloneTables))
		for index, table := range standaloneTables {
			tables[index] = table.name
		}
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "budget books of "+ownerPrincipalID+" in the budget", tables...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
			return err
		}
		for _, table := range standaloneTables {
			columns := columnsOf[table.name]
			names := make([]string, len(columns))
			placeholders := make([]string, len(columns))
			for index, column := range columns {
				names[index] = column.name
				placeholders[index] = fmt.Sprintf("$%d", index+1)
			}
			copied, err := standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
				Name: table.name, Columns: names, OrderBy: table.orderBy,
				Insert:  `INSERT INTO ` + table.name + ` (` + strings.Join(names, ", ") + `) VALUES (` + strings.Join(placeholders, ", ") + `)`,
				Convert: func(row []any) ([]any, error) { return checkStandaloneValues(columns, row) },
			})
			if err != nil {
				return err
			}
			counts[table.name] = copied
			if hasColumn(columns, "seq") {
				if err := continueNumberingAfterCopy(ctx, tx, table.name); err != nil {
					return err
				}
			}
		}
		// The balance triggers run here, at commit; so do the deferred keys.
		if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return fmt.Errorf("checking the copied books: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// moduleColumns are table's columns in the module, without the owner, which
// the copy's connection setting fills.
func moduleColumns(ctx context.Context, tx *sql.Tx, table string) ([]moduleColumn, error) {
	rows, err := tx.QueryContext(ctx, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name != 'owner_principal_id' ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var columns []moduleColumn
	for rows.Next() {
		var column moduleColumn
		if err := rows.Scan(&column.name, &column.dataType); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("the budget module has no table %s", table)
	}
	return columns, nil
}

func hasColumn(columns []moduleColumn, name string) bool {
	for _, column := range columns {
		if column.name == name {
			return true
		}
	}
	return false
}

// checkStandaloneValues refuses a value SQLite held in a type its module
// column does not hold. SQLite keeps whatever it is given, so this is where
// an amount stored as a decimal, or a number stored as text, would be found;
// none is converted.
func checkStandaloneValues(columns []moduleColumn, row []any) ([]any, error) {
	for index, value := range row {
		if value == nil {
			continue
		}
		column := columns[index]
		switch column.dataType {
		case "bigint", "integer":
			if _, ok := value.(int64); !ok {
				return nil, fmt.Errorf("%s holds a %T, not an integer", column.name, value)
			}
		case "text":
			switch text := value.(type) {
			case string:
			case []byte:
				row[index] = string(text)
			default:
				return nil, fmt.Errorf("%s holds a %T, not text", column.name, value)
			}
		case "double precision":
			switch number := value.(type) {
			case float64:
			case int64:
				row[index] = float64(number)
			default:
				return nil, fmt.Errorf("%s holds a %T, not a number", column.name, value)
			}
		default:
			return nil, fmt.Errorf("%s is a %s column, which the import does not know", column.name, column.dataType)
		}
	}
	return row, nil
}

// continueNumberingAfterCopy makes table's next number follow both the rows
// just copied and every number already handed out to any owner. The copy can
// see only its own owner's rows, so the sequence itself says how far it went.
func continueNumberingAfterCopy(ctx context.Context, tx *sql.Tx, table string) error {
	var copiedLast int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM `+table).Scan(&copiedLast); err != nil {
		return err
	}
	var sequence string
	if err := tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1, 'seq')`, table).Scan(&sequence); err != nil {
		return err
	}
	var handedOut int64
	var called bool
	if err := tx.QueryRowContext(ctx, `SELECT last_value, is_called FROM `+sequence).Scan(&handedOut, &called); err != nil {
		return fmt.Errorf("reading the numbering of %s: %w", table, err)
	}
	if !called {
		handedOut = 0
	}
	last := max(copiedLast, handedOut)
	return postgresstorage.ContinueNumberingAfter(ctx, tx, table, "seq", last)
}
