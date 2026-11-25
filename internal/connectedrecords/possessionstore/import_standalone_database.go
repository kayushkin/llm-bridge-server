package possessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the import's errors.
const standaloneStoreName = "possession-store"

// importedTables are the module's tables an import copies, each with the
// same name and the same columns as in the standalone file, the owner
// aside. originals is filled from the originals directory instead.
var importedTables = []string{
	"possessions", "garments", "computers", "computer_parts", "audio_devices", "musical_instruments", "vehicles",
	"vehicle_odometer_readings", "furniture", "fitness_equipment", "sports_gear", "tabletop_games", "media", "collectibles",
	"art_and_decor", "home_textiles", "appliances", "personal_care", "bags_and_cases", "electronic_accessories",
	"household_tools", "music_accessories", "outfits", "jewelry", "watches", "eyewear", "outfit_garments", "outfit_wearings",
	"outfit_possessions", "products", "purchases", "purchase_lines", "photo_reviews", "model_readings", "sightings",
	"possession_photos", "owner_questions", "possession_merges", "outfit_merges", "wearable_groups", "outfit_looks",
	"outfit_look_photos", "outfit_slots", "outfit_slot_photos", "outfit_rebuilds", "revisions", "outfit_cover_choices",
	"outfit_cover_wearer_boxes",
}

// ImportCounts says how many rows of each table an import copied, and how
// many originals it carried into file-store.
type ImportCounts struct {
	Rows      map[string]int64 `json:"rows"`
	Originals int64            `json:"originals"`
}

// Summary is the counts in one line, and nothing of the records.
func (counts ImportCounts) Summary() string {
	tables := make([]string, 0, len(counts.Rows))
	for table := range counts.Rows {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	parts := make([]string, 0, len(tables)+1)
	for _, table := range tables {
		parts = append(parts, table+"="+strconv.FormatInt(counts.Rows[table], 10))
	}
	parts = append(parts, "originals="+strconv.FormatInt(counts.Originals, 10))
	return strings.Join(parts, " ")
}

// standaloneOriginal is one file of the standalone store's originals/.
type standaloneOriginal struct {
	sha256, extension, path string
	byteCount, modifiedAt   int64
}

// standaloneOriginals lists the originals directory: every file must be
// <sha256>.<extension> of a kind the store keeps, and nothing else may be
// there.
func standaloneOriginals(directory string) ([]standaloneOriginal, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("the originals directory: %w", err)
	}
	originals := []standaloneOriginal{}
	for _, entry := range entries {
		name := entry.Name()
		extension := strings.ToLower(filepath.Ext(name))
		sum := strings.TrimSuffix(name, filepath.Ext(name))
		if _, known := OriginalMediaTypes[extension]; !known || !sha256Pattern.MatchString(sum) {
			return nil, fmt.Errorf("the originals directory holds %q, which is not <sha256>.<jpg|jpeg|png|webp>", name)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("original %s is not a regular file", name)
		}
		originals = append(originals, standaloneOriginal{sha256: sum, extension: extension, path: filepath.Join(directory, name),
			byteCount: info.Size(), modifiedAt: info.ModTime().Unix()})
	}
	return originals, nil
}

// moduleColumns are a table's columns in the module, in order, without the
// owner, which the import sets.
func moduleColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name <> 'owner_principal_id' ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("the module has no table %s", table)
	}
	return columns, rows.Err()
}

// copiedTable is the copy of one table: every column, owned by owner.
func copiedTable(table string, columns []string, owner string) standalonesqlite.Table {
	placeholders := make([]string, len(columns)+1)
	for index := range placeholders {
		placeholders[index] = "$" + strconv.Itoa(index+1)
	}
	orderBy := columns[0]
	if contains(columns, "seq") {
		orderBy = "seq"
	}
	return standalonesqlite.Table{
		Name:    table,
		Columns: columns,
		OrderBy: orderBy,
		Insert: "INSERT INTO " + table + " (owner_principal_id, " + strings.Join(columns, ", ") + ") OVERRIDING SYSTEM VALUE VALUES (" +
			strings.Join(placeholders, ", ") + ")",
		Convert: func(row []any) ([]any, error) { return append([]any{owner}, row...), nil },
	}
}

// ImportStandaloneDatabase copies every row of a standalone possession-store's
// SQLite file into the module behind root, as it is — ids, numbers, times,
// JSON text — every row owned by ownerPrincipalID, and carries each original
// in originalsDirectory (<sha256>.<extension>, the standalone data
// directory's originals/) into file-store.
//
// The originals are carried first, because file-store is another module and
// cannot share the rows' transaction; the rows are then copied in one
// transaction, whole or not at all, and on any failure every file already
// carried is destroyed again.
//
// It refuses an owner that is not a principal-store id, a module that holds
// any row of any owner, a table whose columns differ from the module's, and
// a row the module would not take: an id that is not the form its number
// makes, or a row pointing at one the file does not hold. Afterwards every
// numbered table numbers on from the highest number copied, so no id that
// chats, notes and links already carry is handed out again.
func ImportStandaloneDatabase(ctx context.Context, root *Store, sqlitePath, originalsDirectory, ownerPrincipalID string) (ImportCounts, error) {
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return ImportCounts{}, err
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return ImportCounts{}, err
	}
	defer source.Close()
	every, err := root.Narrowed(ctx, ownership.EveryOwner())
	if err != nil {
		return ImportCounts{}, err
	}
	owned, err := root.NarrowedToOwner(ctx, ownerPrincipalID)
	if err != nil {
		return ImportCounts{}, err
	}
	allTables := append(append([]string{}, importedTables...), "originals")
	tables := []standalonesqlite.Table{}
	err = postgresstorage.InTransaction(ctx, every.db.pool, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, ModuleSchema, allTables...); err != nil {
			return err
		}
		for _, table := range importedTables {
			columns, err := moduleColumns(ctx, tx, table)
			if err != nil {
				return err
			}
			if err := standalonesqlite.CheckColumns(ctx, source, standaloneStoreName, table, columns); err != nil {
				return fmt.Errorf("%s: %w", sqlitePath, err)
			}
			tables = append(tables, copiedTable(table, columns, ownerPrincipalID))
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	toCarry, err := standaloneOriginals(originalsDirectory)
	if err != nil {
		return ImportCounts{}, err
	}

	cleanupContext := context.WithoutCancel(ctx)
	carried := map[string]string{}
	failWith := func(cause error) error {
		failures := []error{fmt.Errorf("importing %s: %w", sqlitePath, cause)}
		for sum, fileID := range carried {
			if err := root.originals.Destroy(cleanupContext, fileID); err != nil {
				failures = append(failures, fmt.Errorf("destroying file %s, carried for original %s by this failed import: %w", fileID, sum, err))
			}
		}
		return errors.Join(failures...)
	}
	for _, original := range toCarry {
		if _, twice := carried[original.sha256]; twice {
			return ImportCounts{}, failWith(fmt.Errorf("the originals directory holds %s twice, under two extensions", original.sha256))
		}
		fileID, err := root.originals.KeepCopyOf(ctx, ownerPrincipalID, original.sha256, OriginalMediaTypes[original.extension], original.path)
		if err != nil {
			return ImportCounts{}, failWith(fmt.Errorf("carrying original %s: %w", original.sha256, err))
		}
		carried[original.sha256] = fileID
	}

	counts := ImportCounts{Rows: map[string]int64{}}
	err = postgresstorage.InTransaction(ctx, owned.db.pool, func(tx *sql.Tx) error {
		// Rows point at one another in both directions (a possession merged
		// into a later one), so the keys are checked once every row is in.
		if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
			return err
		}
		for _, table := range tables {
			count, err := standalonesqlite.Copy(ctx, source, tx, table)
			if err != nil {
				return err
			}
			counts.Rows[table.Name] = count
		}
		for _, original := range toCarry {
			if _, err := tx.ExecContext(ctx, `INSERT INTO originals (owner_principal_id, sha256, extension, media_type, file_id, byte_count, added_by, added_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, ownerPrincipalID, original.sha256, original.extension, OriginalMediaTypes[original.extension],
				carried[original.sha256], original.byteCount, standaloneImportActor, original.modifiedAt); err != nil {
				return fmt.Errorf("recording original %s: %w", original.sha256, err)
			}
			counts.Originals++
		}
		for _, table := range tables {
			column := ""
			switch {
			case contains(table.Columns, "seq"):
				column = "seq"
			case table.Name == "revisions":
				column = "id"
			default:
				continue
			}
			var last int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX("+column+"), 0) FROM "+table.Name).Scan(&last); err != nil {
				return fmt.Errorf("reading the highest number in %s: %w", table.Name, err)
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table.Name, column, last); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			return fmt.Errorf("a copied row points at one the file does not hold: %w", err)
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, failWith(err)
	}
	return counts, nil
}

// standaloneImportActor is added_by on every original an import carried: the
// standalone store kept no record of who added a file.
const standaloneImportActor = "import-possession-store"
