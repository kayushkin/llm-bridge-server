package predictionstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows: a resolved prediction with a revision and
// a link, a void one, a soft-deleted one, and gaps in every numbering.
func newStandaloneDatabase(t *testing.T) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prediction-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO predictions VALUES ('prediction_000001', 1, 'the cache halves start time', 'time ten starts before and after', 'will-fix', '["cache","startup"]', 'measured', 'agent-a', 0.9, 0.6, 1900000000, 'resolved', 'true', 1800000500, 'it did', 1800000000, 1800000500, 0)`,
		`INSERT INTO predictions VALUES ('prediction_000002', 2, 'the job was canceled', 'read the job row', 'behavior', '', 'guessed', '', 0.3, 0.3, 0, 'void', '', 1800001000, 'canceled', 1800000900, 1800001000, 0)`,
		`INSERT INTO predictions VALUES ('prediction_000004', 4, 'a deleted claim', 'nothing', 'external', '[]', 'read', 'agent-b', 0.05, 0.05, 0, 'open', '', 0, '', 1800002000, 1800003000, 1800003000)`,
		`INSERT INTO estimates VALUES (3, 'prediction_000001', 0.6, 'initial estimate', 'inferred', 'agent-a', 1800000000)`,
		`INSERT INTO estimates VALUES (5, 'prediction_000001', 0.9, 'measured it', 'measured', 'agent-a', 1800000100)`,
		`INSERT INTO estimates VALUES (6, 'prediction_000002', 0.3, 'initial estimate', 'guessed', '', 1800000900)`,
		`INSERT INTO estimates VALUES (9, 'prediction_000004', 0.05, 'initial estimate', 'read', 'agent-b', 1800002000)`,
		`INSERT INTO links VALUES (7, 'prediction_000001', 'note', 'f1db12e8-6857-46d9-80ae-9bc4e0d7c200', 'the todo', 1800000050)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	s := newTestStore(t)
	counts, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Predictions: 3, DeletedPredictions: 1, Estimates: 4, Links: 1, LastPredictionSeq: 4, LastEstimateID: 9, LastLinkID: 7}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	first, err := s.Get(testContext, "prediction_000001")
	if err != nil {
		t.Fatal(err)
	}
	if first.Claim != "the cache halves start time" || first.Probability != 0.9 || first.InitialProbability != 0.6 ||
		first.Status != "resolved" || first.Outcome != "true" || first.ResolvedAt != 1800000500 ||
		first.CreatedAt != 1800000000 || first.UpdatedAt != 1800000500 || first.DueAt != 1900000000 ||
		strings.Join(first.Tags, ",") != "cache,startup" || first.Author != "agent-a" || first.ResolutionNote != "it did" {
		t.Fatalf("prediction_000001 = %+v", first)
	}
	if len(first.Estimates) != 2 || first.Estimates[0].ID != 3 || first.Estimates[1].ID != 5 || first.Estimates[1].Rationale != "measured it" {
		t.Fatalf("estimates = %+v", first.Estimates)
	}
	if len(first.Links) != 1 || first.Links[0].ID != 7 || first.Links[0].Label != "the todo" || first.Links[0].CreatedAt != 1800000050 {
		t.Fatalf("links = %+v", first.Links)
	}
	void, err := s.Get(testContext, "prediction_000002")
	if err != nil {
		t.Fatal(err)
	}
	if void.Status != "void" || void.Outcome != "" || len(void.Tags) != 0 {
		t.Fatalf("prediction_000002 = %+v; an empty tags column is an empty list", void)
	}
	deleted, err := s.Get(testContext, "prediction_000004")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedAt != 1800003000 {
		t.Fatalf("soft-deleted row came back with deleted_at %d", deleted.DeletedAt)
	}
	if live, _ := s.Count(testContext, Filter{}); live != 2 {
		t.Fatalf("live predictions = %d, want 2: the deleted one stays deleted", live)
	}

	// The next of each is numbered after the highest copied, never in a gap.
	created := mustCreate(t, s, samplePrediction())
	if created.ID != "prediction_000005" {
		t.Fatalf("next prediction = %s, want prediction_000005", created.ID)
	}
	if created.Estimates[0].ID != 10 {
		t.Fatalf("next estimate = %d, want 10", created.Estimates[0].ID)
	}
	link, err := s.AddLink(testContext, created.ID, &Link{EntityType: "note", EntityRef: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if link.ID != 8 {
		t.Fatalf("next link = %d, want 8", link.ID)
	}
	if found, _ := s.List(testContext, Filter{Query: "cache"}); len(found) != 1 || found[0].ID != "prediction_000001" {
		t.Fatalf("search over imported rows found %d", len(found))
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, samplePrediction())
	_, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t))
	if err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("import into a module with a row = %v, want a refusal", err)
	}
	if n, _ := s.Count(testContext, Filter{IncludeDeleted: true}); n != 1 {
		t.Fatalf("the refused import left %d predictions, want the 1 that was there", n)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	for name, change := range map[string]struct{ statement, refusal string }{
		"an unknown column":        {`ALTER TABLE predictions ADD COLUMN confidence_band TEXT NOT NULL DEFAULT ''`, "the import knows"},
		"an id off its number":     {`UPDATE predictions SET id = 'prediction_000003' WHERE seq = 4`, "whose id is prediction_000004"},
		"tags that are not a list": {`UPDATE predictions SET tags = '{"a":1}' WHERE seq = 1`, "not a JSON list of strings"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			path := newStandaloneDatabase(t)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(change.statement); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if _, err := ImportStandaloneDatabase(testContext, s.db, path); err == nil || !strings.Contains(err.Error(), change.refusal) {
				t.Fatalf("importing a file with %s = %v, want a refusal saying %q", name, err, change.refusal)
			}
			if n, _ := s.Count(testContext, Filter{IncludeDeleted: true}); n != 0 {
				t.Fatalf("the refused import left %d predictions", n)
			}
		})
	}
}
