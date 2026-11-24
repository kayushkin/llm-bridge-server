package bundlestore

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
// standalone bundle-store kept, once its own start-up migrations had run. The
// import refuses a file whose tables have any other columns: a column it does
// not know is data it would drop, and a column it misses is a file the
// standalone store never finished migrating (a member keyed by name, a parent
// linked by name, a table from before denies). Start the standalone store on
// such a file once, so it migrates it, and import the result.
var standaloneColumns = map[string][]string{
	"bundles": {"id", "name", "display_name", "description", "extends_id", "match_tags", "model", "effort",
		"denied_read_paths", "enabled", "created_at", "updated_at"},
	"bundle_members": {"bundle_id", "kind", "member_id", "name", "condition", "effect"},
}

// standaloneLegacyMembersColumns are the columns of the table the standalone
// store moved name-keyed members into. Only a file that once held such
// members has it, so it is copied when present and not required.
var standaloneLegacyMembersColumns = []string{"bundle_id", "kind", "name", "condition"}

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Bundles         int   `json:"bundles"`
	Members         int   `json:"members"`
	LegacyMembers   int   `json:"legacy_members"`
	LastBundleID    int64 `json:"last_bundle_id"`
	DisabledBundles int   `json:"disabled_bundles"`
}

// ImportStandaloneDatabase copies every bundle and member from the SQLite
// file of a standalone bundle-store into the module behind db, as they are:
// ids, inheritance links, members with their effects, times and enabled
// flags, and the name-keyed members the standalone store had set aside. It
// runs in one transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// registries or run twice, and it refuses a file it cannot copy whole: a table
// with a column it does not know or lacks one it needs, an enabled flag that is
// not 0 or 1, denied read paths that are not a JSON list, or a row the schema
// refuses (a parent or bundle the file does not hold, a kind or effect outside
// the CHECKs). Afterwards bundles number on from the highest id copied, so no
// id that kanban boards and sessions already carry is handed out again.
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
		found, err := standaloneTableColumns(ctx, source, table)
		if err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		if len(found) == 0 {
			return counts, fmt.Errorf("%s has no table %s, so it is not a bundle-store database", sqlitePath, table)
		}
		if err := sameColumns(table, found, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	legacyColumns, err := standaloneTableColumns(ctx, source, LegacyMembersTable)
	if err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	hasLegacyMembers := len(legacyColumns) > 0
	if hasLegacyMembers {
		if err := sameColumns(LegacyMembersTable, legacyColumns, standaloneLegacyMembersColumns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range []string{"bundles", "bundle_members", LegacyMembersTable} {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table, err)
			}
			if held > 0 {
				return fmt.Errorf("the bundles module already holds %d rows in %s; the import copies into an empty module only", held, table)
			}
		}
		if err := importBundles(ctx, source, tx, &counts); err != nil {
			return err
		}
		if err := importMembers(ctx, source, tx, &counts); err != nil {
			return err
		}
		if hasLegacyMembers {
			if err := importLegacyMembers(ctx, source, tx, &counts); err != nil {
				return err
			}
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "bundles", "id", counts.LastBundleID)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func standaloneTableColumns(ctx context.Context, source *sql.DB, table string) ([]string, error) {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found = append(found, name)
	}
	return found, rows.Err()
}

func sameColumns(table string, found, want []string) error {
	sortedFound := append([]string{}, found...)
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedFound)
	sort.Strings(sortedWant)
	if strings.Join(sortedFound, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole, so a file the standalone store has not finished migrating must be opened by it once first",
			table, strings.Join(sortedFound, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}

// importBundles copies the bundles with no parent first, then sets each
// parent link, so a child whose parent has the higher id is copied too.
func importBundles(ctx context.Context, source *sql.DB, tx *sql.Tx, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `
		SELECT id, name, display_name, description, extends_id, match_tags, model, effort,
			denied_read_paths, enabled, created_at, updated_at
		FROM bundles ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading bundles: %w", err)
	}
	defer rows.Close()
	type link struct{ child, parent int64 }
	var links []link
	for rows.Next() {
		var b Bundle
		var extendsID sql.NullInt64
		var matchTags, deniedReadPaths string
		var enabled int64
		if err := rows.Scan(&b.ID, &b.Name, &b.DisplayName, &b.Description, &extendsID, &matchTags, &b.Model, &b.Effort,
			&deniedReadPaths, &enabled, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return fmt.Errorf("reading bundles: %w", err)
		}
		if enabled != 0 && enabled != 1 {
			return fmt.Errorf("bundle %d (%s) has enabled %d; the import knows 0 and 1 only", b.ID, b.Name, enabled)
		}
		if _, err := decodeStringsStrictly(deniedReadPaths); err != nil {
			return fmt.Errorf("bundle %d (%s): its denied_read_paths %q are not a JSON list of strings: %w", b.ID, b.Name, deniedReadPaths, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO bundles (id, name, display_name, description, match_tags, model, effort,
				denied_read_paths, enabled, created_at, updated_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			b.ID, b.Name, b.DisplayName, b.Description, matchTags, b.Model, b.Effort,
			deniedReadPaths, enabled == 1, b.CreatedAt, b.UpdatedAt); err != nil {
			return fmt.Errorf("copying bundle %d (%s): %w", b.ID, b.Name, err)
		}
		if extendsID.Valid {
			links = append(links, link{child: b.ID, parent: extendsID.Int64})
		}
		counts.Bundles++
		if enabled == 0 {
			counts.DisabledBundles++
		}
		counts.LastBundleID = b.ID
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading bundles: %w", err)
	}
	for _, l := range links {
		if _, err := tx.ExecContext(ctx, `UPDATE bundles SET extends_id = $1 WHERE id = $2`, l.parent, l.child); err != nil {
			return fmt.Errorf("copying bundle %d's link to its parent %d: %w", l.child, l.parent, err)
		}
	}
	return nil
}

func importMembers(ctx context.Context, source *sql.DB, tx *sql.Tx, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `
		SELECT bundle_id, kind, member_id, name, condition, effect
		FROM bundle_members ORDER BY bundle_id, kind, member_id`)
	if err != nil {
		return fmt.Errorf("reading bundle members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bundleID int64
		var m Member
		var kind, effect string
		if err := rows.Scan(&bundleID, &kind, &m.ID, &m.Name, &m.Condition, &effect); err != nil {
			return fmt.Errorf("reading bundle members: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO bundle_members (bundle_id, kind, member_id, name, condition, effect)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			bundleID, kind, m.ID, m.Name, m.Condition, effect); err != nil {
			return fmt.Errorf("copying %s member %d of bundle %d: %w", kind, m.ID, bundleID, err)
		}
		counts.Members++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading bundle members: %w", err)
	}
	return nil
}

func importLegacyMembers(ctx context.Context, source *sql.DB, tx *sql.Tx, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `
		SELECT bundle_id, kind, name, condition FROM `+LegacyMembersTable+` ORDER BY bundle_id, kind, name`)
	if err != nil {
		return fmt.Errorf("reading %s: %w", LegacyMembersTable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var bundleID int64
		var kind, name, condition string
		if err := rows.Scan(&bundleID, &kind, &name, &condition); err != nil {
			return fmt.Errorf("reading %s: %w", LegacyMembersTable, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO `+LegacyMembersTable+` (bundle_id, kind, name, condition) VALUES ($1, $2, $3, $4)`,
			bundleID, kind, name, condition); err != nil {
			return fmt.Errorf("copying the name-keyed %s member %q of bundle %d: %w", kind, name, bundleID, err)
		}
		counts.LegacyMembers++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", LegacyMembersTable, err)
	}
	return nil
}
