package memorystore

import (
	"database/sql"
	"math"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Search computes BM25 in its query with FTS5's constants (search.go,
// bm25K1), so a store moved from SQLite ranks as it did. This holds it to
// that: the ranking corpus goes into a real FTS5 index, as the SQLite store
// built it, and for every ranking query the order FTS5's bm25() gives must be
// the order Search gives. Importance and recency are equal across the corpus,
// so the order is BM25's alone. The relevance numbers are compared too, as a
// ratio of each memory's to the best one, since that ratio is what the blend
// with importance and recency sees.
func TestSearchRanksAsFTS5Bm25Did(t *testing.T) {
	store := storeWithRankingCorpus(t)
	reference, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	reference.SetMaxOpenConns(1)
	if _, err := reference.Exec(`CREATE VIRTUAL TABLE reference USING fts5(memory_id UNINDEXED, content, summary, tokenize='porter unicode61')`); err != nil {
		t.Fatal(err)
	}
	for _, memory := range rankingCorpus() {
		if _, err := reference.Exec(`INSERT INTO reference (memory_id, content, summary) VALUES (?, ?, '')`, memory.ID, memory.Content); err != nil {
			t.Fatal(err)
		}
	}

	queries := []string{}
	for _, c := range rankingCases() {
		queries = append(queries, c.query)
	}
	queries = append(queries, "scheduler scheduler", "store", "the job and the cron")

	for _, query := range queries {
		tokens := tokenizeForSearch(query)
		quoted := make([]string, len(tokens))
		for index, token := range tokens {
			quoted[index] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
		}
		rows, err := reference.Query(`SELECT memory_id, -bm25(reference) FROM reference WHERE reference MATCH ? ORDER BY bm25(reference), memory_id`, strings.Join(quoted, " OR "))
		if err != nil {
			t.Fatalf("%q: fts5: %v", query, err)
		}
		var wantIDs []string
		var wantRelevance []float64
		for rows.Next() {
			var id string
			var relevance float64
			if err := rows.Scan(&id, &relevance); err != nil {
				t.Fatal(err)
			}
			wantIDs = append(wantIDs, id)
			wantRelevance = append(wantRelevance, relevance)
		}
		rows.Close()

		got, err := store.Search(query, 100)
		if err != nil {
			t.Fatalf("%q: search: %v", query, err)
		}
		gotIDs := idsOf(got)
		if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
			t.Errorf("%q: Search ranks %v, FTS5 ranked %v", query, gotIDs, wantIDs)
			continue
		}

		lexemes, err := store.lexemesOf(tokens)
		if err != nil {
			t.Fatal(err)
		}
		gotRelevance := relevanceOf(t, store, lexemes, gotIDs)
		for index := range gotIDs {
			wantRatio := wantRelevance[index] / wantRelevance[0]
			gotRatio := gotRelevance[index] / gotRelevance[0]
			if math.Abs(wantRatio-gotRatio) > 1e-9 {
				t.Errorf("%q: %s has relevance %.6f of the best, FTS5 gave %.6f", query, gotIDs[index], gotRatio, wantRatio)
			}
		}
	}
}

// relevanceOf asks the module for the BM25 relevance Search ranks each of ids
// by, with the statement Search runs, in the order of ids.
func relevanceOf(t *testing.T, store *Store, lexemes, ids []string) []float64 {
	t.Helper()
	byID := map[string]float64{}
	operands := make([]string, len(lexemes))
	for index, lexeme := range lexemes {
		operands[index] = tsqueryLiteral(lexeme)
	}
	rows, err := store.db.Query(bm25RankingStatement+`WHERE m.id = ANY($3::text[])`, lexemes, operands, ids)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		byID[values[0].(string)] = values[len(values)-1].(float64)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	relevance := make([]float64, len(ids))
	for index, id := range ids {
		relevance[index] = byID[id]
	}
	return relevance
}
