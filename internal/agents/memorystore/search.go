package memorystore

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
// bm25() returns 0 for a term that appears in every indexed document, because
// such a term tells you nothing about which document you want. If the query
// consists only of such terms, every candidate scores 0, the product with
// importance and recency is 0 for all of them, and the ranking collapses onto
// the id tie-break — importance and recency lose their vote for no reason.
//
// This is applied only when the WHOLE candidate set is tied at zero, so it can
// never reorder a set that BM25 did separate.
const zeroInformationRelevance = 1.0

// SearchFiltered finds memories matching the query, optionally filtered by orchestrator.
// If orchestrator is empty, all memories are searched.
func (s *Store) SearchFiltered(query string, limit int, orchestrator string) ([]Memory, error) {
	return s.searchInternal(query, limit, orchestrator)
}

// Search finds memories matching the query, ranked by relevance, importance and
// recency.
//
// Relevance is BM25 over an FTS5 index of the content and summary. It used to be
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
	match, ok := ftsMatchExpression(query)
	if !ok {
		return []Memory{}, nil
	}

	now := time.Now()
	sqlQuery := `
	SELECT m.id, m.content, m.summary, m.original_id, m.importance, m.access_count,
	       m.last_accessed, m.created_at, m.source, m.embedding, m.always_load,
	       m.expires_at, m.tokens, m.ref_type, m.ref_target, m.is_lazy, m.orchestrator,
	       bm25(` + ftsTableName + `), ` + ftsTableName + `.memory_id
	FROM ` + ftsTableName + `
	JOIN memories m ON m.rowid = ` + ftsTableName + `.rowid
	WHERE ` + ftsTableName + ` MATCH ?
	  AND m.importance > 0
	  AND (m.expires_at IS NULL OR m.expires_at > ?)
	`
	args := []interface{}{match, now.Unix()}
	if orchestrator != "" {
		sqlQuery += " AND m.orchestrator = ?"
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
		var bm25Score float64
		var indexedMemoryID string

		err := rows.Scan(
			&m.ID, &m.Content, &summary, &originalID,
			&m.Importance, &m.AccessCount, &lastAccessed, &createdAt, &m.Source, &embJSON,
			&m.AlwaysLoad, &expiresAt, &m.Tokens,
			&m.RefType, &refTarget, &m.IsLazy, &m.Orchestrator,
			&bm25Score, &indexedMemoryID,
		)
		if err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}

		// The index is keyed on memories.rowid, which a VACUUM of that table is
		// free to renumber. Refuse rather than answer with the wrong memory.
		if indexedMemoryID != m.ID {
			return nil, ftsRowidDriftError(indexedMemoryID, m.ID)
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

		// bm25() is negative and orders ascending — a more relevant document is
		// a more negative number — so the sign flip is what turns it into a
		// score that composes with importance and recency.
		relevance := -bm25Score
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
	// accepts parameters answered 200 with every result's tag list empty.
	// loadTagsForMemories chunks under that ceiling; a failure is now returned.
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
	// SQLite happened to hand over first. Ids are unique, so deciding on them
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
