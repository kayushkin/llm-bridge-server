package journalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the importer's errors.
const standaloneStoreName = "journal-store"

// The columns of each table of the SQLite database the standalone
// journal-store kept (its schema.sql; it added none later). The import
// refuses a file whose tables have any other columns: a column it does not
// know is data it would drop. entries_fts is not copied: it is derived, and
// the module's search column is built from the copied rows.
var (
	standaloneEntryColumns = []string{"id", "seq", "kind", "title", "body", "written_at", "visibility", "published_at",
		"slug", "tags", "about_ref", "source", "source_ref", "source_hash", "created_at", "updated_at", "deleted_at"}
	standaloneEntryRevisionColumns = []string{"revision_id", "entry_id", "reason", "replaced_at", "kind", "title", "body",
		"written_at", "visibility", "published_at", "slug", "tags", "about_ref", "source", "source_ref", "source_hash",
		"created_at", "updated_at", "deleted_at"}
	standaloneMentionColumns      = []string{"entry_id", "position", "line", "entity_ref", "entity_type", "label"}
	standaloneTimelineItemColumns = []string{"id", "seq", "timeline_id", "occurred_at", "ends_at", "precision", "title",
		"body", "sources", "created_at", "updated_at", "deleted_at"}
	standaloneTimelineItemRevisionColumns = []string{"revision_id", "item_id", "reason", "replaced_at", "timeline_id",
		"occurred_at", "ends_at", "precision", "title", "body", "sources", "created_at", "updated_at", "deleted_at"}
	standaloneTimelineItemMentionColumns = []string{"item_id", "position", "entity_ref", "entity_type", "label"}
)

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Entries               int64 `json:"entries"`
	EntryRevisions        int64 `json:"entry_revisions"`
	Mentions              int64 `json:"mentions"`
	TimelineItems         int64 `json:"timeline_items"`
	TimelineItemRevisions int64 `json:"timeline_item_revisions"`
	TimelineItemMentions  int64 `json:"timeline_item_mentions"`
}

// Summary is the counts in one line, and nothing of what the rows hold.
func (counts ImportCounts) Summary() string {
	return fmt.Sprintf("entries=%d entry_revisions=%d mentions=%d timeline_items=%d timeline_item_revisions=%d timeline_item_mentions=%d",
		counts.Entries, counts.EntryRevisions, counts.Mentions, counts.TimelineItems, counts.TimelineItemRevisions, counts.TimelineItemMentions)
}

// ImportStandaloneDatabase copies every entry, revision, mention and timeline
// item from the SQLite file of a standalone journal-store into the module
// behind db, as they are — numbers, ids, times, soft-deleted rows — with every
// row owned by ownerPrincipalID: the standalone store was one person's. It
// runs in one transaction, so it copies everything or nothing.
//
// It refuses an owner that is not a principal-store id, a module that already
// holds any row (so it can never mix two journals or run twice), and a file
// it cannot copy whole: a table with a column it does not know, an id that is
// not the form its number makes, tags or sources that are not a list of
// strings, or a row pointing at a record the file does not hold. Afterwards
// each table numbers on from the highest number copied, so no id that chats,
// notes and links already carry is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (ImportCounts, error) {
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return ImportCounts{}, fmt.Errorf("every journal record needs an owner: %w", err)
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return ImportCounts{}, err
	}
	defer source.Close()
	for table, want := range map[string][]string{
		"entries":                 standaloneEntryColumns,
		"entry_revisions":         standaloneEntryRevisionColumns,
		"mentions":                standaloneMentionColumns,
		"timeline_items":          standaloneTimelineItemColumns,
		"timeline_item_revisions": standaloneTimelineItemRevisionColumns,
		"timeline_item_mentions":  standaloneTimelineItemMentionColumns,
	} {
		if err := standalonesqlite.CheckColumns(ctx, source, standaloneStoreName, table, want); err != nil {
			return ImportCounts{}, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	var counts ImportCounts
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "journal", "entries", "entry_revisions", "mentions",
			"timeline_items", "timeline_item_revisions", "timeline_item_mentions"); err != nil {
			return err
		}
		var lastEntry, lastEntryRevision, lastTimelineItem, lastTimelineItemRevision int64
		tables := []struct {
			table standalonesqlite.Table
			count *int64
		}{
			{entriesTable(ownerPrincipalID, &lastEntry), &counts.Entries},
			{entryRevisionsTable(ownerPrincipalID, &lastEntryRevision), &counts.EntryRevisions},
			{mentionsTable(ownerPrincipalID), &counts.Mentions},
			{timelineItemsTable(ownerPrincipalID, &lastTimelineItem), &counts.TimelineItems},
			{timelineItemRevisionsTable(ownerPrincipalID, &lastTimelineItemRevision), &counts.TimelineItemRevisions},
			{timelineItemMentionsTable(ownerPrincipalID), &counts.TimelineItemMentions},
		}
		for _, copying := range tables {
			copied, err := standalonesqlite.Copy(ctx, source, tx, copying.table)
			if err != nil {
				return err
			}
			*copying.count = copied
		}
		for _, numbering := range []struct {
			table, column string
			last          int64
		}{
			{"entries", "seq", lastEntry},
			{"entry_revisions", "revision_id", lastEntryRevision},
			{"timeline_items", "seq", lastTimelineItem},
			{"timeline_item_revisions", "revision_id", lastTimelineItemRevision},
		} {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbering.table, numbering.column, numbering.last); err != nil {
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

// standaloneRow reads one SQLite row by column name, keeping the first error.
type standaloneRow struct {
	columns []string
	values  []any
	err     error
}

func newStandaloneRow(columns []string, values []any) *standaloneRow {
	return &standaloneRow{columns: columns, values: values}
}

func (row *standaloneRow) value(column string) any {
	for index, name := range row.columns {
		if name == column {
			return row.values[index]
		}
	}
	panic("journalstore: the import reads column " + column + ", which its own list does not name")
}

func (row *standaloneRow) integer(column string) int64 {
	if row.err != nil {
		return 0
	}
	number, err := standalonesqlite.Integer(column, row.value(column))
	row.err = err
	return number
}

func (row *standaloneRow) text(column string) string {
	if row.err != nil {
		return ""
	}
	text, err := standalonesqlite.Text(column, row.value(column))
	row.err = err
	return text
}

// stringList reads a TEXT column holding a JSON list of strings, and answers
// it as written for a jsonb column.
func (row *standaloneRow) stringList(column string) string {
	raw := row.text(column)
	if row.err != nil {
		return ""
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil || list == nil {
		row.err = fmt.Errorf("%s is not a JSON list of strings", column)
		return ""
	}
	return raw
}

// number reads a TEXT column holding a public id of the type prefix names, and
// answers its number.
func (row *standaloneRow) number(column, prefix string) int64 {
	id := row.text(column)
	if row.err != nil {
		return 0
	}
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		row.err = fmt.Errorf("%s: %w", column, err)
	}
	return number
}

// keptID checks that the id column is the id the seq column makes: the
// import keeps ids by their number and will not change one.
func (row *standaloneRow) keptID(prefix string) int64 {
	seq := row.integer("seq")
	id := row.text("id")
	if row.err == nil && id != postgresstorage.FormatRecordID(prefix, seq) {
		row.err = fmt.Errorf("id %q has number %d, whose id is %s; the import keeps ids by their number and will not change one",
			id, seq, postgresstorage.FormatRecordID(prefix, seq))
	}
	return seq
}

func entriesTable(owner string, last *int64) standalonesqlite.Table {
	return standalonesqlite.Table{
		Name:    "entries",
		Columns: standaloneEntryColumns,
		OrderBy: "seq",
		Insert: `INSERT INTO entries (seq, owner_principal_id, kind, title, body, written_at, visibility, published_at, slug,
			tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14, $15, $16, $17)`,
		Convert: func(values []any) ([]any, error) {
			row := newStandaloneRow(standaloneEntryColumns, values)
			seq := row.keptID(EntryIDPrefix)
			converted := []any{seq, owner, row.text("kind"), row.text("title"), row.text("body"), row.integer("written_at"),
				row.text("visibility"), row.integer("published_at"), row.text("slug"), row.stringList("tags"),
				row.text("about_ref"), row.text("source"), row.text("source_ref"), row.text("source_hash"),
				row.integer("created_at"), row.integer("updated_at"), row.integer("deleted_at")}
			if row.err != nil {
				return nil, row.err
			}
			*last = max(*last, seq)
			return converted, nil
		},
	}
}

func entryRevisionsTable(owner string, last *int64) standalonesqlite.Table {
	return standalonesqlite.Table{
		Name:    "entry_revisions",
		Columns: standaloneEntryRevisionColumns,
		OrderBy: "revision_id",
		Insert: `INSERT INTO entry_revisions (revision_id, owner_principal_id, entry_seq, reason, replaced_at, kind, title, body,
			written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at,
			updated_at, deleted_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16, $17, $18, $19, $20)`,
		Convert: func(values []any) ([]any, error) {
			row := newStandaloneRow(standaloneEntryRevisionColumns, values)
			revisionID := row.integer("revision_id")
			converted := []any{revisionID, owner, row.number("entry_id", EntryIDPrefix), row.text("reason"),
				row.integer("replaced_at"), row.text("kind"), row.text("title"), row.text("body"), row.integer("written_at"),
				row.text("visibility"), row.integer("published_at"), row.text("slug"), row.stringList("tags"),
				row.text("about_ref"), row.text("source"), row.text("source_ref"), row.text("source_hash"),
				row.integer("created_at"), row.integer("updated_at"), row.integer("deleted_at")}
			if row.err != nil {
				return nil, row.err
			}
			*last = max(*last, revisionID)
			return converted, nil
		},
	}
}

func mentionsTable(owner string) standalonesqlite.Table {
	return standalonesqlite.Table{
		Name:    "mentions",
		Columns: standaloneMentionColumns,
		OrderBy: "entry_id, position",
		Insert: `INSERT INTO mentions (owner_principal_id, entry_seq, position, line, entity_ref, entity_type, label)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		Convert: func(values []any) ([]any, error) {
			row := newStandaloneRow(standaloneMentionColumns, values)
			converted := []any{owner, row.number("entry_id", EntryIDPrefix), row.integer("position"), row.integer("line"),
				row.text("entity_ref"), row.text("entity_type"), row.text("label")}
			return converted, row.err
		},
	}
}

func timelineItemsTable(owner string, last *int64) standalonesqlite.Table {
	return standalonesqlite.Table{
		Name:    "timeline_items",
		Columns: standaloneTimelineItemColumns,
		OrderBy: "seq",
		Insert: `INSERT INTO timeline_items (seq, owner_principal_id, timeline_seq, occurred_at, ends_at, precision, title, body,
			sources, created_at, updated_at, deleted_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12)`,
		Convert: func(values []any) ([]any, error) {
			row := newStandaloneRow(standaloneTimelineItemColumns, values)
			seq := row.keptID(TimelineItemIDPrefix)
			converted := []any{seq, owner, row.number("timeline_id", EntryIDPrefix), row.integer("occurred_at"),
				row.integer("ends_at"), row.text("precision"), row.text("title"), row.text("body"), row.stringList("sources"),
				row.integer("created_at"), row.integer("updated_at"), row.integer("deleted_at")}
			if row.err != nil {
				return nil, row.err
			}
			*last = max(*last, seq)
			return converted, nil
		},
	}
}

func timelineItemRevisionsTable(owner string, last *int64) standalonesqlite.Table {
	return standalonesqlite.Table{
		Name:    "timeline_item_revisions",
		Columns: standaloneTimelineItemRevisionColumns,
		OrderBy: "revision_id",
		Insert: `INSERT INTO timeline_item_revisions (revision_id, owner_principal_id, item_seq, reason, replaced_at,
			timeline_seq, occurred_at, ends_at, precision, title, body, sources, created_at, updated_at, deleted_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13, $14, $15)`,
		Convert: func(values []any) ([]any, error) {
			row := newStandaloneRow(standaloneTimelineItemRevisionColumns, values)
			revisionID := row.integer("revision_id")
			converted := []any{revisionID, owner, row.number("item_id", TimelineItemIDPrefix), row.text("reason"),
				row.integer("replaced_at"), row.number("timeline_id", EntryIDPrefix), row.integer("occurred_at"),
				row.integer("ends_at"), row.text("precision"), row.text("title"), row.text("body"), row.stringList("sources"),
				row.integer("created_at"), row.integer("updated_at"), row.integer("deleted_at")}
			if row.err != nil {
				return nil, row.err
			}
			*last = max(*last, revisionID)
			return converted, nil
		},
	}
}

func timelineItemMentionsTable(owner string) standalonesqlite.Table {
	return standalonesqlite.Table{
		Name:    "timeline_item_mentions",
		Columns: standaloneTimelineItemMentionColumns,
		OrderBy: "item_id, position",
		Insert: `INSERT INTO timeline_item_mentions (owner_principal_id, item_seq, position, entity_ref, entity_type, label)
			VALUES ($1, $2, $3, $4, $5, $6)`,
		Convert: func(values []any) ([]any, error) {
			row := newStandaloneRow(standaloneTimelineItemMentionColumns, values)
			converted := []any{owner, row.number("item_id", TimelineItemIDPrefix), row.integer("position"),
				row.text("entity_ref"), row.text("entity_type"), row.text("label")}
			return converted, row.err
		},
	}
}
