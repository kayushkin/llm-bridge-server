package skillstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattn/go-sqlite3"
)

// openTestStore opens a store on a fresh directory and proves foreign-key
// enforcement is actually on before any test relies on it. Without the DSN
// parameter SQLite defaults foreign keys OFF, and every test below would then
// pass by never producing the error it is written to classify.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	var enforced int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if enforced != 1 {
		t.Fatalf("foreign_keys is %d, want 1: this suite cannot produce the violation it classifies", enforced)
	}
	return s
}

// insertTestSource creates a parent source through the store's own API, so no
// test carries a hand-written copy of the sources schema that could drift away
// from it.
func insertTestSource(t *testing.T, s *Store) int64 {
	t.Helper()
	src := &Source{Name: "test-source", RepoURL: "https://example.invalid/repo.git"}
	if err := s.UpsertSource(src); err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if src.ID == 0 {
		t.Fatalf("UpsertSource left ID zero; the tests below need a real parent id")
	}
	return src.ID
}

// theMissingParentID is an id no test ever inserts, so a write naming it can
// only fail for want of the parent row.
const theMissingParentID = 424242

func TestUpsertSkillWithAnAbsentSourceReportsTheSentinel(t *testing.T) {
	s := openTestStore(t)
	_, err := s.UpsertSkill(&Skill{SourceID: theMissingParentID, Name: "probe", SkillMD: "x"})
	if !errors.Is(err, ErrReferencedRowNotFound) {
		t.Fatalf("UpsertSkill error = %v, want it to wrap ErrReferencedRowNotFound", err)
	}
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("424242")) {
		t.Errorf("error %q does not name the id that was wrong", got)
	}
}

func TestAppendSeedVersionWithAnAbsentSkillReportsTheSentinel(t *testing.T) {
	s := openTestStore(t)
	_, err := s.AppendSeedVersion(theMissingParentID, "SKILL.md", []byte("x"), SeedSourceIngest, "", "")
	if !errors.Is(err, ErrReferencedRowNotFound) {
		t.Fatalf("AppendSeedVersion error = %v, want it to wrap ErrReferencedRowNotFound", err)
	}
}

func TestRecordObservedHashWithAnAbsentSkillReportsTheSentinel(t *testing.T) {
	s := openTestStore(t)
	err := s.RecordObservedHash("m1", theMissingParentID, "SKILL.md", "abc")
	if !errors.Is(err, ErrReferencedRowNotFound) {
		t.Fatalf("RecordObservedHash error = %v, want it to wrap ErrReferencedRowNotFound", err)
	}
}

// A constraint that is NOT a foreign key must pass through untouched, or the
// classifier would relabel every refused write as a missing parent row. The
// breach here is NOT NULL on skills.skill_md, produced against the real schema
// rather than a hand-written mirror of it.
func TestAnUnrelatedConstraintIsNotClassifiedAsAMissingParent(t *testing.T) {
	s := openTestStore(t)
	sourceID := insertTestSource(t, s)

	_, err := s.db.Exec(`INSERT INTO skills (source_id, name, source_path, content_hash, created_at, updated_at)
		VALUES (?, 'no-body', 'p', 'h', 0, 0)`, sourceID)
	if err == nil {
		t.Fatal("a row missing a NOT NULL column was accepted; this control cannot run")
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("control produced %v, want a driver constraint error", err)
	}
	if sqliteErr.ExtendedCode == sqlite3.ErrConstraintForeignKey {
		t.Fatalf("control produced a foreign-key breach; it must produce a different one to be a control")
	}
	if wrapped := wrapForeignKeyViolation(err, "source", sourceID); errors.Is(wrapped, ErrReferencedRowNotFound) {
		t.Errorf("a NOT NULL breach was classified as a missing parent row: %v", wrapped)
	}
}

// wrapForeignKeyViolation must not invent an error where the write succeeded.
func TestNilStaysNil(t *testing.T) {
	if err := wrapForeignKeyViolation(nil, "skill", 1); err != nil {
		t.Errorf("wrapForeignKeyViolation(nil) = %v, want nil", err)
	}
}

func postJSON(t *testing.T, url string, payload map[string]any) (int, string) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body.String()
}

// The edge is the whole point of the sentinel: a caller naming a skill that
// does not exist has made a correctable mistake, and until this it was told
// the server had broken.
func TestSeedRoutesAnswer404ForASkillThatDoesNotExist(t *testing.T) {
	s := openTestStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, route := range []string{"/seed/observe", "/seed/drift"} {
		status, body := postJSON(t, srv.URL+route, map[string]any{
			"machine_id": "m1",
			"skill_id":   theMissingParentID,
			"relpath":    "SKILL.md",
			"sha256":     "abc",
			"content":    "x",
		})
		if status != http.StatusNotFound {
			t.Errorf("POST %s -> %d %s, want 404", route, status, body)
		}
		if bytes.Contains([]byte(body), []byte("FOREIGN KEY")) {
			t.Errorf("POST %s leaked the driver's constraint text to the caller: %s", route, body)
		}
	}
}

// The same routes must still answer 200 for a skill that does exist, or the
// test above could be satisfied by a handler that 404s unconditionally.
func TestSeedRoutesStillAcceptASkillThatDoesExist(t *testing.T) {
	s := openTestStore(t)
	sk := &Skill{SourceID: insertTestSource(t, s), Name: "real", SkillMD: "# real"}
	if _, err := s.UpsertSkill(sk); err != nil {
		t.Fatalf("insert skill: %v", err)
	}

	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, route := range []string{"/seed/observe", "/seed/drift"} {
		status, body := postJSON(t, srv.URL+route, map[string]any{
			"machine_id": "m1",
			"skill_id":   sk.ID,
			"relpath":    "SKILL.md",
			"sha256":     "abc",
			"content":    "x",
		})
		if status != http.StatusOK {
			t.Errorf("POST %s -> %d %s, want 200", route, status, body)
		}
	}
}

// numberOfConnectionsProbed is how many pool connections the test below holds
// open at once. It only has to exceed one: the defect it exists to catch is a
// pragma that reaches exactly one connection.
const numberOfConnectionsProbed = 8

// Every classification above rests on SQLite actually refusing the write, and
// foreign_keys is a PER-CONNECTION setting while *sql.DB is a pool. Asking one
// connection is not enough to know the pool enforces it — schema.sql also runs
// "PRAGMA foreign_keys = ON" during migration, which reaches whichever single
// connection served the migration and reads back as 1 on that same connection.
// Measured here: with the DSN parameter 8 of 8 connections enforce; with only
// schema.sql's pragma, 1 of 8 does. So a suite that probes one connection
// cannot tell the sound mechanism from the unsound one.
func TestEveryPooledConnectionEnforcesForeignKeys(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	var held []*sql.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()

	notEnforcing := 0
	for i := 0; i < numberOfConnectionsProbed; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("open pool connection %d: %v", i, err)
		}
		held = append(held, c)
		var enforced int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enforced); err != nil {
			t.Fatalf("read foreign_keys on pool connection %d: %v", i, err)
		}
		if enforced != 1 {
			notEnforcing++
		}
	}
	if notEnforcing > 0 {
		t.Errorf("%d of %d pooled connections do not enforce foreign keys: the setting is reaching one connection, not the pool",
			notEnforcing, numberOfConnectionsProbed)
	}
}

// The sentinel must be the only thing that becomes a 404. An edge that answers
// 404 for any store error at all would report a broken database as a caller's
// mistake, and the 404 test above cannot tell the two apart.
func TestASeedRouteErrorThatIsNotTheSentinelIsStillA500(t *testing.T) {
	s := openTestStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Closing the database makes every write fail for a reason that has nothing
	// to do with a missing parent row.
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	for _, route := range []string{"/seed/observe", "/seed/drift"} {
		status, body := postJSON(t, srv.URL+route, map[string]any{
			"machine_id": "m1",
			"skill_id":   theMissingParentID,
			"relpath":    "SKILL.md",
			"sha256":     "abc",
			"content":    "x",
		})
		if status != http.StatusInternalServerError {
			t.Errorf("POST %s with a closed database -> %d %s, want 500", route, status, body)
		}
	}
}
