package memorystore

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func saveMemory(t *testing.T, s *Store, id, content string) {
	t.Helper()
	stamped := time.Now()
	if err := s.Save(Memory{
		ID:           id,
		Content:      content,
		Importance:   0.5,
		Source:       "test",
		CreatedAt:    stamped,
		LastAccessed: stamped,
	}); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

func searchIDs(t *testing.T, s *Store, query string, limit int) []string {
	t.Helper()
	got, err := s.Search(query, limit)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return idsOf(got)
}

// A query naming nothing in the store must come back empty.
//
// The hashed bag of words could not say this. An unmatched query embedded to an
// all-zero vector, cosine returned 0 against every memory, and the blend made
// every score 0 — so Search sorted the whole store by id and handed back the
// first `limit` of it. A caller asking about something the store has never seen
// got a confident answer built from the alphabetically-first memories.
func TestSearchReturnsNothingForAQueryTheStoreDoesNotContain(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "a", "the scheduler kills a job's process group on timeout")
	saveMemory(t, s, "b", "plex library thumbnails are cached on disk")
	saveMemory(t, s, "c", "noteboard stores todos in sqlite")

	if got := searchIDs(t, s, "helicopter aerodynamics fuselage", 10); len(got) != 0 {
		t.Fatalf("a query matching no memory returned %v, want nothing", got)
	}
}

// A query made entirely of stop words has nothing to search for, and must not
// be answered with the store's first `limit` rows either.
func TestSearchReturnsNothingForAQueryOfOnlyStopWords(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "a", "the scheduler kills a job's process group on timeout")
	saveMemory(t, s, "b", "plex library thumbnails are cached on disk")

	for _, query := range []string{"", "   ", "the and for", "!!! ???"} {
		if got := searchIDs(t, s, query, 10); len(got) != 0 {
			t.Errorf("query %q returned %v, want nothing", query, got)
		}
	}
}

// The query is data, not syntax. FTS5 MATCH is a query language, so an
// unescaped user string can change what is asked — or fail to parse at all,
// which turns an ordinary search into a 500.
func TestSearchTreatsFTSOperatorsInTheQueryAsText(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "wanted", "the scheduler reloads its cron schedule on the next tick")
	saveMemory(t, s, "other", "plex library thumbnails are cached on disk")

	// Each of these is a syntax error, an operator, or an unbalanced quote to
	// FTS5, and ordinary words to a person. Every one of them names `scheduler`,
	// so "found nothing" can only mean the expression was not built from the
	// words — it cannot mean the store had no answer.
	for _, query := range []string{
		`scheduler NEAR(cron)`,
		`scheduler OR`,
		`scheduler "unbalanced`,
		`schedule* AND (cron`,
		`scheduler -cron`,
		`^scheduler`,
	} {
		got, err := s.Search(query, 10)
		if err != nil {
			t.Errorf("query %q returned an error rather than a result: %v", query, err)
			continue
		}
		if len(got) == 0 {
			t.Errorf("query %q found nothing; 'scheduler' is in the store", query)
		}
	}
}

// Short technical names are searchable. tokenize() drops every token of three
// characters or fewer to keep the hash vocabulary small; the query side must
// not, or "ssh", "api" and "db" are unaskable while the index holds them.
func TestSearchFindsShortTechnicalTerms(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "ssh", "the runner needs no inbound ssh because the socket is outbound")
	saveMemory(t, s, "api", "tool-store exposes an api for provisioning mcp servers")
	saveMemory(t, s, "unrelated", "marginalia extracts people and places from a chapter")

	for query, want := range map[string]string{"ssh": "ssh", "api": "api"} {
		got := searchIDs(t, s, query, 10)
		if len(got) == 0 || got[0] != want {
			t.Errorf("query %q returned %v, want %s first", query, got, want)
		}
	}
}

// The porter tokenizer is why "scheduling" reaches "scheduler". Dropping it
// from the index definition costs recall on ordinary English and nothing says so.
func TestSearchStemsTheQueryOntoTheIndex(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "sched", "the scheduler runs jobs")
	saveMemory(t, s, "other", "plex caches thumbnails")

	if got := searchIDs(t, s, "scheduling", 10); len(got) != 1 || got[0] != "sched" {
		t.Fatalf("query \"scheduling\" returned %v, want [sched] — the porter tokenizer is not in effect", got)
	}
}

// Rewriting a memory has to rewrite its index row. Save is an upsert, so the
// same call is both the insert and the update, and an index that only ever
// inserts answers with the text the memory used to hold.
func TestSavingOverAMemoryReindexesIt(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "m", "the scheduler runs jobs on a cron expression")
	if got := searchIDs(t, s, "scheduler", 10); len(got) != 1 {
		t.Fatalf("setup: query \"scheduler\" returned %v, want the one memory", got)
	}

	saveMemory(t, s, "m", "plex library thumbnails are cached on disk")

	if got := searchIDs(t, s, "scheduler", 10); len(got) != 0 {
		t.Errorf("the old text is still indexed: query \"scheduler\" returned %v", got)
	}
	if got := searchIDs(t, s, "thumbnails", 10); len(got) != 1 || got[0] != "m" {
		t.Errorf("the new text is not indexed: query \"thumbnails\" returned %v, want [m]", got)
	}
}

// A memory whose importance was zeroed by Forget must leave the results, and
// the index is not where that is decided — the join against `memories` is.
func TestSearchDropsForgottenMemories(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "keep", "the scheduler runs jobs on a cron expression")
	saveMemory(t, s, "drop", "the scheduler also reloads on a patch")

	if got := searchIDs(t, s, "scheduler", 10); len(got) != 2 {
		t.Fatalf("setup: got %v, want both memories", got)
	}
	if err := s.Forget("drop"); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, "scheduler", 10); len(got) != 1 || got[0] != "keep" {
		t.Fatalf("after Forget: got %v, want [keep]", got)
	}
}

// Opening a store written before the index existed has to index what is already
// there. Without the backfill, every memory in every store on this box becomes
// unsearchable the moment the binary is replaced — silently, because an empty
// index is a valid index and answers every query with nothing.
func TestOpeningAStoreIndexesMemoriesTheIndexIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	saveMemory(t, s, "a", "the scheduler runs jobs on a cron expression")
	saveMemory(t, s, "b", "plex library thumbnails are cached on disk")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Empty the index behind the store's back, which is the state a database
	// written by a binary without it is already in.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM ` + ftsTableName); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := raw.QueryRow(`SELECT count(*) FROM ` + ftsTableName).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("setup: index still holds %d rows", remaining)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if got := searchIDs(t, reopened, "scheduler", 10); len(got) != 1 || got[0] != "a" {
		t.Errorf("query \"scheduler\" after reopen returned %v, want [a]", got)
	}
	if got := searchIDs(t, reopened, "thumbnails", 10); len(got) != 1 || got[0] != "b" {
		t.Errorf("query \"thumbnails\" after reopen returned %v, want [b]", got)
	}
}

// The backfill must not index a memory twice, or its terms are counted twice
// and every reopen inflates the same memory's ranking a little further.
func TestReopeningAStoreDoesNotIndexTheSameMemoryTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	saveMemory(t, s, "a", "the scheduler runs jobs on a cron expression")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		again, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := again.Close(); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	var rows int
	if err := raw.QueryRow(`SELECT count(*) FROM `+ftsTableName+` WHERE memory_id = ?`, "a").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("memory 'a' has %d index rows after four opens, want 1", rows)
	}
}

// When BM25 separates none of the candidates, importance and recency must still
// decide the order.
//
// bm25() returns 0 for a term that appears in every indexed document — it has
// no information about which one you want. Left alone that makes every score
// 0 × importance × recency = 0, the whole ranking collapses onto the id
// tie-break, and the two signals that DO differ are silently discarded.
func TestImportanceDecidesWhenBM25SeparatesNothing(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Identical text, so every candidate has an identical zero BM25 score. Ids
	// ascend against importance, so an id tie-break and an importance sort give
	// opposite answers and the test can tell them apart.
	stamped := time.Now()
	for i, importance := range []float64{0.1, 0.9, 0.5} {
		if err := s.Save(Memory{
			ID:           string(rune('a' + i)),
			Content:      "identical text in every memory",
			Importance:   importance,
			Source:       "test",
			CreatedAt:    stamped,
			LastAccessed: stamped,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := searchIDs(t, s, "identical text", 3)
	want := []string{"b", "c", "a"} // importance 0.9, 0.5, 0.1
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %q, want %q (full order %v — importance lost its vote)", i, got[i], want[i], got)
		}
	}
}

// The orchestrator filter still applies, and it is applied to the join rather
// than to the index.
func TestSearchFilteredStillScopesByOrchestrator(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	stamped := time.Now()
	for _, m := range []struct{ id, orchestrator string }{
		{"inber-one", "inber"},
		{"inber-two", "inber"},
		{"claw-one", "openclaw"},
	} {
		if err := s.Save(Memory{
			ID:           m.id,
			Content:      "the scheduler runs jobs on a cron expression",
			Importance:   0.5,
			Source:       "test",
			Orchestrator: m.orchestrator,
			CreatedAt:    stamped,
			LastAccessed: stamped,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.SearchFiltered("scheduler", 10, "inber")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want the two inber memories", idsOf(got))
	}
	for _, m := range got {
		if m.Orchestrator != "inber" {
			t.Fatalf("%s belongs to %q, not inber", m.ID, m.Orchestrator)
		}
	}
}

// An expired memory must not come back, and expiry is checked against the join
// rather than the index too.
func TestSearchDropsExpiredMemories(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	stamped := time.Now()
	past := stamped.Add(-time.Hour)
	future := stamped.Add(time.Hour)

	if err := s.Save(Memory{ID: "gone", Content: "the scheduler runs jobs", Importance: 0.5, Source: "test", CreatedAt: stamped, LastAccessed: stamped, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Memory{ID: "live", Content: "the scheduler reloads on patch", Importance: 0.5, Source: "test", CreatedAt: stamped, LastAccessed: stamped, ExpiresAt: &future}); err != nil {
		t.Fatal(err)
	}

	if got := searchIDs(t, s, "scheduler", 10); len(got) != 1 || got[0] != "live" {
		t.Fatalf("got %v, want [live]", got)
	}
}

func TestFTSMatchExpression(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
		ok    bool
	}{
		{"plain words", "scheduler cron", `"scheduler" OR "cron"`, true},
		{"lowercased", "Scheduler CRON", `"scheduler" OR "cron"`, true},
		{"stop words dropped", "the scheduler and the cron", `"scheduler" OR "cron"`, true},
		{"short terms kept", "ssh api db", `"ssh" OR "api" OR "db"`, true},
		{"punctuation splits", "auth-store:8303/resolve", `"auth" OR "store" OR "8303" OR "resolve"`, true},
		{"operators are text", "scheduler NEAR(cron)", `"scheduler" OR "near" OR "cron"`, true},
		{"empty", "", "", false},
		{"only punctuation", "!!! ???", "", false},
		{"only stop words", "the and for", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ftsMatchExpression(c.query)
			if ok != c.ok {
				t.Fatalf("ftsMatchExpression(%q) ok = %v, want %v", c.query, ok, c.ok)
			}
			if got != c.want {
				t.Fatalf("ftsMatchExpression(%q) = %q, want %q", c.query, got, c.want)
			}
		})
	}
}

// queryPlan returns SQLite's plan for a statement as one string.
func queryPlan(t *testing.T, s *Store, statement string) string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN " + statement)
	if err != nil {
		t.Fatalf("explain %q: %v", statement, err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, " | ")
}

// The index writer's DELETE has to be keyed on the rowid, and this pins that
// structurally rather than by timing.
//
// An FTS5 virtual table cannot carry a secondary index, so a WHERE on any
// ordinary column of it — including the UNINDEXED memory_id it stores — is a
// full scan of the whole index. Keyed that way, re-saving one memory in a store
// of n costs a scan of n: the first version of this index was written that way
// and TestTagsSurviveACandidateSetLargerThanSQLitesParameterLimit, which seeds
// 32,767 memories, went from 271 seconds to a ten-minute timeout under -race.
//
// The assertion is that the two plans DIFFER, not that either equals a literal.
// SQLite's plan text for a virtual table is an implementation detail that has
// changed between releases; whether a constraint was used at all has not.
func TestTheIndexWriterDeletesByRowidAndNotByAScan(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	saveMemory(t, s, "a", "the scheduler runs jobs")

	// The statement the index writer actually runs, not a copy of it.
	byRowid := queryPlan(t, s, strings.ReplaceAll(ftsDeleteStatement, "?", "1"))
	byMemoryID := queryPlan(t, s, "DELETE FROM "+ftsTableName+" WHERE memory_id = 'a'")
	unconstrained := queryPlan(t, s, "DELETE FROM "+ftsTableName)

	if byRowid == byMemoryID {
		t.Errorf("the index writer's DELETE (%q) plans the same as a scan by memory_id (%q) — it is not keyed on the rowid", ftsDeleteStatement, byRowid)
	}
	// The other half: memory_id really is unindexed, so a WHERE on it is the
	// same plan as no WHERE at all. Without this, the test above would pass on
	// any two plans that happened to differ for an unrelated reason.
	if byMemoryID != unconstrained {
		t.Errorf("a WHERE on the UNINDEXED memory_id (%q) plans differently from no WHERE at all (%q); this test's premise no longer holds", byMemoryID, unconstrained)
	}
}

// The rowid mapping is the one thing between a hit and the memory it describes,
// and a VACUUM of `memories` can renumber it. Search must refuse rather than
// answer with the wrong memory.
func TestSearchRefusesWhenTheIndexIsKeyedToTheWrongRow(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saveMemory(t, s, "a", "the scheduler runs jobs on a cron expression")
	saveMemory(t, s, "b", "plex library thumbnails are cached on disk")

	// Rewrite the memory_id stored beside one index row, which is what a
	// renumbering leaves behind: the index row now describes a different memory
	// from the one its rowid joins to.
	if _, err := s.db.Exec(`UPDATE `+ftsTableName+` SET memory_id = ? WHERE memory_id = ?`, "b", "a"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Search("scheduler", 10); err == nil {
		t.Fatal("search answered from an index keyed to the wrong row instead of refusing")
	} else if !strings.Contains(err.Error(), "keyed to the wrong row") {
		t.Fatalf("search failed for the wrong reason: %v", err)
	}
}
