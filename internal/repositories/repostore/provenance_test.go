package repostore

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
)

func mustSetProvenance(t *testing.T, s *Store, id int64, p Provenance) *Repo {
	t.Helper()
	got, err := s.SetProvenance(id, p)
	if err != nil {
		t.Fatalf("set provenance %q on %d: %v", string(p), id, err)
	}
	return got
}

func TestANewRepoStartsUnstatedRatherThanOurs(t *testing.T) {
	s := openTest(t)
	r := &Repo{Name: "dash", Path: "/x/dash"}
	mustUpsertRepo(t, s, r)

	if r.Provenance != ProvenanceUnstated {
		t.Fatalf("insert returned provenance %q, want unstated: a store that guesses puts the guess under every count taken over it", string(r.Provenance))
	}
	got, err := s.GetRepoByName("dash")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != ProvenanceUnstated {
		t.Fatalf("stored provenance %q, want unstated", string(got.Provenance))
	}
}

func TestSetProvenanceStoresTheValueItWasGiven(t *testing.T) {
	s := openTest(t)
	r := &Repo{Name: "claude-squad", Path: "/x/claude-squad"}
	mustUpsertRepo(t, s, r)

	back := mustSetProvenance(t, s, r.ID, ProvenanceVendored)
	if back.Provenance != ProvenanceVendored {
		t.Fatalf("SetProvenance returned %q, want %q", string(back.Provenance), string(ProvenanceVendored))
	}
	reread, err := s.GetRepo(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.Provenance != ProvenanceVendored {
		t.Fatalf("re-read provenance %q, want %q", string(reread.Provenance), string(ProvenanceVendored))
	}
}

// A tag refresh is the routine write on this store — the drift report tells a
// reader to re-POST a repo to re-detect its tags. If that path wrote
// provenance, every refresh would blank a field the request never mentioned.
func TestATagRefreshDoesNotBlankProvenance(t *testing.T) {
	s := openTest(t)
	r := &Repo{Name: "happy", Path: "/x/happy", Tags: []string{"node"}}
	mustUpsertRepo(t, s, r)
	mustSetProvenance(t, s, r.ID, ProvenanceVendored)

	// Exactly what a re-detect does: a struct built from the request, with no
	// provenance on it, upserted by name.
	refresh := &Repo{Name: "happy", Path: "/x/happy", Tags: []string{"node", "typescript"}}
	inserted, err := s.UpsertRepo(refresh)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("refresh inserted a second row instead of updating")
	}
	if refresh.Provenance != ProvenanceVendored {
		t.Fatalf("upsert returned provenance %q, want the stored %q: the caller must not read back the empty value it sent", string(refresh.Provenance), string(ProvenanceVendored))
	}
	stored, err := s.GetRepoByName("happy")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Provenance != ProvenanceVendored {
		t.Fatalf("provenance after refresh is %q, want %q", string(stored.Provenance), string(ProvenanceVendored))
	}
	if !hasTag(stored.EffectiveTags, "typescript") {
		t.Fatalf("refresh did not update tags: %v", stored.EffectiveTags)
	}
}

func TestAnUnknownProvenanceIsRefusedRatherThanStored(t *testing.T) {
	s := openTest(t)
	r := &Repo{Name: "a"}
	mustUpsertRepo(t, s, r)
	mustSetProvenance(t, s, r.ID, ProvenanceOurs)

	if _, err := s.SetProvenance(r.ID, Provenance("third-party")); err == nil {
		t.Fatal("stored an unknown provenance; a count cannot say what population it counted from a value the reader does not know")
	}
	stored, err := s.GetRepo(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Provenance != ProvenanceOurs {
		t.Fatalf("a refused write changed the stored value to %q", string(stored.Provenance))
	}
}

func TestSetProvenanceOnAMissingRepoIsNotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.SetProvenance(4242, ProvenanceOurs); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListFilterSeparatesTheThreeClasses(t *testing.T) {
	s := openTest(t)
	ours := &Repo{Name: "dash"}
	vendored := &Repo{Name: "claude-squad"}
	unstated := &Repo{Name: "job-store"}
	mustUpsertRepo(t, s, ours)
	mustUpsertRepo(t, s, vendored)
	mustUpsertRepo(t, s, unstated)
	mustSetProvenance(t, s, ours.ID, ProvenanceOurs)
	mustSetProvenance(t, s, vendored.ID, ProvenanceVendored)

	cases := []struct {
		filter *Provenance
		want   []string
	}{
		{ptr(ProvenanceOurs), []string{"dash"}},
		{ptr(ProvenanceVendored), []string{"claude-squad"}},
		{ptr(ProvenanceUnstated), []string{"job-store"}},
		{nil, []string{"claude-squad", "dash", "job-store"}},
	}
	for _, c := range cases {
		got, err := s.ListRepos(ListFilter{Provenance: c.filter})
		if err != nil {
			t.Fatal(err)
		}
		names := repoNames(got)
		if len(names) != len(c.want) {
			t.Fatalf("filter %v: got %v, want %v", c.filter, names, c.want)
		}
		for i := range names {
			if names[i] != c.want[i] {
				t.Fatalf("filter %v: got %v, want %v", c.filter, names, c.want)
			}
		}
	}
}

// A nil filter and a filter for the unstated class are different questions.
// They collapse into one the moment the field is a Provenance instead of a
// *Provenance, and the collapse is silent: "everything" starts answering
// "the unclassified ones".
func TestFilteringOffIsNotTheSameQuestionAsFilteringForUnstated(t *testing.T) {
	s := openTest(t)
	a := &Repo{Name: "a"}
	b := &Repo{Name: "b"}
	mustUpsertRepo(t, s, a)
	mustUpsertRepo(t, s, b)
	mustSetProvenance(t, s, a.ID, ProvenanceOurs)

	all, err := s.ListRepos(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	only, err := s.ListRepos(ListFilter{Provenance: ptr(ProvenanceUnstated)})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("no filter returned %v, want both repos", repoNames(all))
	}
	if len(only) != 1 || only[0].Name != "b" {
		t.Fatalf("unstated filter returned %v, want [b]", repoNames(only))
	}
}

// schema.sql is CREATE TABLE IF NOT EXISTS, which does nothing to a table that
// already exists. Without the ALTER, a column added to the schema reaches a
// fresh database and never reaches the live one — where every existing row is.
func TestOpeningADatabaseFromTheOlderSchemaAddsTheColumnAndKeepsItsRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "repo-store.db")

	old, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
		CREATE TABLE repos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			path TEXT NOT NULL DEFAULT '',
			git_remote TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '',
			tag_overrides TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO repos (name, path, tags, created_at, updated_at)
		VALUES ('dash', '/x/dash', '["go"]', 1, 1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open a database written by the older schema: %v", err)
	}
	defer s.Close()

	got, err := s.GetRepoByName("dash")
	if err != nil {
		t.Fatalf("the pre-existing row did not survive the migration: %v", err)
	}
	if got.Provenance != ProvenanceUnstated {
		t.Fatalf("migrated row has provenance %q, want unstated", string(got.Provenance))
	}
	if !hasTag(got.EffectiveTags, "go") {
		t.Fatalf("migrated row lost its tags: %v", got.EffectiveTags)
	}
	if _, err := s.SetProvenance(got.ID, ProvenanceOurs); err != nil {
		t.Fatalf("the added column is not writable: %v", err)
	}
}

func TestKnownProvenancesIsTheSetTheCheckUses(t *testing.T) {
	for _, p := range KnownProvenances() {
		if !p.IsKnown() {
			t.Fatalf("KnownProvenances lists %q but IsKnown refuses it", string(p))
		}
	}
	if Provenance("ours ").IsKnown() {
		t.Fatal("a value that only looks like one is accepted")
	}
}

func ptr[T any](v T) *T { return &v }

// --- HTTP surface ---

func testServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	s := openTest(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func do(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out.Bytes()
}

func TestPutProvenanceSetsItAndTheFilterFindsIt(t *testing.T) {
	srv, s := testServer(t)
	r := &Repo{Name: "claude-squad"}
	mustUpsertRepo(t, s, r)

	code, body := do(t, "PUT", srv.URL+"/repos/"+itoa(r.ID)+"/provenance", `{"provenance":"vendored"}`)
	if code != 200 {
		t.Fatalf("PUT provenance: %d %s", code, body)
	}
	var got Repo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Provenance != ProvenanceVendored {
		t.Fatalf("response provenance %q, want vendored", string(got.Provenance))
	}

	code, body = do(t, "GET", srv.URL+"/repos?provenance=vendored", "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var listed []Repo
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "claude-squad" {
		t.Fatalf("vendored filter returned %s", body)
	}
}

func TestQueryingTheUnstatedClassNeedsItsOwnWord(t *testing.T) {
	srv, s := testServer(t)
	stated := &Repo{Name: "dash"}
	blank := &Repo{Name: "job-store"}
	mustUpsertRepo(t, s, stated)
	mustUpsertRepo(t, s, blank)
	mustSetProvenance(t, s, stated.ID, ProvenanceOurs)

	code, body := do(t, "GET", srv.URL+"/repos?provenance=unstated", "")
	if code != 200 {
		t.Fatalf("unstated filter: %d %s", code, body)
	}
	var listed []Repo
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "job-store" {
		t.Fatalf("unstated filter returned %s", body)
	}

	// An empty parameter is not the unstated class — it cannot be told from the
	// parameter being absent, and absent means the filter is off.
	code, body = do(t, "GET", srv.URL+"/repos?provenance=", "")
	if code != 400 {
		t.Fatalf("empty provenance parameter: want 400, got %d %s", code, body)
	}
}

func TestAnUnknownProvenanceIsA400AtBothEnds(t *testing.T) {
	srv, s := testServer(t)
	r := &Repo{Name: "a"}
	mustUpsertRepo(t, s, r)

	code, body := do(t, "PUT", srv.URL+"/repos/"+itoa(r.ID)+"/provenance", `{"provenance":"third-party"}`)
	if code != 400 {
		t.Fatalf("PUT unknown provenance: want 400, got %d %s", code, body)
	}
	code, body = do(t, "GET", srv.URL+"/repos?provenance=third-party", "")
	if code != 400 {
		t.Fatalf("GET unknown provenance: want 400, got %d %s", code, body)
	}
	code, body = do(t, "PUT", srv.URL+"/repos/"+itoa(r.ID)+"/provenance", `{}`)
	if code != 400 {
		t.Fatalf("PUT with no provenance: want 400, got %d %s", code, body)
	}
}

// The upsert path does not write the column. Accepting a provenance there and
// ignoring it would answer 200 with the stored value, which reads exactly like
// the request having applied.
func TestPostingAProvenanceIsRefusedRatherThanIgnored(t *testing.T) {
	srv, s := testServer(t)
	r := &Repo{Name: "claude-squad"}
	mustUpsertRepo(t, s, r)
	mustSetProvenance(t, s, r.ID, ProvenanceVendored)

	code, body := do(t, "POST", srv.URL+"/repos", `{"name":"claude-squad","provenance":"ours"}`)
	if code != 400 {
		t.Fatalf("POST carrying a provenance: want 400, got %d %s", code, body)
	}
	if !bytes.Contains(body, []byte("PUT /repos/{id}/provenance")) {
		t.Fatalf("the refusal does not name the write path: %s", body)
	}
	stored, err := s.GetRepo(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Provenance != ProvenanceVendored {
		t.Fatalf("the refused POST changed provenance to %q", string(stored.Provenance))
	}

	// The same POST without the field is the ordinary refresh and must work.
	code, body = do(t, "POST", srv.URL+"/repos", `{"name":"claude-squad"}`)
	if code != 200 {
		t.Fatalf("ordinary refresh: %d %s", code, body)
	}
	var got Repo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Provenance != ProvenanceVendored {
		t.Fatalf("refresh response reports provenance %q, want the stored vendored", string(got.Provenance))
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
