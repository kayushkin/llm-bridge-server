package calendarstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the import's errors.
const standaloneStoreName = "calendar-store"

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Calendars   int64 `json:"calendars"`
	Imports     int64 `json:"imports"`
	Events      int64 `json:"events"`
	Attendees   int64 `json:"attendees"`
	PeopleLinks int64 `json:"people_links"`
	Revisions   int64 `json:"revisions"`
}

// Summary is the counts in one line, and nothing of the records.
func (counts ImportCounts) Summary() string {
	return fmt.Sprintf("calendars=%d imports=%d events=%d attendees=%d people_links=%d revisions=%d",
		counts.Calendars, counts.Imports, counts.Events, counts.Attendees, counts.PeopleLinks, counts.Revisions)
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

// standaloneTables are the tables of the standalone file in the order they
// are copied (each after what it points at), with the live schema's columns:
// schema.sql's and those its migrate() added.
func standaloneTables(owner string) []standalonesqlite.Table {
	return []standalonesqlite.Table{
		{
			Name:    "calendars",
			Columns: []string{"seq", "id", "name", "source", "source_ref", "created_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO calendars (seq, owner_principal_id, name, source, source_ref, created_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(CalendarIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				return []any{seq, owner, row[2], row[3], row[4], row[5]}, nil
			},
		},
		{
			Name: "calendar_imports",
			Columns: []string{"seq", "id", "calendar_id", "file_name", "format", "file_sha256", "created", "updated",
				"unchanged", "people_linked", "skipped", "imported_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO calendar_imports (seq, owner_principal_id, calendar_seq, file_name, format, file_sha256,
				created, updated, unchanged, people_linked, skipped, imported_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(ImportIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				calendarSeq, err := standaloneReference("calendar_id", CalendarIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner, calendarSeq}, row[3:]...), nil
			},
		},
		{
			Name: "calendar_events",
			Columns: []string{"seq", "id", "calendar_id", "uid", "recurrence_id", "source_event_id", "summary", "description",
				"location", "starts_at", "ends_at", "all_day", "start_date", "end_date", "time_zone", "recurrence_rule",
				"status", "transparency", "organizer_email", "organizer_name", "source_created_at", "source_modified_at",
				"sequence", "raw", "raw_hash", "import_id", "created_at", "updated_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO calendar_events (seq, owner_principal_id, calendar_seq, uid, recurrence_id, source_event_id,
				summary, description, location, starts_at, ends_at, all_day, start_date, end_date, time_zone, recurrence_rule,
				status, transparency, organizer_email, organizer_name, source_created_at, source_modified_at,
				sequence, raw, raw_hash, import_seq, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
				$17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(EventIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				calendarSeq, err := standaloneReference("calendar_id", CalendarIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				allDay, err := standalonesqlite.Boolean("all_day", row[11])
				if err != nil {
					return nil, err
				}
				importSeq, err := standaloneReference("import_id", ImportIDPrefix, row[25])
				if err != nil {
					return nil, err
				}
				values := append([]any{seq, owner, calendarSeq}, row[3:11]...)
				values = append(values, allDay)
				values = append(values, row[12:25]...)
				return append(values, importSeq, row[26], row[27]), nil
			},
		},
		{
			Name:    "calendar_event_attendees",
			Columns: []string{"event_id", "position", "email", "name", "role", "participation_status"},
			OrderBy: "event_id, position",
			Insert: `INSERT INTO calendar_event_attendees (owner_principal_id, event_seq, position, email, name, role, participation_status)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			Convert: func(row []any) ([]any, error) {
				eventSeq, err := standaloneReference("event_id", EventIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				return append([]any{owner, eventSeq}, row[1:]...), nil
			},
		},
		{
			Name:    "calendar_event_people",
			Columns: []string{"event_id", "person_id", "linked_by", "reason", "linked_at"},
			OrderBy: "event_id, person_id",
			Insert: `INSERT INTO calendar_event_people (owner_principal_id, event_seq, person_id, linked_by, reason, linked_at)
				VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				eventSeq, err := standaloneReference("event_id", EventIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				return append([]any{owner, eventSeq}, row[1:]...), nil
			},
		},
		{
			Name:    "calendar_event_revisions",
			Columns: []string{"seq", "event_id", "reason", "replaced_at", "event_json"},
			OrderBy: "seq",
			Insert: `INSERT INTO calendar_event_revisions (seq, owner_principal_id, event_seq, reason, replaced_at, event_json)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standalonesqlite.Integer("seq", row[0])
				if err != nil {
					return nil, err
				}
				eventSeq, err := standaloneReference("event_id", EventIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner, eventSeq}, row[2:]...), nil
			},
		},
	}
}

// numberedTables are the tables whose seq is an identity column, numbered on
// after the import.
var numberedTables = []string{"calendars", "calendar_imports", "calendar_events", "calendar_event_revisions"}

// ImportStandaloneDatabase copies every row of a standalone calendar-store's
// SQLite file into the module behind db, as it is — numbers, ids, times and
// raw text — with every row owned by ownerPrincipalID. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses an owner that is not a principal-store id, a module that already
// holds any row (so it can never mix two stores or run twice), and a file it
// cannot copy whole: a table with a column it does not know, an id that is not
// the form its number makes, or a row pointing at one the file does not hold.
// Afterwards each numbered table numbers on from the highest number copied, so
// no id that chats, notes and links already carry is handed out again.
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
	copied := map[string]int64{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "calendar", tableNames...); err != nil {
			return err
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
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) FROM "+table).Scan(&last); err != nil {
				return fmt.Errorf("reading the highest number in %s: %w", table, err)
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table, "seq", last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, fmt.Errorf("importing %s: %w", sqlitePath, err)
	}
	counts = ImportCounts{
		Calendars:   copied["calendars"],
		Imports:     copied["calendar_imports"],
		Events:      copied["calendar_events"],
		Attendees:   copied["calendar_event_attendees"],
		PeopleLinks: copied["calendar_event_people"],
		Revisions:   copied["calendar_event_revisions"],
	}
	return counts, nil
}
