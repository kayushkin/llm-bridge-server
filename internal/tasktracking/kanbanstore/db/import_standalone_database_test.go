package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/model"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	// The standalone store wrote through mattn/go-sqlite3, so the files the
	// tests import are written through it too; the import reads them with
	// the pure-Go driver.
	_ "github.com/mattn/go-sqlite3"
)

// standaloneFile builds a SQLite file with the standalone store's last schema
// and runs statements in it, through the driver the standalone store wrote
// with, so its times are stored in the form that store stored them.
func standaloneFile(t *testing.T, statements ...func(*sql.DB) error) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "standalone.db")
	source, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Exec(string(schema)); err != nil {
		t.Fatalf("the standalone schema: %v", err)
	}
	for _, statement := range statements {
		if err := statement(source); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func execAll(statements ...[]any) func(*sql.DB) error {
	return func(source *sql.DB) error {
		for _, statement := range statements {
			if _, err := source.Exec(statement[0].(string), statement[1:]...); err != nil {
				return err
			}
		}
		return nil
	}
}

func emptyModule(t *testing.T) *sql.DB {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	return postgrestest.NewDatabase(t, module).Open(module)
}

func TestImportCopiesEveryRowAndKeepsIDsTimesAndNumbering(t *testing.T) {
	created := time.Date(2026, 5, 6, 20, 22, 17, 834267979, time.UTC)
	later := created.Add(time.Hour)
	taxonomy := `{"name":"support-mail","domain":"support mail","axes":[{"id":"classification_axis_000003","name":"category","values":[{"id":"classification_value_000004","name":"billing"}]}]}`
	path := standaloneFile(t, execAll(
		[]any{`INSERT INTO boards (id, name, description, archived, created_at, updated_at, organization_id) VALUES (?, ?, ?, 0, ?, ?, ?)`,
			"board-1", "Support", "made up", created, later, "principal_000900"},
		[]any{`INSERT INTO columns (id, board_id, name, position, color, wip_limit, created_at, updated_at, work_state) VALUES (?, ?, ?, ?, '', ?, ?, ?, ?)`,
			"column-1", "board-1", "Doing", 1, 3, created, later, "working"},
		[]any{`INSERT INTO placements (card_id, board_id, column_id, position, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			"card-1", "board-1", "column-1", 2.5, created, later},
		[]any{`INSERT INTO card_links (id, card_id, entity_type, entity_ref, label, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			"link-1", "card-1", "session", "br_1", "", created},
		[]any{`INSERT INTO card_events (id, card_id, board_id, kind, clock_state, detail, occurred_at, recorded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"event-1", "card-1", "board-1", "card_created", "running", `{"b": 1,  "a": 2}`, created, later},
		[]any{`INSERT INTO message_triggers (id, board_id, name, event_kind, recipient_user_id, message_template, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			"trigger-1", "board-1", "Tell someone", "card_created", "@someone:example.org", "hello", created.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano)},
		[]any{`INSERT INTO classification_identifiers (sequence, id, kind, board_id, axis_id, created_at) VALUES (3, 'classification_axis_000003', 'axis', 'board-1', '', ?)`, created},
		[]any{`INSERT INTO classification_identifiers (sequence, id, kind, board_id, axis_id, created_at) VALUES (4, 'classification_value_000004', 'value', 'board-1', 'classification_axis_000003', ?)`, created},
		// A number handed out whose row is gone: the next id must come after it.
		[]any{`UPDATE sqlite_sequence SET seq = 7 WHERE name = 'classification_identifiers'`},
		[]any{`INSERT INTO classification_taxonomy_revisions (board_id, revision, taxonomy, semantic_digest, created_at, created_by) VALUES (?, 1, ?, 'digest-1', ?, 'principal_000900')`,
			"board-1", taxonomy, created},
	))

	database := emptyModule(t)
	counts, err := ImportStandaloneDatabase(context.Background(), database, path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if counts.Boards != 1 || counts.Columns != 1 || counts.Placements != 1 || counts.CardLinks != 1 || counts.CardEvents != 1 ||
		counts.MessageTriggers != 1 || counts.ClassificationIdentifiers != 2 || counts.ClassificationTaxonomyRevisions != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	if counts.LastClassificationIdentifierSequence != 7 {
		t.Fatalf("last identifier = %d, want 7, the counter's", counts.LastClassificationIdentifierSequence)
	}

	store := New(database)
	board, err := store.GetBoard("board-1")
	if err != nil {
		t.Fatal(err)
	}
	if !board.CreatedAt.Equal(created.Truncate(time.Microsecond)) || board.CreatedAt.Location() != time.UTC {
		t.Fatalf("board created_at = %v, want %v in UTC", board.CreatedAt, created.Truncate(time.Microsecond))
	}
	if board.OrganizationID != "principal_000900" || board.TaxonomyRevision != 1 || board.Taxonomy == nil || board.Taxonomy.Axes[0].ID != "classification_axis_000003" {
		t.Fatalf("board = %+v", board)
	}
	placement, err := store.GetPlacement("card-1", "board-1")
	if err != nil || placement.Position != 2.5 {
		t.Fatalf("placement = %+v, %v", placement, err)
	}
	events, err := store.ListCardEvents("card-1", "")
	if err != nil || len(events) != 1 || string(events[0].Detail) != `{"b": 1,  "a": 2}` {
		t.Fatalf("events = %+v, %v; the detail must keep its bytes", events, err)
	}
	trigger, err := store.GetMessageTrigger("trigger-1")
	if err != nil || trigger.Enabled || !trigger.CreatedAt.Equal(created) {
		t.Fatalf("trigger = %+v, %v; its text times keep their nanoseconds", trigger, err)
	}
	var minted string
	if err := store.db.writeImmediately(func(run executor) error {
		var err error
		minted, err = mintClassificationIdentifier(run, "axis", "board-1", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if minted != "classification_axis_000008" {
		t.Fatalf("the next identifier is %s, want classification_axis_000008", minted)
	}
	cards, err := store.ListCardsByEntity("session", "br_1")
	if err != nil || len(cards) != 1 || cards[0].Placements[0].WorkState == nil || *cards[0].Placements[0].WorkState != model.WorkState("working") {
		encoded, _ := json.Marshal(cards)
		t.Fatalf("cards = %s, %v", encoded, err)
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	path := standaloneFile(t)
	database := emptyModule(t)
	if _, err := New(database).CreateBoard(&model.CreateBoardRequest{Name: "Already here"}); err != nil {
		t.Fatal(err)
	}
	_, err := ImportStandaloneDatabase(context.Background(), database, path)
	if err == nil || !strings.Contains(err.Error(), "already holds 1 rows in boards") {
		t.Fatalf("import into a module with a board = %v", err)
	}
}

func TestImportRefusesAFileMissingAColumnOrCarryingAnUnknownOne(t *testing.T) {
	missing := standaloneFile(t, execAll([]any{`ALTER TABLE tickets DROP COLUMN source_entity_ref`}))
	_, err := ImportStandaloneDatabase(context.Background(), emptyModule(t), missing)
	if err == nil || !strings.Contains(err.Error(), "table tickets lacks columns source_entity_ref") {
		t.Fatalf("import of a file missing a column = %v", err)
	}
	nameOnlyTaxonomy := standaloneFile(t, execAll([]any{`ALTER TABLE boards ADD COLUMN taxonomy TEXT`}))
	_, err = ImportStandaloneDatabase(context.Background(), emptyModule(t), nameOnlyTaxonomy)
	if err == nil || !strings.Contains(err.Error(), "table boards has columns taxonomy") {
		t.Fatalf("import of a file with a name-only taxonomy column = %v", err)
	}
}

// standaloneKanbanStoreCopy names a copy of a real standalone database to
// import, for a check by hand: it is never committed and never set in CI.
var standaloneKanbanStoreCopy = flag.String("standalone-kanban-store-copy", "", "path of a copy of a standalone kanban-store database to import and count")

func TestImportedStandaloneKanbanStoreCopy(t *testing.T) {
	if *standaloneKanbanStoreCopy == "" {
		t.Skip("no -standalone-kanban-store-copy given")
	}
	started := time.Now()
	counts, err := ImportStandaloneDatabase(context.Background(), emptyModule(t), *standaloneKanbanStoreCopy)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.MarshalIndent(counts, "", "  ")
	t.Logf("imported in %s:\n%s", time.Since(started).Round(time.Millisecond), encoded)
}
