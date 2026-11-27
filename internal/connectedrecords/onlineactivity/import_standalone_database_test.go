package onlineactivity

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func hashOf(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// standaloneFixture is a standalone data directory: the SQLite file and its
// attachments folder.
type standaloneFixture struct {
	databasePath, attachmentsDirectory string
}

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows, a purged activity, and gaps in every
// numbering, and its attachment files under attachments/<xx>/<sha256><ext>.
func newStandaloneDatabase(t *testing.T) standaloneFixture {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	fixture := standaloneFixture{databasePath: filepath.Join(directory, "activity-store.db"), attachmentsDirectory: filepath.Join(directory, "attachments")}
	db, err := sql.Open("sqlite", fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	audio := "a recorded question"
	audioHash := hashOf(audio)
	storedPath := "attachments/" + audioHash[:2] + "/" + audioHash + ".mp3"
	if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(storedPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, storedPath), []byte(audio), 0o600); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, imported_at, finished_at, counts)
			VALUES ('import_000002', 2, 'google_my_activity_html', '` + strings.Repeat("a", 64) + `', 'laptop', 'Takeout/Search', 1700000000, 1700000100, '{"created":2,"folded":0,"known":0,"attachments_stored":1}')`,
		`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, imported_at, finished_at, counts)
			VALUES ('import_000005', 5, 'chrome_history_json', '` + strings.Repeat("b", 64) + `', 'laptop', 'Takeout/Chrome', 1700000200, 0, '{}')`,
		`INSERT INTO activities (id, seq, product, action, action_text, title, url, occurred_at, latitude, longitude, details, source, source_ref,
			import_id, dedupe_key, created_at, updated_at, deleted_at, purged_at)
			VALUES ('activity_000003', 3, 'Search', 'searched', 'Searched for', 'sourdough starter', 'https://www.google.com/search?q=sourdough+starter',
			1767664870, 10.0001, 20.0002, '{"locations":[{"name":"Example Town","latitude":10.0001,"longitude":20.0002,"basis":"From your device","url":""}]}',
			'google_my_activity', 'ref-search', 'import_000002', 'k1', 1700000000, 1700000000, 0, 0)`,
		`INSERT INTO activities (id, seq, product, action, action_text, title, url, occurred_at, latitude, longitude, details, source, source_ref,
			import_id, dedupe_key, created_at, updated_at, deleted_at, purged_at)
			VALUES ('activity_000007', 7, 'Chrome', 'visited', '', '', '', 1767664900, NULL, NULL, '{}', 'google_my_activity', 'ref-visit',
			NULL, 'k2', 1700000000, 1700000300, 1700000300, 1700000300)`,
		`INSERT INTO sightings VALUES ('activity_000003', 'google_my_activity', 'ref-search', 'Search', 'searched', 'import_000002', 1767664870, 1700000000)`,
		`INSERT INTO sightings VALUES ('activity_000003', 'chrome_history', 'ref-chrome', 'Chrome', 'visited', 'import_000005', 1767664870, 1700000200)`,
		`INSERT INTO sightings VALUES ('activity_000007', 'google_my_activity', 'ref-visit', 'Chrome', 'visited', 'import_000002', 1767664900, 1700000000)`,
		`INSERT INTO attachments (id, seq, activity_id, file_name, content_type, sha256, size_bytes, stored_path, created_at, purged_at)
			VALUES ('attachment_000004', 4, 'activity_000003', 'question.mp3', 'audio/mpeg', '` + audioHash + `', ` + strconv.Itoa(len(audio)) + `, '` + storedPath + `', 1700000000, 0)`,
		`INSERT INTO attachments (id, seq, activity_id, file_name, content_type, sha256, size_bytes, stored_path, created_at, purged_at)
			VALUES ('attachment_000006', 6, 'activity_000007', '', 'image/png', '` + strings.Repeat("c", 64) + `', 10, '', 1700000000, 1700000300)`,
		`INSERT INTO revisions (id, activity_id, reason, revised_at, snapshot) VALUES (8, 'activity_000007', 'purge', 1700000300, '{"id":"activity_000007","title":""}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return fixture
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	ts := newTestServer(t)
	fixture := newStandaloneDatabase(t)
	counts, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, fixture.databasePath, fixture.attachmentsDirectory, aliceID)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (StandaloneImportCounts{Imports: 2, Activities: 2, Sightings: 3, Attachments: 2, AttachmentFiles: 1, Revisions: 1, PurgedActivities: 1}) {
		t.Errorf("counts = %+v", counts)
	}
	if strings.Contains(counts.Summary(), "sourdough") || !strings.Contains(counts.Summary(), "activities=2") {
		t.Errorf("summary = %q", counts.Summary())
	}

	var search Activity
	ts.do("GET", "/activities/activity_000003", nil, 200, &search)
	if search.OwnerPrincipalID != aliceID || search.Title != "sourdough starter" || search.ImportID != "import_000002" ||
		search.Latitude == nil || *search.Latitude != 10.0001 || len(search.Details.Locations) != 1 || len(search.Sightings) != 2 ||
		search.Sightings[1].Source != "chrome_history" || search.Sightings[1].ImportID != "import_000005" {
		t.Errorf("the search = %+v", search)
	}
	if len(search.Attachments) != 1 || search.Attachments[0].ID != "attachment_000004" || search.Attachments[0].Sha256 != hashOf("a recorded question") {
		t.Fatalf("its attachment = %+v", search.Attachments)
	}
	if response := ts.raw("GET", "/attachments/attachment_000004", nil); response.Code != 200 || response.Body.String() != "a recorded question" {
		t.Errorf("the attachment's bytes = %d %q", response.Code, response.Body.String())
	}
	if told := ts.files.told["file_000001"]; told != (AttachmentFile{ActivityID: "activity_000003", FileName: "question.mp3", ContentType: "audio/mpeg"}) {
		t.Errorf("file-store was told %+v", told)
	}
	// The purged activity stays purged, out of search, its attachment gone.
	var visit Activity
	ts.do("GET", "/activities/activity_000007?include_deleted=true", nil, 200, &visit)
	if visit.PurgedAt == 0 || visit.ImportID != "" || visit.Latitude != nil {
		t.Errorf("the purged visit = %+v", visit)
	}
	ts.do("GET", "/attachments/attachment_000006", nil, http.StatusGone, nil)
	var revisions []Revision
	ts.do("GET", "/activities/activity_000007/revisions", nil, 200, &revisions)
	if len(revisions) != 1 || revisions[0].ID != 8 || revisions[0].Reason != RevisionPurge {
		t.Errorf("revisions = %+v", revisions)
	}
	var page ActivityPage
	ts.do("GET", "/activities?q=sourdough&include_deleted=true", nil, 200, &page)
	if page.Total != 1 {
		t.Errorf("search after the import = %+v", page)
	}
	ts.do("GET", "/activities?q=town", nil, 200, &page)
	if page.Total != 1 {
		t.Errorf("a location name is searched after the import: %+v", page)
	}
	var imports []Import
	ts.do("GET", "/imports", nil, 200, &imports)
	if len(imports) != 2 || imports[0].ID != "import_000005" || imports[1].Counts.Created != 2 {
		t.Errorf("imports = %+v", imports)
	}
	// Bob sees none of it.
	ts.as(bob).do("GET", "/activities/activity_000003", nil, http.StatusNotFound, nil)

	// New records number on after the highest copied, and a known record is
	// still known by its source reference.
	var started ImportStarted
	ts.do("POST", "/imports", ImportStart{Kind: ImportKindMyActivityHTML, Sha256: strings.Repeat("a", 64)}, 200, &started)
	if !started.AlreadyFinished || started.Import.ID != "import_000002" {
		t.Errorf("the imported file again = %+v", started)
	}
	newID, newCounts := ts.importFile(ImportKindMyActivityHTML, sha("d"), searchOf("ref-search"), searchOf("ref-new"))
	if newID != "import_000006" || newCounts.Known != 1 || newCounts.Created != 1 {
		t.Errorf("the next import = %s %+v", newID, newCounts)
	}
	ts.do("GET", "/activities?q=quince", nil, 200, &page)
	if page.Total != 1 || page.Activities[0].ID != "activity_000008" || page.Activities[0].Attachments[0].ID != "attachment_000007" {
		t.Errorf("the next activity = %+v", page)
	}
	ts.do("DELETE", "/activities/activity_000008", nil, http.StatusNoContent, nil)
	ts.do("GET", "/activities/activity_000008/revisions", nil, 200, &revisions)
	if len(revisions) != 1 || revisions[0].ID != 9 {
		t.Errorf("the next revision = %+v", revisions)
	}
}

// searchOf is a made-up search with one attachment, filed under ref.
func searchOf(ref string) ImportActivity {
	record := search()
	record.SourceRef, record.Title, record.URL = ref, "quince jam", "https://www.google.com/search?q=quince+jam"
	return record
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	ts := newTestServer(t)
	fixture := newStandaloneDatabase(t)
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, fixture.databasePath, fixture.attachmentsDirectory, "alice"); err == nil {
		t.Error("an owner that is not a principal id was taken")
	}

	// A file whose bytes do not match its row is refused, and nothing stays.
	tampered := newStandaloneDatabase(t)
	entries, _ := filepath.Glob(filepath.Join(tampered.attachmentsDirectory, "*", "*"))
	if len(entries) != 1 {
		t.Fatalf("attachment files = %v", entries)
	}
	if err := os.WriteFile(entries[0], []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, tampered.databasePath, tampered.attachmentsDirectory, aliceID); err == nil ||
		!strings.Contains(err.Error(), "sha256") {
		t.Errorf("a file whose sha256 does not match: %v", err)
	}
	var health Health
	ts.do("GET", "/health", nil, 200, &health)
	if health.Counts.Activities != 0 || ts.files.held() != 0 {
		t.Errorf("a refused import left rows %+v or %d files", health.Counts, ts.files.held())
	}

	// A file that fails after its attachment bytes went to file-store takes
	// them back out.
	late := newStandaloneDatabase(t)
	db, err := sql.Open("sqlite", late.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO revisions (id, activity_id, reason, revised_at, snapshot) VALUES (9, 'activity_000099', 'delete', 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, late.databasePath, late.attachmentsDirectory, aliceID); err == nil {
		t.Error("a revision of an activity the file does not hold was taken")
	}
	if ts.files.held() != 0 {
		t.Errorf("a failed import left %d files in file-store", ts.files.held())
	}

	// A column the import does not know is data it would drop.
	extra := newStandaloneDatabase(t)
	db, err = sql.Open("sqlite", extra.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE activities ADD COLUMN mood TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, extra.databasePath, extra.attachmentsDirectory, aliceID); err == nil ||
		!strings.Contains(err.Error(), "mood") {
		t.Errorf("an unknown column: %v", err)
	}

	// Into a module that already holds rows, never.
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, fixture.databasePath, fixture.attachmentsDirectory, aliceID); err != nil {
		t.Fatal(err)
	}
	held := ts.files.held()
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, fixture.databasePath, fixture.attachmentsDirectory, bobID); err == nil ||
		!strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import: %v", err)
	}
	if ts.files.held() != held {
		t.Errorf("a refused second import changed file-store from %d to %d files", held, ts.files.held())
	}
	if _, err := ImportStandaloneDatabase(testContext, ts.store.db, ts.files, filepath.Join(t.TempDir(), "missing.db"), fixture.attachmentsDirectory, aliceID); err == nil {
		t.Error("a missing file imported")
	}
}
