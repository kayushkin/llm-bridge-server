package permissionstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// standaloneColumns are the columns of each table of the SQLite database the
// standalone permission-store kept, besides schema_version, which records its
// migrations and holds no data. The import refuses a file whose tables have
// any other columns: a column it does not know is data it would drop.
var standaloneColumns = map[string][]string{
	"rules": {"id", "scope", "priority", "tool", "pattern", "outcome", "message", "enabled", "created_at", "updated_at"},
	"audit": {"id", "ts", "bridge_id", "instance_id", "tool", "input_json", "atoms_json", "outcome", "matched_rule_id", "resolved_by"},
}

// auditRowsPerInsert is how many audit rows one INSERT carries. Ten columns
// each, so 500 rows is 5,000 parameters, well under Postgres's 65,535.
const auditRowsPerInsert = 500

// ImportCounts says what an import copied.
type ImportCounts struct {
	Rules                int `json:"rules"`
	AuditEntries         int `json:"audit_entries"`
	DefaultRulesReplaced int `json:"default_rules_replaced"`
}

// ImportStandaloneDatabase copies every rule and every audit row from the
// SQLite file of a standalone permission-store into the module behind db, as
// they are: ids, times, enabled flags and the order audit rows were written
// in. It runs in one transaction, so it copies everything or nothing.
//
// A freshly migrated module holds the default rules its migration inserted.
// The standalone database holds its own copy of them, as edited or deleted
// since, so the import removes the module's defaults first and copies the
// file's rules in their place. It refuses a module that holds anything else:
// an audit row, a rule that is not a default, or a default someone changed,
// so it can never mix two rule sets or run twice. It refuses a file it cannot
// copy whole: a table with a column it does not know, or an enabled flag that
// is not 0 or 1.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	for table, want := range standaloneColumns {
		if err := checkStandaloneColumns(ctx, source, table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		var audited int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit").Scan(&audited); err != nil {
			return fmt.Errorf("counting audit rows: %w", err)
		}
		if audited > 0 {
			return fmt.Errorf("the permissions module already holds %d audit rows; the import copies into a freshly migrated module only", audited)
		}
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE id NOT LIKE 'seed-v1-%' OR created_at <> updated_at`).Scan(&others); err != nil {
			return fmt.Errorf("counting rules: %w", err)
		}
		if others > 0 {
			return fmt.Errorf("the permissions module holds %d rules that are not its untouched defaults; the import copies into a freshly migrated module only", others)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM rules`)
		if err != nil {
			return fmt.Errorf("removing the default rules: %w", err)
		}
		replaced, err := result.RowsAffected()
		if err != nil {
			return err
		}
		counts.DefaultRulesReplaced = int(replaced)
		if counts.Rules, err = importRules(ctx, source, tx); err != nil {
			return err
		}
		counts.AuditEntries, err = importAudit(ctx, source, tx)
		return err
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func importRules(ctx context.Context, source *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := source.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules ORDER BY rowid`)
	if err != nil {
		return 0, fmt.Errorf("reading rules: %w", err)
	}
	defer rows.Close()
	copied := 0
	for rows.Next() {
		var r Rule
		var enabled int64
		if err := rows.Scan(&r.ID, &r.Scope, &r.Priority, &r.Tool, &r.Pattern, &r.Outcome, &r.Message, &enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return 0, fmt.Errorf("reading rules: %w", err)
		}
		if enabled != 0 && enabled != 1 {
			return 0, fmt.Errorf("rule %s has enabled %d, which is neither 0 nor 1", r.ID, enabled)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rules (`+ruleColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			r.ID, r.Scope, r.Priority, r.Tool, r.Pattern, r.Outcome, r.Message, enabled == 1, r.CreatedAt, r.UpdatedAt); err != nil {
			return 0, fmt.Errorf("copying rule %s: %w", r.ID, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading rules: %w", err)
	}
	return copied, nil
}

// importAudit copies the audit rows in the order SQLite wrote them, so the
// identity column numbers them in that order and a tie on ts reads the same.
func importAudit(ctx context.Context, source *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, ts, bridge_id, instance_id, tool, input_json, atoms_json, outcome, matched_rule_id, resolved_by
		FROM audit ORDER BY rowid`)
	if err != nil {
		return 0, fmt.Errorf("reading audit: %w", err)
	}
	defer rows.Close()
	copied := 0
	batch := make([]any, 0, auditRowsPerInsert*10)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var values strings.Builder
		for row := 0; row < len(batch)/10; row++ {
			if row > 0 {
				values.WriteString(", ")
			}
			values.WriteString("(")
			for column := 0; column < 10; column++ {
				if column > 0 {
					values.WriteString(", ")
				}
				fmt.Fprintf(&values, "$%d", row*10+column+1)
			}
			values.WriteString(")")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit (id, ts, bridge_id, instance_id, tool, input_json, atoms_json, outcome, matched_rule_id, resolved_by) VALUES `+values.String(), batch...); err != nil {
			return fmt.Errorf("copying audit rows %d to %d: %w", copied-len(batch)/10+1, copied, err)
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var id, ts, bridgeID, instanceID, tool, input, atoms, outcome, matchedRuleID, resolvedBy string
		if err := rows.Scan(&id, &ts, &bridgeID, &instanceID, &tool, &input, &atoms, &outcome, &matchedRuleID, &resolvedBy); err != nil {
			return 0, fmt.Errorf("reading audit: %w", err)
		}
		batch = append(batch, id, ts, bridgeID, instanceID, tool, input, atoms, outcome, matchedRuleID, resolvedBy)
		copied++
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading audit: %w", err)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return copied, nil
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("it has no table %s, so it is not a permission-store database", table)
	}
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedWant)
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole",
			table, strings.Join(found, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}
