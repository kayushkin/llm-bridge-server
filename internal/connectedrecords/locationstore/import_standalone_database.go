package locationstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// StandaloneDatabaseImportCounts says how many rows of each table an import
// of a standalone location-store copied. (ImportCounts is the name of what
// an import batch did, a wire type of the routes.)
type StandaloneDatabaseImportCounts struct {
	Imports              int64 `json:"imports"`
	Places               int64 `json:"places"`
	Visits               int64 `json:"visits"`
	Trips                int64 `json:"trips"`
	TripPathPoints       int64 `json:"trip_path_points"`
	Points               int64 `json:"points"`
	Revisions            int64 `json:"revisions"`
	HomePeriods          int64 `json:"home_periods"`
	ImportRecords        int64 `json:"import_records"`
	CalendarEventLinks   int64 `json:"calendar_event_links"`
	CalendarSeriesPlaces int64 `json:"calendar_series_places"`
	PlaceSuggestions     int64 `json:"place_suggestions"`
}

// Summary names each count, and nothing of the records.
func (counts StandaloneDatabaseImportCounts) Summary() string {
	return fmt.Sprintf("imports=%d places=%d visits=%d trips=%d trip_path_points=%d points=%d revisions=%d home_periods=%d import_records=%d calendar_event_links=%d calendar_series_places=%d place_suggestions=%d",
		counts.Imports, counts.Places, counts.Visits, counts.Trips, counts.TripPathPoints, counts.Points, counts.Revisions, counts.HomePeriods,
		counts.ImportRecords, counts.CalendarEventLinks, counts.CalendarSeriesPlaces, counts.PlaceSuggestions)
}

// standaloneTables are the tables of the standalone store's SQLite file, with
// their columns as they ran live: schema.sql and the columns its addColumns
// migration added. Each is copied into the module's table of the same name.
var standaloneTables = []struct {
	name    string
	columns []string
	// orderBy orders the read: by number, or for the tables with no number
	// by the order SQLite wrote them in, which the copy keeps.
	orderBy string
}{
	{"imports", []string{"id", "seq", "kind", "sha256", "source_machine", "source_path", "imported_at", "finished_at", "counts",
		"snapshot", "snapshot_changes", "recorded_by"}, "seq"},
	{"places", []string{"id", "seq", "name", "latitude", "longitude", "radius_meters", "address", "kind", "google_place_id", "description",
		"described_by", "tags", "source", "import_id", "created_at", "updated_at", "deleted_at", "locality", "merged_into_place_id"}, "seq"},
	{"visits", []string{"id", "seq", "place_id", "started_at", "ended_at", "latitude", "longitude", "probability", "source_semantic_type",
		"description", "described_by", "tags", "source", "source_ref", "import_id", "created_at", "updated_at", "deleted_at", "locality",
		"superseded_by_import_id"}, "seq"},
	{"trips", []string{"id", "seq", "parent_trip_id", "started_at", "ended_at", "start_latitude", "start_longitude", "end_latitude",
		"end_longitude", "start_place_id", "end_place_id", "mode", "source_mode", "distance_meters", "probability", "title", "description",
		"described_by", "tags", "source", "source_ref", "import_id", "created_at", "updated_at", "deleted_at", "start_locality",
		"end_locality", "kind", "title_by", "superseded_by_import_id", "parent_set_by"}, "seq"},
	{"trip_path_points", []string{"trip_id", "position", "latitude", "longitude", "timestamp"}, "trip_id, position"},
	{"points", []string{"timestamp", "latitude", "longitude", "accuracy_meters", "source", "source_ref", "import_id"}, "rowid"},
	{"revisions", []string{"revision_id", "entity_type", "entity_id", "reason", "replaced_at", "row_json"}, "revision_id"},
	{"home_periods", []string{"id", "seq", "place_id", "latitude", "longitude", "locality", "started_at", "ended_at", "nights_known",
		"nights_at_home", "described_by", "created_at", "updated_at", "deleted_at"}, "seq"},
	{"import_records", []string{"import_id", "entity_type", "entity_id"}, "import_id, entity_type, entity_id"},
	{"calendar_event_links", []string{"id", "seq", "calendar_id", "event_id", "recurring_event_id", "summary", "location", "started_at",
		"ended_at", "all_day", "target_type", "target_id", "rule", "confidence", "reason", "matched_by", "created_at", "updated_at",
		"deleted_at", "deleted_by"}, "seq"},
	{"calendar_series_places", []string{"id", "seq", "calendar_id", "recurring_event_id", "summary", "place_id", "spot_place_ids",
		"instances", "instances_out", "instances_at_place", "agreement", "confidence", "outcome", "first_instance_at", "last_instance_at",
		"window_from", "window_to", "matched_by", "created_at", "updated_at"}, "seq"},
	{"place_suggestions", []string{"id", "seq", "place_id", "source", "source_ref", "name", "address", "kind", "distance_meters", "reason",
		"evidence", "status", "proposed_by", "decided_by", "decided_at", "decision_note", "created_at", "updated_at", "deleted_at"}, "seq"},
}

// standaloneIDPrefixes is the id prefix of each numbered table.
var standaloneIDPrefixes = map[string]string{
	"imports": "import", "places": "place", "visits": "visit", "trips": "trip", "home_periods": "home_period",
	"calendar_event_links": "calendar_event_link", "calendar_series_places": "calendar_series_place", "place_suggestions": "place_suggestion",
}

// copyBatchParameters bounds the placeholders of one multi-row insert, well
// under Postgres' 65535.
const copyBatchParameters = 30000

// ImportStandaloneDatabase copies every row of the standalone location-store's
// SQLite file at sqlitePath into the module behind db, every record owned by
// ownerPrincipalID, with every id, time and soft-deleted row as it was. It
// runs in one transaction, so it copies everything or nothing, and writes
// many rows per statement, since a store holds hundreds of thousands of
// points.
//
// It refuses an empty or malformed owner, a module that already holds any
// row, and a file it cannot copy whole: a table whose columns are not the
// live standalone schema's, or an id that is not the form its number makes.
// A row pointing at a record the file does not hold fails the module's
// foreign keys, and so the import. Each revision's row gains the owner, as
// one taken now would carry it. Each numbered table numbers on after the
// highest number copied, so no id that chats, notes and links already carry
// is handed out again. The store keeps no derived search column, so nothing
// is rebuilt.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (StandaloneDatabaseImportCounts, error) {
	var counts StandaloneDatabaseImportCounts
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return counts, err
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	moduleTables := []string{}
	for _, table := range standaloneTables {
		if err := standalonesqlite.CheckColumns(ctx, source, "location-store", table.name, table.columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		moduleTables = append(moduleTables, table.name)
	}
	countOf := map[string]*int64{
		"imports": &counts.Imports, "places": &counts.Places, "visits": &counts.Visits, "trips": &counts.Trips,
		"trip_path_points": &counts.TripPathPoints, "points": &counts.Points, "revisions": &counts.Revisions,
		"home_periods": &counts.HomePeriods, "import_records": &counts.ImportRecords, "calendar_event_links": &counts.CalendarEventLinks,
		"calendar_series_places": &counts.CalendarSeriesPlaces, "place_suggestions": &counts.PlaceSuggestions,
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "locations", moduleTables...); err != nil {
			return err
		}
		// A place may name a later place it was merged into, and a movement a
		// later journey; those two keys are checked at commit.
		if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
			return err
		}
		last := map[string]int64{}
		for _, table := range standaloneTables {
			convert, err := standaloneConversion(table.name, table.columns, ownerPrincipalID, last)
			if err != nil {
				return err
			}
			copied, err := copyStandaloneTable(ctx, source, tx, table.name, table.columns, table.orderBy, convert)
			if err != nil {
				return err
			}
			*countOf[table.name] = copied
		}
		for table := range standaloneIDPrefixes {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table, "seq", last[table]); err != nil {
				return err
			}
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "revisions", "revision_id", last["revisions"])
	})
	if err != nil {
		return StandaloneDatabaseImportCounts{}, err
	}
	return counts, nil
}

// standaloneConversion is how one table's rows become the module's: the
// columns written, and a function from the values read to the values
// written. It keeps the highest number of each numbered table in last.
func standaloneConversion(table string, columns []string, owner string, last map[string]int64) (standaloneConvert, error) {
	position := map[string]int{}
	for index, column := range columns {
		position[column] = index
	}
	written := append([]string{"owner_principal_id"}, columns...)
	jsonbColumns := map[string]bool{"tags": true, "spot_place_ids": true}
	booleanColumns := map[string]bool{"all_day": true}
	convert := standaloneConvert{columns: written, jsonb: jsonbColumns}
	prefix, numbered := standaloneIDPrefixes[table]
	convert.values = func(row []any) ([]any, error) {
		values := append([]any{owner}, row...)
		if numbered {
			id, err := standalonesqlite.Text("id", row[position["id"]])
			if err != nil {
				return nil, err
			}
			seq, err := standalonesqlite.Integer("seq", row[position["seq"]])
			if err != nil {
				return nil, err
			}
			if id != postgresstorage.FormatRecordID(prefix, seq) {
				return nil, fmt.Errorf("id %q has number %d, whose id is %s; the import keeps ids by their number and will not change one",
					id, seq, postgresstorage.FormatRecordID(prefix, seq))
			}
			last[table] = max(last[table], seq)
		}
		for column, index := range position {
			switch {
			case booleanColumns[column]:
				value, err := standalonesqlite.Boolean(column, row[index])
				if err != nil {
					return nil, err
				}
				values[index+1] = value
			case jsonbColumns[column]:
				text, err := standalonesqlite.Text(column, row[index])
				if err != nil {
					return nil, err
				}
				var array []string
				if err := json.Unmarshal([]byte(text), &array); err != nil {
					return nil, fmt.Errorf("%s is not a JSON array of strings: %w", column, err)
				}
				values[index+1] = text
			}
		}
		if table == "revisions" {
			revisionID, err := standalonesqlite.Integer("revision_id", row[position["revision_id"]])
			if err != nil {
				return nil, err
			}
			last[table] = max(last[table], revisionID)
			rowJSON, err := standalonesqlite.Text("row_json", row[position["row_json"]])
			if err != nil {
				return nil, err
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rowJSON), &fields); err != nil {
				return nil, fmt.Errorf("row_json is not a JSON object: %w", err)
			}
			encodedOwner, err := json.Marshal(owner)
			if err != nil {
				return nil, err
			}
			fields["owner_principal_id"] = encodedOwner
			withOwner, err := json.Marshal(fields)
			if err != nil {
				return nil, err
			}
			values[position["row_json"]+1] = string(withOwner)
		}
		return values, nil
	}
	return convert, nil
}

// standaloneConvert is one table's conversion; see standaloneConversion.
type standaloneConvert struct {
	columns []string
	jsonb   map[string]bool
	values  func(row []any) ([]any, error)
}

// copyStandaloneTable reads every row of table from source in order and
// writes them into the module's table of the same name, many rows per
// statement. It answers how many it copied; any error stops it, naming the
// row by its first column.
func copyStandaloneTable(ctx context.Context, source *sql.DB, tx *sql.Tx, table string, columns []string, orderBy string, convert standaloneConvert) (int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT "+strings.Join(columns, ", ")+" FROM "+table+" ORDER BY "+orderBy)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", table, err)
	}
	defer rows.Close()
	rowsPerStatement := copyBatchParameters / len(convert.columns)
	var pending []any
	pendingRows := 0
	var copied int64
	flush := func() error {
		if pendingRows == 0 {
			return nil
		}
		var statement strings.Builder
		statement.WriteString("INSERT INTO " + table + " (" + strings.Join(convert.columns, ", ") + ") OVERRIDING SYSTEM VALUE VALUES ")
		for rowIndex := 0; rowIndex < pendingRows; rowIndex++ {
			if rowIndex > 0 {
				statement.WriteString(", ")
			}
			statement.WriteString("(")
			for columnIndex, column := range convert.columns {
				if columnIndex > 0 {
					statement.WriteString(", ")
				}
				statement.WriteString("$" + strconv.Itoa(rowIndex*len(convert.columns)+columnIndex+1))
				if convert.jsonb[column] {
					statement.WriteString("::jsonb")
				}
			}
			statement.WriteString(")")
		}
		if _, err := tx.ExecContext(ctx, statement.String(), pending...); err != nil {
			return fmt.Errorf("copying %d rows of %s ending at row %d: %w", pendingRows, table, copied, err)
		}
		pending, pendingRows = pending[:0], 0
		return nil
	}
	for rows.Next() {
		row := make([]any, len(columns))
		pointers := make([]any, len(row))
		for index := range row {
			pointers[index] = &row[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return copied, fmt.Errorf("reading row %d of %s: %w", copied+1, table, err)
		}
		values, err := convert.values(row)
		if err != nil {
			return copied, fmt.Errorf("%s row %s=%v: %w", table, columns[0], row[0], err)
		}
		pending = append(pending, values...)
		pendingRows++
		copied++
		if pendingRows == rowsPerStatement {
			if err := flush(); err != nil {
				return copied, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("reading %s: %w", table, err)
	}
	return copied, flush()
}
