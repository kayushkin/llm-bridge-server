package memorystore

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BuildContextRequest specifies how to build context from memory
type BuildContextRequest struct {
	Tags              []string // Tags to match (from message/query)
	TokenBudget       int      // Maximum tokens to include
	MinImportance     float64  // Minimum importance threshold (default: 0.0)
	ExcludeTags       []string // Tags to exclude (e.g., "test", "archived")
	IncludeAlwaysLoad bool     // Whether to include AlwaysLoad memories (default: true)
	MaxChunkSize      int      // Skip memories larger than this (default: 0 = no limit)
	TruncateThreshold int      // Truncate memories larger than this to a preview (default: 0 = no truncation)
	TruncatePreview   int      // How many chars to keep in preview (default: 300)
}

// BuildContext retrieves memories suitable for including in a prompt.
// Returns memories ordered by priority and total tokens used.
//
// Priority order:
// 1. AlwaysLoad memories (identity, instructions)
// 2. Tag-matched memories (more matches = higher priority)
// 3. High importance memories
// 4. Recently accessed memories
func (s *Store) BuildContext(req BuildContextRequest) ([]Memory, int, error) {
	// Set defaults
	if req.TokenBudget <= 0 {
		req.TokenBudget = 32000
	}
	if !req.IncludeAlwaysLoad && req.MinImportance == 0 {
		req.MinImportance = 0.4 // reasonable default if not including always-load
	}

	// Build query.
	//
	// The embedding column is deliberately absent. BuildContext ranks on
	// importance, tag overlap and recency — it never calls CosineSimilarity —
	// so selecting the vector meant reading and JSON-decoding roughly 780 bytes
	// per row to throw it away, which was the single largest cost in this
	// function.
	//
	// This used to end "Search is the path that ranks by embedding, and it still
	// selects it". Search now ranks on BM25 over an FTS5 index and no longer
	// ranks on the embedding either, though it still selects the column because
	// it returns the whole row. Nothing on this box ranks by embedding any more.
	//
	// There is deliberately no ORDER BY. The ordering contract lives in the
	// comparator below, which is total, so the result does not depend on the
	// order rows arrive in and an ORDER BY would only pay for it twice. It is
	// not free either: `ORDER BY id` turns this from
	// `SEARCH memories USING INDEX idx_importance (importance>?)` into a full
	// `SCAN memories USING INDEX sqlite_autoindex_memories_1`, which on the
	// largest store on this host reads all 35,764 rows to find the 47 live ones.
	now := time.Now()
	query := `
	SELECT id, content, summary, original_id, importance, access_count, last_accessed, created_at, source, always_load, expires_at, tokens, ref_type, ref_target, is_lazy
	FROM memories
	WHERE importance >= ?
	  AND (expires_at IS NULL OR expires_at > ?)
	`
	args := []interface{}{req.MinImportance, now.Unix()}

	// Add exclusion filter if needed
	if len(req.ExcludeTags) > 0 {
		placeholders := make([]string, len(req.ExcludeTags))
		for i := range req.ExcludeTags {
			placeholders[i] = "?"
			args = append(args, req.ExcludeTags[i])
		}
		query += ` AND id NOT IN (
			SELECT memory_id FROM memory_tags WHERE tag IN (` + join(placeholders, ",") + `)
		)`
	}

	// Execute query
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// Scan all candidate memories
	var candidates []memoryWithScore
	tagSet := make(map[string]bool)
	for _, tag := range req.Tags {
		tagSet[tag] = true
	}

	for rows.Next() {
		m, err := s.scanMemoryRowWithoutEmbedding(rows)
		if err != nil {
			continue
		}

		// Skip oversized memories if limit set
		if req.MaxChunkSize > 0 && m.Tokens > req.MaxChunkSize {
			continue
		}

		candidates = append(candidates, memoryWithScore{memory: m})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate memories: %w", err)
	}
	rows.Close()

	// Tags for the whole candidate set in one pass. Asking per candidate cost a
	// round trip each, issued while the row cursor above was still open.
	candidateIDs := make([]string, len(candidates))
	for i := range candidates {
		candidateIDs[i] = candidates[i].memory.ID
	}
	tagsByMemory, err := s.loadTagsForMemories(candidateIDs)
	if err != nil {
		return nil, 0, err
	}
	for i := range candidates {
		candidates[i].memory.Tags = tagsByMemory[candidates[i].memory.ID]
		candidates[i].score = calculateScore(candidates[i].memory, tagSet)
	}

	// Sort by priority.
	//
	// The comparator is total — every pair of distinct candidates is decided,
	// with id as the last resort — and that is load-bearing, not tidiness. The
	// survivors of the budget cut below become the entire `system` array of an
	// Anthropic request and carry a cache breakpoint. Anthropic hashes
	// tools -> system -> messages in order, so two memories swapping places
	// invalidate that breakpoint and every breakpoint after it, and the whole
	// conversation is re-charged at the 1.25x cache-write rate instead of the
	// 0.10x read rate.
	//
	// Leaving a pair undecided does not merely leave their order arbitrary:
	// sort.Slice is pdqsort, whose placement of equal elements depends on the
	// whole input, so an undecided pair makes the OUTPUT depend on the order
	// SQLite handed the rows over in. That order is not fixed either — the scan
	// runs on idx_importance, and updateAccess multiplies importance by 1.01 on
	// every read while DecayImportance multiplies it by 0.99 daily, so rows move
	// within that index constantly. Measured with the tie-break removed: the
	// same forty equally-scoring memories, written in the opposite order, put a
	// completely disjoint set of ten in the prompt.
	sort.Slice(candidates, func(i, j int) bool {
		// AlwaysLoad memories always come first
		if candidates[i].memory.AlwaysLoad != candidates[j].memory.AlwaysLoad {
			return candidates[i].memory.AlwaysLoad
		}
		// Within the always-load head, order by id and nothing else.
		//
		// Score cannot decide MEMBERSHIP here: the budget cut below appends an
		// always-load memory whether or not it fits, so every always-load
		// candidate reaches the result and the running token total after the
		// head is the same whatever order they went in. Score can only decide
		// their BYTES — and it is the wrong thing to decide them with, because
		// every one of its three inputs varies from turn to turn while the head
		// itself does not. calculateScore adds 0.3 per tag matching the current
		// user message, adds a wall-clock recency bonus that steps down as
		// last_accessed crosses one day and one week, and starts from an
		// importance that updateAccess multiplies by 1.01 on every read and
		// DecayImportance multiplies by 0.99 daily. Sorting the head by any of
		// those lets a memory move for reasons that have nothing to do with the
		// head's contents, and this head is the front of the `system` array
		// that carries the caller's cache breakpoint — so a swap here
		// invalidates it and every breakpoint after it, re-charging the whole
		// conversation at the write rate.
		//
		// Ids are unique and do not drift, so this is total on its own and the
		// head's order is fixed by construction rather than by the scores
		// happening not to cross.
		if candidates[i].memory.AlwaysLoad {
			return candidates[i].memory.ID < candidates[j].memory.ID
		}
		// Then by score
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		// Then smaller memories first (more likely to fit)
		if candidates[i].memory.Tokens != candidates[j].memory.Tokens {
			return candidates[i].memory.Tokens < candidates[j].memory.Tokens
		}
		// Ids are unique, so nothing below this line is undecided.
		return candidates[i].memory.ID < candidates[j].memory.ID
	})

	// Set truncation defaults
	truncateThreshold := req.TruncateThreshold
	truncatePreview := req.TruncatePreview
	if truncatePreview <= 0 {
		truncatePreview = 300 // ~100 tokens preview
	}

	// Build result list within budget
	var result []Memory
	tokensUsed := 0

	for _, candidate := range candidates {
		m := candidate.memory

		// Auto-truncate large memories to preview + expand hint
		if truncateThreshold > 0 && m.Tokens > truncateThreshold && !m.AlwaysLoad {
			m = truncateMemoryToPreview(m, truncatePreview)
		}

		// Check budget
		if tokensUsed+m.Tokens > req.TokenBudget {
			if m.AlwaysLoad {
				result = append(result, m)
				tokensUsed += m.Tokens
			}
			continue
		}

		result = append(result, m)
		tokensUsed += m.Tokens
	}

	// Partition: stable memories first, volatile (file refs, recent) last.
	// This preserves prompt cache hits — stable prefix doesn't change between turns.
	result = partitionStableFirst(result)

	return result, tokensUsed, nil
}

// scanMemoryRowWithoutEmbedding scans a memory from a database row whose SELECT
// omits the embedding column, leaving Memory.Embedding nil.
//
// The name says so because the omission is invisible at the call site otherwise:
// a ranking function that reached for the vector here would compare against an
// empty one and score every memory identically rather than fail.
func (s *Store) scanMemoryRowWithoutEmbedding(row *sql.Rows) (Memory, error) {
	var m Memory
	var summary, originalID, refTarget sql.NullString
	var lastAccessed, createdAt int64
	var expiresAt sql.NullInt64

	err := row.Scan(
		&m.ID, &m.Content, &summary, &originalID,
		&m.Importance, &m.AccessCount, &lastAccessed, &createdAt, &m.Source,
		&m.AlwaysLoad, &expiresAt, &m.Tokens,
		&m.RefType, &refTarget, &m.IsLazy,
	)
	if err != nil {
		return m, err
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

	return m, nil
}

// tagLookupChunkSize is how many memory ids go into one `IN (...)` list.
//
// It is a correctness bound, not a tuning knob. Every id becomes a bound
// parameter, and SQLite refuses a statement with more parameters than
// SQLITE_MAX_VARIABLE_NUMBER — measured against this driver, 32,766 ids answer
// and 32,767 fail with "too many SQL variables". That ceiling has been 999 in
// older SQLite builds, so the chunk stays under the lower of the two and the
// query cannot depend on which build it is linked against.
const tagLookupChunkSize = 900

// loadTagsForMemories returns the tags of every named memory, keyed by memory id.
//
// A memory with no tags is absent from the map rather than present and empty;
// callers that need an empty slice should default it themselves.
func (s *Store) loadTagsForMemories(ids []string) (map[string][]string, error) {
	tagsByMemory := make(map[string][]string, len(ids))

	for start := 0; start < len(ids); start += tagLookupChunkSize {
		end := start + tagLookupChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}

		query := "SELECT memory_id, tag FROM memory_tags WHERE memory_id IN (" +
			join(placeholders, ",") + ")"
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("load tags for %d memories: %w", len(chunk), err)
		}
		for rows.Next() {
			var memoryID, tag string
			if err := rows.Scan(&memoryID, &tag); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan memory tag: %w", err)
			}
			tagsByMemory[memoryID] = append(tagsByMemory[memoryID], tag)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate memory tags: %w", err)
		}
		rows.Close()
	}

	return tagsByMemory, nil
}

// partitionStableFirst moves volatile memories (file refs, recent files) to the end
// while preserving relative order within each group (stable sort).
func partitionStableFirst(memories []Memory) []Memory {
	var stable, volatile []Memory
	for _, m := range memories {
		if isVolatileMemory(m) {
			volatile = append(volatile, m)
		} else {
			stable = append(stable, m)
		}
	}
	return append(stable, volatile...)
}

// isVolatileMemory returns true for memories that change between turns
// (file references from tool calls, recent file scans).
//
// The prefix tests are strings.HasPrefix and must stay that way. They were
// hand-rolled as `len(m.ID) > 8 && m.ID[:8] == "fileref:"`, which is HasPrefix
// with the boundary wrong: an id that is EXACTLY the bare prefix has length 8,
// fails `> 8`, and was classified stable. inber implements the same predicate
// over the same ids with strings.HasPrefix (engine/turn_prompt.go
// isVolatileMemoryID), so the two copies disagreed on precisely those ids —
// one would park such a memory in the cached system prefix while the other
// treated it as volatile. Degenerate, but it is a divergence between two rules
// that are supposed to be one, and it cost nothing to remove.
func isVolatileMemory(m Memory) bool {
	if strings.HasPrefix(m.ID, "fileref:") {
		return true
	}
	if strings.HasPrefix(m.ID, "recent:") {
		return true
	}
	if strings.HasPrefix(m.ID, "file:") {
		return true
	}
	for _, tag := range m.Tags {
		if tag == "recent" {
			return true
		}
	}
	return false
}

// truncateMemoryToPreview replaces a large memory's content with a preview
// and a hint to use memory_expand(id) for the full content.
//
// Truncation is skipped if the preview would be >50% of the original content —
// in that case it's not worth truncating since most of the content would still load.
func truncateMemoryToPreview(m Memory, previewChars int) Memory {
	content := m.Content
	// Use summary if available and content is empty (lazy ref)
	if content == "" && m.Summary != "" {
		content = m.Summary
	}
	if len(content) <= previewChars {
		return m // Already small enough
	}

	// Only truncate if we're removing more than 50% of the content.
	// If the preview is >50% of the full content, just include it all —
	// truncating saves little context and loses information.
	if previewChars*2 >= len(content) {
		return m
	}

	// The word/line walk-back below is already rune-safe, because neither a
	// space nor a newline can appear inside a multi-byte UTF-8 sequence. This
	// first cut is not, and it is what survives when the walk-back finds no
	// break past halfway — which is the usual case for scripts that do not
	// separate words with an ASCII space.
	preview := truncateAtRuneBoundary(content, previewChars)
	// Try to break at a word/line boundary
	if lastNewline := lastIndexByte(preview, '\n'); lastNewline > previewChars/2 {
		preview = preview[:lastNewline]
	} else if lastSpace := lastIndexByte(preview, ' '); lastSpace > previewChars/2 {
		preview = preview[:lastSpace]
	}

	omittedTokens := m.Tokens - (len(preview)+2)/3
	m.Content = preview + "\n\n[... truncated — use memory_expand(\"" + m.ID + "\") for full content (" + itoa(m.Tokens) + " tokens, " + itoa(omittedTokens) + " omitted)]"
	m.Tokens = (len(m.Content) + 2) / 3
	return m
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// memoryWithScore pairs a memory with its relevance score
type memoryWithScore struct {
	memory Memory
	score  float64
}

// calculateScore computes a relevance score for a memory
func calculateScore(m Memory, tagSet map[string]bool) float64 {
	score := m.Importance

	// Tag matching bonus
	matchCount := 0
	for _, tag := range m.Tags {
		if tagSet[tag] {
			matchCount++
		}
	}
	score += float64(matchCount) * 0.3 // each matching tag adds 0.3

	// Recency bonus (recently accessed memories are more relevant)
	daysSinceAccess := time.Since(m.LastAccessed).Hours() / 24
	if daysSinceAccess < 1 {
		score += 0.2
	} else if daysSinceAccess < 7 {
		score += 0.1
	}

	return score
}

// hasTag checks if a tag list contains a specific tag
func hasTag(tags []string, target string) bool {
	for _, tag := range tags {
		if tag == target {
			return true
		}
	}
	return false
}

// join is a simple string join helper
func join(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}