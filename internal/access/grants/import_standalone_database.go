package grants

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

// standaloneGrantColumns are the columns of the grants table of the SQLite
// database the standalone grant-store kept. The import refuses a file whose
// table has any other columns: a column it does not know is data it would drop.
var standaloneGrantColumns = []string{"id", "seq", "principal_id", "relation", "resource_type", "resource_id", "note", "granted_at", "revoked_at"}

// ImportCounts says what an import copied.
type ImportCounts struct {
	Grants        int   `json:"grants"`
	RevokedGrants int   `json:"revoked_grants"`
	LastGrantSeq  int64 `json:"last_grant_seq"`
}

// ImportStandaloneDatabase copies every grant from the SQLite file of a
// standalone grant-store into the module behind db, as they are: numbers, ids,
// times, notes and revoked rows. It runs in one transaction, so it copies
// everything or nothing.
//
// It refuses a module that already holds any grant, so it can never mix two
// registries or run twice, and it refuses a file it cannot copy whole: a table
// with a column it does not know, or an id that is not the form its number
// makes. The vocabulary is not re-checked: a grant the standalone store held is
// copied as it was, and the store reads it as it always did. Afterwards the
// numbering goes on from the highest number copied, so no id that sessions,
// chats and notes already carry is handed out again.
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
	if err := checkStandaloneColumns(ctx, source, "grants", standaloneGrantColumns); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		var held int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM grants").Scan(&held); err != nil {
			return fmt.Errorf("counting grants: %w", err)
		}
		if held > 0 {
			return fmt.Errorf("the grants module already holds %d grants; the import copies into an empty module only", held)
		}
		rows, err := source.QueryContext(ctx, `
			SELECT id, seq, principal_id, relation, resource_type, resource_id, note, granted_at, revoked_at
			FROM grants ORDER BY seq`)
		if err != nil {
			return fmt.Errorf("reading grants: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var g Grant
			if err := rows.Scan(&g.ID, &g.Seq, &g.PrincipalID, &g.Relation, &g.ResourceType, &g.ResourceID, &g.Note, &g.GrantedAt, &g.RevokedAt); err != nil {
				return fmt.Errorf("reading grants: %w", err)
			}
			if g.ID != formatID(g.Seq) {
				return fmt.Errorf("grant %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", g.ID, g.Seq, formatID(g.Seq))
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO grants (seq, principal_id, relation, resource_type, resource_id, note, granted_at, revoked_at)
				OVERRIDING SYSTEM VALUE
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				g.Seq, g.PrincipalID, g.Relation, g.ResourceType, g.ResourceID, g.Note, g.GrantedAt, g.RevokedAt); err != nil {
				return fmt.Errorf("copying grant %s: %w", g.ID, err)
			}
			counts.Grants++
			if g.RevokedAt != 0 {
				counts.RevokedGrants++
			}
			counts.LastGrantSeq = g.Seq
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reading grants: %w", err)
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "grants", "seq", counts.LastGrantSeq)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
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
		return fmt.Errorf("it has no table %s, so it is not a grant-store database", table)
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
