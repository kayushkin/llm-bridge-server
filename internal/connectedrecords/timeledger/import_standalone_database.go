package timeledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the import's errors.
const standaloneStoreName = "time-ledger-store"

// ImportCounts says how many rows of each table an import copied. Links are
// summed over every link table.
type ImportCounts struct {
	TimeEntries     int64 `json:"time_entries"`
	Revisions       int64 `json:"revisions"`
	Kinds           int64 `json:"kinds"`
	KindAssignments int64 `json:"kind_assignments"`
	Reviews         int64 `json:"reviews"`
	Links           int64 `json:"links"`
}

// Summary is the counts in one line, and nothing of the records.
func (counts ImportCounts) Summary() string {
	return fmt.Sprintf("time_entries=%d revisions=%d kinds=%d kind_assignments=%d reviews=%d links=%d",
		counts.TimeEntries, counts.Revisions, counts.Kinds, counts.KindAssignments, counts.Reviews, counts.Links)
}

// standaloneRecordNumber reads a standalone row's number and checks its id is
// the one that number makes: the import keeps ids by their number and will
// not change one.
func standaloneRecordNumber(prefix string, seqValue, idValue any) (int64, error) {
	seq, err := standalonesqlite.Integer("seq", seqValue)
	if err != nil {
		return 0, err
	}
	id, err := standalonesqlite.Text("id", idValue)
	if err != nil {
		return 0, err
	}
	if want := postgresstorage.FormatRecordID(prefix, seq); id != want {
		return 0, fmt.Errorf("id %q has number %d, whose id is %s", id, seq, want)
	}
	return seq, nil
}

// standaloneReference reads a column that names another row by its public id.
func standaloneReference(column, prefix string, value any) (int64, error) {
	id, err := standalonesqlite.Text(column, value)
	if err != nil {
		return 0, err
	}
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", column, err)
	}
	return number, nil
}

// optionalReference reads a column that names another row or holds NULL.
func optionalReference(column, prefix string, value any) (sql.NullInt64, error) {
	if value == nil {
		return sql.NullInt64{}, nil
	}
	number, err := standaloneReference(column, prefix, value)
	return sql.NullInt64{Int64: number, Valid: err == nil}, err
}

// linkTableNames are LinkTypes' tables in a fixed order.
func linkTableNames() []string {
	names := make([]string, 0, len(LinkTypes))
	for _, name := range LinkTypeNames() {
		names = append(names, LinkTypes[name].Table)
	}
	return names
}

// standaloneLinkTable is one link table as the standalone store kept it.
func standaloneLinkTable(owner string, linkType LinkType) standalonesqlite.Table {
	columns := []string{"seq", "time_entry_id", linkType.RefColumn}
	if linkType.HasOccurrence {
		columns = append(columns, "occurrence_starts_at")
	}
	if linkType.HasOverlap {
		columns = append(columns, "overlap_seconds")
	}
	if linkType.HasAmount {
		columns = append(columns, "amount_cents")
	}
	columns = append(columns, "bears_on", "linked_by", "linked_by_kind", "reason", "linked_at", "removed_at", "removed_by", "removed_by_kind")
	insertColumns := append([]string{"seq", "owner_principal_id", "time_entry_seq"}, columns[2:]...)
	placeholders := make([]string, len(insertColumns))
	for index := range placeholders {
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	return standalonesqlite.Table{
		Name:    linkType.Table,
		Columns: columns,
		OrderBy: "seq",
		Insert: `INSERT INTO ` + linkType.Table + ` (` + strings.Join(insertColumns, ", ") + `)
			OVERRIDING SYSTEM VALUE VALUES (` + strings.Join(placeholders, ", ") + `)`,
		Convert: func(row []any) ([]any, error) {
			seq, err := standalonesqlite.Integer("seq", row[0])
			if err != nil {
				return nil, err
			}
			entrySeq, err := standaloneReference("time_entry_id", EntryIDPrefix, row[1])
			if err != nil {
				return nil, err
			}
			return append([]any{seq, owner, entrySeq}, row[2:]...), nil
		},
	}
}

// standaloneTables are the tables of the standalone file in the order they
// are copied (each after what it points at), with the live schema's columns.
func standaloneTables(owner string) []standalonesqlite.Table {
	tables := []standalonesqlite.Table{
		{
			Name: "time_entries",
			Columns: []string{"seq", "id", "title", "title_by", "title_by_kind", "span_by", "span_by_kind", "starts_at", "ends_at", "all_day",
				"start_date", "end_date", "time_zone", "place", "place_by", "place_by_kind", "parent_time_entry_id", "parent_set_by",
				"parent_set_by_kind", "created_by", "created_by_kind", "created_reason", "created_at", "updated_at", "evidence_changed_at",
				"deleted_at", "deleted_by"},
			// A parent can come after its child (an entry placed inside a
			// journey made later), so the import defers the parent's foreign
			// key to its commit.
			OrderBy: "seq",
			Insert: `INSERT INTO time_entries (seq, owner_principal_id, title, title_by, title_by_kind, span_by, span_by_kind, starts_at, ends_at,
				all_day, start_date, end_date, time_zone, place, place_by, place_by_kind, parent_seq, parent_set_by, parent_set_by_kind,
				created_by, created_by_kind, created_reason, created_at, updated_at, evidence_changed_at, deleted_at, deleted_by)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
				$20, $21, $22, $23, $24, $25, $26, $27)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(EntryIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				allDay, err := standalonesqlite.Boolean("all_day", row[9])
				if err != nil {
					return nil, err
				}
				parentSeq, err := optionalReference("parent_time_entry_id", EntryIDPrefix, row[16])
				if err != nil {
					return nil, err
				}
				values := append([]any{seq, owner}, row[2:9]...)
				values = append(values, allDay)
				values = append(values, row[10:16]...)
				values = append(values, parentSeq)
				return append(values, row[17:]...), nil
			},
		},
		{
			Name:    "time_entry_revisions",
			Columns: []string{"revision_id", "time_entry_id", "reason", "replaced_at", "entry"},
			OrderBy: "revision_id",
			Insert: `INSERT INTO time_entry_revisions (revision_id, owner_principal_id, time_entry_seq, reason, replaced_at, entry)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				revisionID, err := standalonesqlite.Integer("revision_id", row[0])
				if err != nil {
					return nil, err
				}
				entrySeq, err := standaloneReference("time_entry_id", EntryIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{revisionID, owner, entrySeq}, row[2:]...), nil
			},
		},
		{
			Name:    "time_entry_kinds",
			Columns: []string{"seq", "id", "name", "description", "created_by", "created_by_kind", "created_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO time_entry_kinds (seq, owner_principal_id, name, description, created_by, created_by_kind, created_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(KindIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner}, row[2:]...), nil
			},
		},
		{
			Name: "time_entry_kind_assignments",
			Columns: []string{"seq", "time_entry_id", "kind_id", "assigned_by", "assigned_by_kind", "reason", "assigned_at", "removed_at",
				"removed_by", "removed_by_kind"},
			OrderBy: "seq",
			Insert: `INSERT INTO time_entry_kind_assignments (seq, owner_principal_id, time_entry_seq, kind_seq, assigned_by, assigned_by_kind,
				reason, assigned_at, removed_at, removed_by, removed_by_kind)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standalonesqlite.Integer("seq", row[0])
				if err != nil {
					return nil, err
				}
				entrySeq, err := standaloneReference("time_entry_id", EntryIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				kindSeq, err := standaloneReference("kind_id", KindIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner, entrySeq, kindSeq}, row[3:]...), nil
			},
		},
		{
			Name: "time_entry_reviews",
			Columns: []string{"seq", "id", "time_entry_id", "outcome", "reason", "reviewed_by", "reviewed_by_kind", "model", "input_tokens",
				"output_tokens", "cost_usd", "links_seen", "suggested_parent_time_entry_id", "suggested_kind_names", "created_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO time_entry_reviews (seq, owner_principal_id, time_entry_seq, outcome, reason, reviewed_by, reviewed_by_kind, model,
				input_tokens, output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(ReviewIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				entrySeq, err := standaloneReference("time_entry_id", EntryIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner, entrySeq}, row[3:]...), nil
			},
		},
	}
	for _, name := range LinkTypeNames() {
		tables = append(tables, standaloneLinkTable(owner, LinkTypes[name]))
	}
	return tables
}

// numberedTables are each table whose number is an identity column, with
// that column, numbered on after the import.
var numberedTables = func() [][2]string {
	tables := [][2]string{{"time_entries", "seq"}, {"time_entry_revisions", "revision_id"}, {"time_entry_kinds", "seq"},
		{"time_entry_kind_assignments", "seq"}, {"time_entry_reviews", "seq"}}
	for _, table := range linkTableNames() {
		tables = append(tables, [2]string{table, "seq"})
	}
	return tables
}()

// standaloneHighestNumber is the highest number SQLite ever handed out in a
// table: its AUTOINCREMENT counter, which can be past the highest row left.
func standaloneHighestNumber(ctx context.Context, source *sql.DB, table string) (int64, error) {
	var highest int64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&highest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the highest number SQLite gave %s: %w", table, err)
	}
	return highest, nil
}

// ImportStandaloneDatabase copies every row of a standalone time-ledger-store's
// SQLite file into the module behind db, as it is — numbers, ids, times and
// text — with every row owned by ownerPrincipalID. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses an owner that is not a principal-store id, a module that already
// holds any row (so it can never mix two stores or run twice), and a file it
// cannot copy whole: a table with a column it does not know, an id that is not
// the form its number makes, or a row pointing at one the file does not hold.
// Afterwards each numbered table numbers on from the highest number the
// standalone store ever gave, so no id that notes and links already carry is
// handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (ImportCounts, error) {
	var counts ImportCounts
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return counts, err
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	tables := standaloneTables(ownerPrincipalID)
	tableNames := make([]string, 0, len(tables))
	for _, table := range tables {
		if err := standalonesqlite.CheckColumns(ctx, source, standaloneStoreName, table.Name, table.Columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		tableNames = append(tableNames, table.Name)
	}
	highestUsed := map[string]int64{}
	for _, table := range numberedTables {
		if highestUsed[table[0]], err = standaloneHighestNumber(ctx, source, table[0]); err != nil {
			return counts, err
		}
	}
	copied := map[string]int64{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "time_ledger", tableNames...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS time_entries_parent_is_the_owners DEFERRED`); err != nil {
			return fmt.Errorf("deferring the parent check to the end of the import: %w", err)
		}
		for _, table := range tables {
			count, err := standalonesqlite.Copy(ctx, source, tx, table)
			if err != nil {
				return err
			}
			copied[table.Name] = count
		}
		for _, table := range numberedTables {
			var last int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX("+table[1]+"), 0) FROM "+table[0]).Scan(&last); err != nil {
				return fmt.Errorf("reading the highest number in %s: %w", table[0], err)
			}
			last = max(last, highestUsed[table[0]])
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table[0], table[1], last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, fmt.Errorf("importing %s: %w", sqlitePath, err)
	}
	counts = ImportCounts{
		TimeEntries:     copied["time_entries"],
		Revisions:       copied["time_entry_revisions"],
		Kinds:           copied["time_entry_kinds"],
		KindAssignments: copied["time_entry_kind_assignments"],
		Reviews:         copied["time_entry_reviews"],
	}
	for _, table := range linkTableNames() {
		counts.Links += copied[table]
	}
	return counts, nil
}
