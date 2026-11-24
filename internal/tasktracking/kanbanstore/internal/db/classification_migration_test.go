package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// legacyStore makes a store, then puts it back in the shape it had before
// taxonomy revisions: a taxonomy column on boards holding a name-only taxonomy.
func legacyStore(t *testing.T, storedTaxonomy string) (path, boardID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "legacy.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	board, err := store.CreateBoard(&model.CreateBoardRequest{Name: "Support"})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, statement := range []string{
		`ALTER TABLE boards ADD COLUMN taxonomy TEXT`,
		`DELETE FROM classification_taxonomy_revisions`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`UPDATE boards SET taxonomy = ? WHERE id = ?`, storedTaxonomy, board.ID); err != nil {
		t.Fatal(err)
	}
	return path, board.ID
}

const legacyTaxonomy = `{"name":"support-mail","domain":"support mail","axes":[
	{"name":"category","required":true,"values":[{"name":"billing"},{"name":"delivery"}]},
	{"name":"urgency","values":[{"name":"today"}]}]}`

func TestANameOnlyTaxonomyMovesToRevisionOneWithIDsOnce(t *testing.T) {
	path, boardID := legacyStore(t, legacyTaxonomy)
	store, err := New(path)
	if err != nil {
		t.Fatalf("open the legacy store: %v", err)
	}
	board, err := store.GetBoard(boardID)
	if err != nil {
		t.Fatal(err)
	}
	if board.TaxonomyRevision != 1 || board.Taxonomy == nil || len(board.Taxonomy.Axes) != 2 {
		t.Fatalf("migrated board: %+v", board)
	}
	for _, axis := range board.Taxonomy.Axes {
		if !strings.HasPrefix(axis.ID, "classification_axis_") {
			t.Fatalf("axis %q got id %q", axis.Name, axis.ID)
		}
		for _, value := range axis.Values {
			if !strings.HasPrefix(value.ID, "classification_value_") {
				t.Fatalf("value %q got id %q", value.Name, value.ID)
			}
		}
	}
	var mapped int
	var createdBy string
	store.db.QueryRow(`SELECT COUNT(*) FROM classification_taxonomy_migrations WHERE board_id = ?`, boardID).Scan(&mapped)
	store.db.QueryRow(`SELECT created_by FROM classification_taxonomy_revisions WHERE board_id = ?`, boardID).Scan(&createdBy)
	if mapped != 5 || createdBy != migrationPrincipal {
		t.Fatalf("mapping rows %d (want 5: two axes, three values), created_by %q", mapped, createdBy)
	}
	var billingID string
	store.db.QueryRow(`SELECT assigned_id FROM classification_taxonomy_migrations WHERE board_id = ? AND axis_name = 'category' AND value_name = 'billing'`, boardID).Scan(&billingID)
	if billingID != board.Taxonomy.Axes[0].Values[0].ID {
		t.Fatalf("the mapping says billing is %s, the taxonomy says %s", billingID, board.Taxonomy.Axes[0].Values[0].ID)
	}
	if present, _ := columnExists(store.db, "boards", "taxonomy"); present {
		t.Fatal("boards.taxonomy is still there")
	}
	store.Close()

	again, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	reopened, _ := again.GetBoard(boardID)
	if reopened.TaxonomyRevision != 1 || reopened.Taxonomy.Axes[0].ID != board.Taxonomy.Axes[0].ID {
		t.Fatalf("a second start changed the migrated taxonomy: %+v", reopened)
	}
}

func TestAStoredTaxonomyThatDoesNotParseStopsTheStart(t *testing.T) {
	path, boardID := legacyStore(t, `{"name":`)
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), boardID) {
		t.Fatalf("want an error naming board %s, got %v", boardID, err)
	}
	raw, _ := sql.Open("sqlite3", path)
	defer raw.Close()
	if present, _ := columnExists(raw, "boards", "taxonomy"); !present {
		t.Fatal("a failed migration dropped the column anyway")
	}
}
