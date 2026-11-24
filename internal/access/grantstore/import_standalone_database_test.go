package grantstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store kept
// one, with made-up rows: an active grant, a revoked one beside an active row
// of the same tuple, a grant on every instance, and a gap in the numbering.
func newStandaloneDatabase(t *testing.T, extraStatements ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "grant-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := append([]string{
		`INSERT INTO grants VALUES ('grant_000001', 1, 'principal_000001', 'can_use', 'tool', '13', 'e2e', 1800000000, 0)`,
		`INSERT INTO grants VALUES ('grant_000002', 2, 'principal_000006', 'can_use', 'tool', '14', '', 1800000100, 1800000200)`,
		`INSERT INTO grants VALUES ('grant_000003', 3, 'principal_000006', 'can_use', 'tool', '14', 'again', 1800000300, 0)`,
		`INSERT INTO grants VALUES ('grant_000005', 5, 'principal_000020', 'can_dispatch_on', 'instance', '*', '', 1800000400, 0)`,
	}, extraStatements...)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryGrantAsItWasAndNumbersOnAfterIt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	counts, err := ImportStandaloneDatabase(ctx, s.db, newStandaloneDatabase(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if want := (ImportCounts{Grants: 4, RevokedGrants: 1, LastGrantSeq: 5}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	revoked, err := s.Get(ctx, "grant_000002")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.RevokedAt != 1800000200 || revoked.GrantedAt != 1800000100 || revoked.PrincipalID != "principal_000006" {
		t.Fatalf("revoked grant = %+v", revoked)
	}
	first, err := s.Get(ctx, "grant_000001")
	if err != nil || first.Note != "e2e" || first.ResourceID != "13" {
		t.Fatalf("first grant = %+v, %v", first, err)
	}
	if _, err := s.Get(ctx, "grant_000004"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the gap: %v", err)
	}
	all, _ := s.List(ctx, Filter{IncludeRevoked: true})
	if len(all) != 4 {
		t.Fatalf("rows = %d, want 4", len(all))
	}
	created := mustGrant(t, s, "principal_000002", RelationCanUse, ResourceTypeTool, "15")
	if created.ID != "grant_000006" {
		t.Fatalf("the next grant is %s, want grant_000006", created.ID)
	}
	if _, err := ImportStandaloneDatabase(ctx, s.db, newStandaloneDatabase(t)); err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	badID := newStandaloneDatabase(t, `INSERT INTO grants VALUES ('grant_7', 7, 'principal_000001', 'can_use', 'tool', '1', '', 1, 0)`)
	if _, err := ImportStandaloneDatabase(ctx, s.db, badID); err == nil || !strings.Contains(err.Error(), "keeps ids by their number") {
		t.Fatalf("a malformed id: %v", err)
	}
	extraColumn := newStandaloneDatabase(t, `ALTER TABLE grants ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`)
	if _, err := ImportStandaloneDatabase(ctx, s.db, extraColumn); err == nil || !strings.Contains(err.Error(), "copies only a table it can copy whole") {
		t.Fatalf("an unknown column: %v", err)
	}
	if counts, _ := s.Counts(ctx); counts.Grants != 0 {
		t.Fatalf("a refused import left %d grants", counts.Grants)
	}
	if _, err := ImportStandaloneDatabase(ctx, s.db, filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("a missing file was imported")
	}
}
