package memorystore

import (
	"strings"
	"testing"
	"time"

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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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

// The search document is a generated column of the row, so a memory is
// searchable however its row was written, and with no backfill. The SQLite
// store kept a separate FTS5 index that every save had to write beside the row
// and every open had to backfill; a row the index missed answered every query
// with nothing. This writes a row straight into the table, as an import does,
// and searches for it.
func TestAMemoryWrittenStraightIntoTheTableIsSearchable(t *testing.T) {
	s := openTestStore(t)
	stamped := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO memories (id, content, last_accessed, created_at, source, embedding)
		VALUES ($1, $2, $3, $3, 'test', '[]'), ($4, $5, $3, $3, 'test', '[]')`,
		"a", "the scheduler runs jobs on a cron expression", stamped,
		"b", "plex library thumbnails are cached on disk"); err != nil {
		t.Fatal(err)
	}

	if got := searchIDs(t, s, "scheduler", 10); len(got) != 1 || got[0] != "a" {
		t.Errorf("query \"scheduler\" returned %v, want [a]", got)
	}
	if got := searchIDs(t, s, "thumbnails", 10); len(got) != 1 || got[0] != "b" {
		t.Errorf("query \"thumbnails\" returned %v, want [b]", got)
	}
}

// Saving a memory again must not count its words again, or every save
// inflates the same memory's ranking a little further. The SQLite store pinned
// this on its index rows; here it is the row's word count and its relevance.
func TestSavingAMemoryAgainDoesNotCountItsWordsTwice(t *testing.T) {
	s := openTestStore(t)
	saveMemory(t, s, "a", "the scheduler runs jobs on a cron expression")
	saveMemory(t, s, "b", "plex library thumbnails are cached on disk")
	first, err := s.Search("scheduler", 10)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		saveMemory(t, s, "a", "the scheduler runs jobs on a cron expression")
	}

	var words int
	if err := s.db.QueryRow(`SELECT search_word_count FROM memories WHERE id = $1`, "a").Scan(&words); err != nil {
		t.Fatal(err)
	}
	if words != 8 {
		t.Fatalf("memory 'a' counts %d words after four saves, want the 8 it holds", words)
	}
	again, err := s.Search("scheduler", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(again) != 1 || again[0].ID != first[0].ID {
		t.Fatalf("search after re-saving returned %v, before %v", idsOf(again), idsOf(first))
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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

// The query is cut into terms on every rune that is not a letter or a number,
// so no operator, quote or parenthesis reaches the tsquery, and each lexeme is
// then quoted as an operand.
func TestTokenizeForSearch(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"plain words", "scheduler cron", []string{"scheduler", "cron"}},
		{"lowercased", "Scheduler CRON", []string{"scheduler", "cron"}},
		{"stop words dropped", "the scheduler and the cron", []string{"scheduler", "cron"}},
		{"short terms kept", "ssh api db", []string{"ssh", "api", "db"}},
		{"punctuation splits", "auth-store:8303/resolve", []string{"auth", "store", "8303", "resolve"}},
		{"operators are text", "scheduler NEAR(cron)", []string{"scheduler", "near", "cron"}},
		{"empty", "", nil},
		{"only punctuation", "!!! ???", nil},
		{"only stop words", "the and for", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenizeForSearch(c.query)
			if strings.Join(got, ",") != strings.Join(c.want, ",") || len(got) != len(c.want) {
				t.Fatalf("tokenizeForSearch(%q) = %q, want %q", c.query, got, c.want)
			}
		})
	}
}

func TestAnyOfTheLexemesQuotesEachOperand(t *testing.T) {
	got := anyOfTheLexemes([]string{"schedul", "o'brien", `back\slash`, "or"})
	want := `'schedul' | 'o''brien' | 'back\\slash' | 'or'`
	if got != want {
		t.Fatalf("anyOfTheLexemes = %s, want %s", got, want)
	}
}

// A lexeme that looks like tsquery syntax is only ever a word: Postgres reads
// the quoted operands back as the lexemes themselves.
func TestTheTsqueryReadsBackAsTheLexemes(t *testing.T) {
	s := openTestStore(t)
	var read string
	if err := s.db.QueryRow(`SELECT $1::tsquery::text`, anyOfTheLexemes([]string{"o'brien", "!", "&"})).Scan(&read); err != nil {
		t.Fatal(err)
	}
	if want := `'o''brien' | '!' | '&'`; read != want {
		t.Fatalf("the tsquery reads back as %s, want %s", read, want)
	}
}
