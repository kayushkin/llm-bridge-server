package memories

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// recencyDecayPerDay is the per-day multiplier applied to a memory's score for
// each day since it was last read.
//
// Decaying the retrieval score by a fixed factor, rather than asking the model
// to judge freshness, is the approach the retrieval literature validates; see
// noteboard note 72503642-f787-4f97-ab4b-576037346bb2. It is unchanged from
// when relevance came from cosine similarity — the blend was never the defect.
const recencyDecayPerDay = 0.99

// zeroInformationRelevance is the relevance every candidate is given when BM25
// separates none of them.
//
// BM25 gives next to nothing (an IDF of 1e-6, as FTS5's bm25() did) to a term
// that appears in at least half the memories, because such a term tells you
// nothing about which document you want. If the query
// consists only of such terms, every candidate scores 0, the product with
// importance and recency is 0 for all of them, and the ranking collapses onto
// the id tie-break — importance and recency lose their vote for no reason.
//
// This is applied only when the WHOLE candidate set is tied at zero, so it can
// never reorder a set that BM25 did separate.
const zeroInformationRelevance = 1.0

// bm25K1 and bm25B are the BM25 constants SQLite's FTS5 bm25() uses, written
// into the ranking query. The query computes, for each memory, the sum over
// the query's terms of
//
//	idf × (f × (k1 + 1)) / (f + k1 × (1 − b + b × |D| / avgdl))
//
// where f is how often the term appears in the memory, |D| how many words the
// memory holds, avgdl the mean over every memory, and idf
// ln((N − n + 0.5) / (n + 0.5)) over the N memories of which n hold the term,
// taken as 1e-6 when it is not positive. These are FTS5's numbers, so a
// ranking moved from SQLite keeps its order; the words themselves come from
// Postgres's Snowball stemmer rather than SQLite's Porter one, which differ on
// a few endings.
const (
	bm25K1 = "1.2"
	bm25B  = "0.75"
)

// bm25RankingStatement selects every column of a memory and, last, its BM25
// relevance to the query: $1 holds the query's lexemes and $2 each one written
// as a tsquery operand. The caller adds the WHERE that picks the candidates.
//
// Both CTEs are MATERIALIZED: term is read inside a subquery run once per
// candidate, and Postgres inlines a CTE read once, which would count each
// term's hits over the whole table again for every candidate.
const bm25RankingStatement = `
	WITH corpus AS MATERIALIZED (
		SELECT count(*)::float8 AS row_count, coalesce(avg(search_word_count), 0)::float8 AS average_words
		FROM memories
	), term AS MATERIALIZED (
		SELECT query_term.lexeme, query_term.operand,
		       (SELECT count(*)::float8 FROM memories hit WHERE hit.search_document @@ query_term.operand::tsquery) AS hits
		FROM unnest($1::text[], $2::text[]) AS query_term(lexeme, operand)
	)
	SELECT m.id, m.content, m.summary, m.original_id, m.importance, m.access_count,
	       m.last_accessed, m.created_at, m.source, m.embedding, m.always_load,
	       m.expires_at, m.tokens, m.ref_type, m.ref_target, m.is_lazy, m.orchestrator,
	       (SELECT coalesce(sum(
	                CASE WHEN ln((corpus.row_count - term.hits + 0.5) / (term.hits + 0.5)) <= 0 THEN 1e-6
	                     ELSE ln((corpus.row_count - term.hits + 0.5) / (term.hits + 0.5)) END
	                * (frequency.count * (` + bm25K1 + ` + 1))
	                / (frequency.count + ` + bm25K1 + ` * (1 - ` + bm25B + ` + ` + bm25B + ` * m.search_word_count / corpus.average_words))
	        ), 0)
	        FROM term
	        CROSS JOIN LATERAL (
	            SELECT coalesce((SELECT cardinality(entry.positions) FROM unnest(m.search_document) AS entry
	                             WHERE entry.lexeme = term.lexeme), 0)::float8 AS count
	        ) AS frequency)
	FROM memories m CROSS JOIN corpus
`

// lexemesOf turns each search token into the lexemes Postgres keeps for it,
// in the query's order and keeping repeats, as FTS5 scored each quoted word of
// a MATCH on its own. A token is letters and numbers only, so it is almost
// always one lexeme.
func (s *Store) lexemesOf(tokens []string) ([]string, error) {
	rows, err := s.db.Query(`
	SELECT entry.lexeme
	FROM unnest($1::text[]) WITH ORDINALITY AS token(word, place)
	CROSS JOIN LATERAL unnest(to_tsvector('memory_text'::regconfig, token.word)) AS entry
	ORDER BY token.place, entry.lexeme`, tokens)
	if err != nil {
		return nil, fmt.Errorf("stem the query: %w", err)
	}
	defer rows.Close()
	var lexemes []string
	for rows.Next() {
		var lexeme string
		if err := rows.Scan(&lexeme); err != nil {
			return nil, fmt.Errorf("stem the query: %w", err)
		}
		lexemes = append(lexemes, lexeme)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stem the query: %w", err)
	}
	return lexemes, nil
}

// SearchFiltered finds memories matching the query, optionally filtered by orchestrator.
// If orchestrator is empty, all memories are searched.
func (s *Store) SearchFiltered(query string, limit int, orchestrator string) ([]Memory, error) {
	return s.searchInternal(query, limit, orchestrator)
}

// Search finds memories matching the query, ranked by relevance, importance and
// recency.
//
// Relevance is BM25 over the words of the content and summary, computed in the
// query the way SQLite's FTS5 bm25() computed it (see bm25K1). It used to be
// the cosine similarity of a 256-bucket hashed bag of words, which had no term
// weighting of any kind: every word counted the same whether it appeared in one
// memory or all of them, and unrelated words sharing a bucket scored as a match.
// See noteboard todo 0c31b497-0418-4577-b156-6699918ef5a8.
func (s *Store) Search(query string, limit int) ([]Memory, error) {
	return s.searchInternal(query, limit, "")
}

func (s *Store) searchInternal(query string, limit int, orchestrator string) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}

	// A query with no searchable terms matches nothing, and says so. The cosine
	// path could not: an all-zero query vector scored 0 against every memory,
	// which sorted the whole store by id and returned the first `limit` of it as
	// though they were results.
	tokens := tokenizeForSearch(query)
	if len(tokens) == 0 {
		return []Memory{}, nil
	}
	lexemes, err := s.lexemesOf(tokens)
	if err != nil {
		return nil, err
	}
	if len(lexemes) == 0 {
		return []Memory{}, nil
	}
	operands := make([]string, len(lexemes))
	for index, lexeme := range lexemes {
		operands[index] = tsqueryLiteral(lexeme)
	}

	now := time.Now()
	sqlQuery := bm25RankingStatement + `
	WHERE m.search_document @@ $3::tsquery
	  AND m.importance > 0
	  AND (m.expires_at IS NULL OR m.expires_at > $4)
	`
	args := []interface{}{lexemes, operands, anyOfTheLexemes(lexemes), now.Unix()}
	if orchestrator != "" {
		sqlQuery += " AND m.orchestrator = $5"
		args = append(args, orchestrator)
	}
	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer rows.Close()

	type scored struct {
		memory    Memory
		relevance float64
		score     float64
	}
	var candidates []scored
	memoryIDs := []string{}
	var maxRelevance float64

	for rows.Next() {
		var m Memory
		var summary, originalID, refTarget sql.NullString
		var embJSON []byte
		var lastAccessed, createdAt int64
		var expiresAt sql.NullInt64
		var relevance float64

		err := rows.Scan(
			&m.ID, &m.Content, &summary, &originalID,
			&m.Importance, &m.AccessCount, &lastAccessed, &createdAt, &m.Source, &embJSON,
			&m.AlwaysLoad, &expiresAt, &m.Tokens,
			&m.RefType, &refTarget, &m.IsLazy, &m.Orchestrator,
			&relevance,
		)
		if err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}

		m.Summary = summary.String
		m.OriginalID = originalID.String
		m.RefTarget = refTarget.String
		m.LastAccessed = time.Unix(lastAccessed, 0)
		m.CreatedAt = time.Unix(createdAt, 0)
		if expiresAt.Valid {
			exp := time.Unix(expiresAt.Int64, 0)
			m.ExpiresAt = &exp
		}

		// The embedding column is still written and still returned, so a caller
		// reading it sees no change. Nothing ranks on it any more.
		if err := json.Unmarshal(embJSON, &m.Embedding); err != nil {
			return nil, fmt.Errorf("unmarshal embedding: %w", err)
		}

		if relevance < 0 {
			relevance = 0
		}
		if relevance > maxRelevance {
			maxRelevance = relevance
		}

		candidates = append(candidates, scored{memory: m, relevance: relevance})
		memoryIDs = append(memoryIDs, m.ID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	for i := range candidates {
		relevance := candidates[i].relevance
		if maxRelevance == 0 {
			relevance = zeroInformationRelevance
		}
		daysSinceAccess := now.Sub(candidates[i].memory.LastAccessed).Hours() / 24
		recencyBoost := math.Pow(recencyDecayPerDay, daysSinceAccess)
		candidates[i].score = relevance * candidates[i].memory.Importance * recencyBoost
	}

	// Fetch tags for all candidates.
	//
	// This used to bind one parameter per candidate in a single IN list and then
	// drop the resulting error, so a store with more live memories than SQLite
	// accepts parameters answered 200 with every result's tag list empty. A
	// failure is now returned.
	if len(memoryIDs) > 0 {
		tagsByMemory, err := s.loadTagsForMemories(memoryIDs)
		if err != nil {
			return nil, err
		}
		for i := range candidates {
			candidates[i].memory.Tags = tagsByMemory[candidates[i].memory.ID]
			if candidates[i].memory.Tags == nil {
				candidates[i].memory.Tags = []string{}
			}
		}
	}

	// Sort by score (descending), breaking ties on id.
	//
	// This used to be a stable sort whose comment said ties "keep the order the
	// rows arrived in". Stability was the wrong guarantee to reach for: the
	// query above has no ORDER BY, so the arrival order it preserved was itself
	// undefined, and the top-N cut below then admitted whichever tied memories
	// the database happened to hand over first. Ids are unique, so deciding on them
	// makes the comparator total and the result a function of the candidate set.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].memory.ID < candidates[j].memory.ID
	})

	// Take top N
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	result := make([]Memory, len(candidates))
	for i, c := range candidates {
		result[i] = c.memory
	}

	return result, nil
}
