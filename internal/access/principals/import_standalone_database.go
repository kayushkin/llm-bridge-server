package principals

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
// standalone principal-store kept, after its ensureColumns had added
// availability and is_administrator. The import refuses a file whose tables
// have any other columns: a column it does not know is data it would drop.
var standaloneColumns = map[string][]string{
	"principals": {"id", "seq", "kind", "display_name", "email", "disabled_at", "created_at", "updated_at",
		"availability", "is_administrator"},
	"group_members": {"group_id", "member_id", "created_at"},
	"time_off":      {"id", "seq", "principal_id", "starts_at", "ends_at", "note", "created_at"},
	"identity_bindings": {"id", "seq", "principal_id", "provider", "tenant_id", "subject", "linked_at", "linked_by",
		"link_note", "unlinked_at", "unlinked_by", "unlink_reason"},
}

// standaloneIndexTables are the tables of the file that only index the ones
// above: the FTS5 table over principals and the shadow tables FTS5 keeps for
// it. They hold nothing the import would lose, since the module builds its
// own search document from the rows.
var standaloneIndexTables = map[string]bool{
	"principals_fts": true, "principals_fts_data": true, "principals_fts_idx": true,
	"principals_fts_content": true, "principals_fts_docsize": true, "principals_fts_config": true,
}

// ImportCounts says how many rows of each table an import copied, and the
// highest number each numbered table held.
type ImportCounts struct {
	Principals          int   `json:"principals"`
	DisabledPrincipals  int   `json:"disabled_principals"`
	Memberships         int   `json:"memberships"`
	TimeOff             int   `json:"time_off"`
	IdentityBindings    int   `json:"identity_bindings"`
	LastPrincipalSeq    int64 `json:"last_principal_seq"`
	LastTimeOffSeq      int64 `json:"last_time_off_seq"`
	LastIdentityBinding int64 `json:"last_identity_binding_seq"`
}

// ImportStandaloneDatabase copies every principal, membership, absence and
// identity binding from the SQLite file of a standalone principal-store into
// the module behind db, as they are: numbers, ids, times, disabled rows and
// unlinked bindings. It runs in one transaction, so it copies everything or
// nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// registries or run twice, and it refuses a file it cannot copy whole: a table
// it does not know, a table with a column it does not know, an id that is not
// the form its number makes, or a row pointing at a principal the file does
// not hold. Afterwards each table numbers on from the highest number copied,
// so no id that cards, grants and tickets already carry is handed out again.
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
	if err := checkStandaloneTables(ctx, source); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	for table, want := range standaloneColumns {
		if err := checkStandaloneColumns(ctx, source, table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range []string{"principals", "group_members", "time_off", "identity_bindings"} {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table, err)
			}
			if held > 0 {
				return fmt.Errorf("the principals module already holds %d rows in %s; the import copies into an empty module only", held, table)
			}
		}
		var err error
		if counts.Principals, counts.DisabledPrincipals, counts.LastPrincipalSeq, err = importPrincipals(ctx, source, tx); err != nil {
			return err
		}
		if counts.Memberships, err = importMemberships(ctx, source, tx); err != nil {
			return err
		}
		if counts.TimeOff, counts.LastTimeOffSeq, err = importTimeOff(ctx, source, tx); err != nil {
			return err
		}
		if counts.IdentityBindings, counts.LastIdentityBinding, err = importIdentityBindings(ctx, source, tx); err != nil {
			return err
		}
		for _, numbering := range []struct {
			table string
			last  int64
		}{
			{"principals", counts.LastPrincipalSeq},
			{"time_off", counts.LastTimeOffSeq},
			{"identity_bindings", counts.LastIdentityBinding},
		} {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbering.table, "seq", numbering.last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// checkStandaloneTables refuses a file holding a table the import does not
// know, since its rows would be left behind.
func checkStandaloneTables(ctx context.Context, source *sql.DB) error {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return fmt.Errorf("listing its tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, known := standaloneColumns[name]; known || standaloneIndexTables[name] {
			continue
		}
		return fmt.Errorf("it has a table %s the import does not know; it copies only a file it can copy whole", name)
	}
	return rows.Err()
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
		return fmt.Errorf("it has no table %s, so it is not a principal-store database", table)
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

// standalonePrincipalNumber reads a principal id the file holds, which must be
// the exact form its number makes.
func standalonePrincipalNumber(id, where string) (int64, error) {
	seq, ok := principalNumberOf(id)
	if !ok {
		return 0, fmt.Errorf("%s names %q, which is not a principal id this store makes", where, id)
	}
	return seq, nil
}

func importPrincipals(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied, disabled int, lastSeq int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, seq, kind, display_name, email, disabled_at, created_at, updated_at, availability, is_administrator
		FROM principals ORDER BY seq`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reading principals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Principal
		var availability string
		var isAdministrator int64
		if err := rows.Scan(&p.ID, &p.Seq, &p.Kind, &p.DisplayName, &p.Email, &p.DisabledAt, &p.CreatedAt, &p.UpdatedAt,
			&availability, &isAdministrator); err != nil {
			return 0, 0, 0, fmt.Errorf("reading principals: %w", err)
		}
		if p.ID != formatID(p.Seq) {
			return 0, 0, 0, fmt.Errorf("principal %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", p.ID, p.Seq, formatID(p.Seq))
		}
		if _, ok := lookupKindDefinition(p.Kind); !ok {
			return 0, 0, 0, fmt.Errorf("principal %s has kind %q, which is not one of %s", p.ID, p.Kind, strings.Join(Kinds, ", "))
		}
		if isAdministrator != 0 && isAdministrator != 1 {
			return 0, 0, 0, fmt.Errorf("principal %s has is_administrator %d, which is neither 0 nor 1", p.ID, isAdministrator)
		}
		if _, err := decodeAvailability(availability); err != nil {
			return 0, 0, 0, fmt.Errorf("principal %s: %w", p.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO principals (seq, kind, display_name, email, disabled_at, created_at, updated_at, availability, is_administrator, search_document)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, `+searchDocumentSQL(10, 11)+`)`,
			p.Seq, p.Kind, p.DisplayName, p.Email, p.DisabledAt, p.CreatedAt, p.UpdatedAt, availability, isAdministrator == 1,
			searchTextOf(p.DisplayName), searchTextOf(p.Email)); err != nil {
			return 0, 0, 0, fmt.Errorf("copying principal %s: %w", p.ID, err)
		}
		copied++
		if p.DisabledAt != 0 {
			disabled++
		}
		lastSeq = p.Seq
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("reading principals: %w", err)
	}
	return copied, disabled, lastSeq, nil
}

func importMemberships(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, err error) {
	rows, err := source.QueryContext(ctx, `SELECT group_id, member_id, created_at FROM group_members ORDER BY group_id, member_id`)
	if err != nil {
		return 0, fmt.Errorf("reading group_members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var groupID, memberID string
		var createdAt int64
		if err := rows.Scan(&groupID, &memberID, &createdAt); err != nil {
			return 0, fmt.Errorf("reading group_members: %w", err)
		}
		where := fmt.Sprintf("membership %s/%s", groupID, memberID)
		groupSeq, err := standalonePrincipalNumber(groupID, where)
		if err != nil {
			return 0, err
		}
		memberSeq, err := standalonePrincipalNumber(memberID, where)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members (group_seq, member_seq, created_at) VALUES ($1, $2, $3)`,
			groupSeq, memberSeq, createdAt); err != nil {
			return 0, fmt.Errorf("copying %s: %w", where, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading group_members: %w", err)
	}
	return copied, nil
}

func importTimeOff(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastSeq int64, err error) {
	rows, err := source.QueryContext(ctx, `SELECT id, seq, principal_id, starts_at, ends_at, note, created_at FROM time_off ORDER BY seq`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading time_off: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t TimeOff
		if err := rows.Scan(&t.ID, &t.Seq, &t.PrincipalID, &t.StartsAt, &t.EndsAt, &t.Note, &t.CreatedAt); err != nil {
			return 0, 0, fmt.Errorf("reading time_off: %w", err)
		}
		if t.ID != formatTimeOffID(t.Seq) {
			return 0, 0, fmt.Errorf("time off %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", t.ID, t.Seq, formatTimeOffID(t.Seq))
		}
		principalSeq, err := standalonePrincipalNumber(t.PrincipalID, "time off "+t.ID)
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO time_off (seq, principal_seq, starts_at, ends_at, note, created_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6)`,
			t.Seq, principalSeq, t.StartsAt, t.EndsAt, t.Note, t.CreatedAt); err != nil {
			return 0, 0, fmt.Errorf("copying time off %s: %w", t.ID, err)
		}
		copied++
		lastSeq = t.Seq
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading time_off: %w", err)
	}
	return copied, lastSeq, nil
}

func importIdentityBindings(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastSeq int64, err error) {
	rows, err := source.QueryContext(ctx, `SELECT id, seq, principal_id, provider, tenant_id, subject, linked_at, linked_by,
		link_note, unlinked_at, unlinked_by, unlink_reason FROM identity_bindings ORDER BY seq`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading identity_bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b IdentityBinding
		if err := rows.Scan(&b.ID, &b.Seq, &b.PrincipalID, &b.Provider, &b.TenantID, &b.Subject, &b.LinkedAt, &b.LinkedBy,
			&b.LinkNote, &b.UnlinkedAt, &b.UnlinkedBy, &b.UnlinkReason); err != nil {
			return 0, 0, fmt.Errorf("reading identity_bindings: %w", err)
		}
		if b.ID != formatIdentityBindingID(b.Seq) {
			return 0, 0, fmt.Errorf("identity binding %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", b.ID, b.Seq, formatIdentityBindingID(b.Seq))
		}
		where := "identity binding " + b.ID
		principalSeq, err := standalonePrincipalNumber(b.PrincipalID, where)
		if err != nil {
			return 0, 0, err
		}
		linkedBySeq, err := standalonePrincipalNumber(b.LinkedBy, where+" linked_by")
		if err != nil {
			return 0, 0, err
		}
		// unlinked_by is '' on an active binding, and a principal id on an
		// unlinked one; the module keeps the first as null.
		var unlinkedBySeq sql.NullInt64
		if b.UnlinkedBy != "" {
			seq, err := standalonePrincipalNumber(b.UnlinkedBy, where+" unlinked_by")
			if err != nil {
				return 0, 0, err
			}
			unlinkedBySeq = sql.NullInt64{Int64: seq, Valid: true}
		} else if b.UnlinkedAt != 0 {
			return 0, 0, fmt.Errorf("%s was unlinked at %d by nobody; the module records who unlinked every binding", where, b.UnlinkedAt)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity_bindings (seq, principal_seq, provider, tenant_id, subject, linked_at, linked_by_seq,
				link_note, unlinked_at, unlinked_by_seq, unlink_reason)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			b.Seq, principalSeq, b.Provider, b.TenantID, b.Subject, b.LinkedAt, linkedBySeq,
			b.LinkNote, b.UnlinkedAt, unlinkedBySeq, b.UnlinkReason); err != nil {
			return 0, 0, fmt.Errorf("copying %s: %w", where, err)
		}
		copied++
		lastSeq = b.Seq
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading identity_bindings: %w", err)
	}
	return copied, lastSeq, nil
}
