package memorystore

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// compactionGroupKeyForMemory returns the key a memory compacts under, and the
// primary tag to name the resulting compacted memory after. Two defects are
// fixed here; both merged memories that have nothing to do with each other, and
// compaction destroys the originals, so neither is cosmetic.
//
// 1. An untagged memory used to key on the literal "untagged". That is not an
// identity, it is the ABSENCE of one, worn by every memory the tagger could not
// classify — and the tagger classifies far less than it looks: Tag() has no
// case for source "agent", so ordinary agent prose gets no tags at all.
// Measured before the fix: three untagged memories on the front door key, a
// postgres pool size and a birthday merged into one memory and the originals
// were soft-deleted. The fix is a SUBSTITUTE IDENTITY rather than an exclusion
// — the memory's own id, which is unique, so it falls through to the
// len(members) < 2 skip below and is never merged with a stranger. Excluding
// untagged memories from the candidate query instead would look equivalent and
// is not: it would also hide them from any future grouping that CAN key them.
//
// 2. A tagged memory used to key on tags[0], which is not stable. Tag() builds
// its result by ranging a Go map, so the order is randomised per call and then
// frozen into memory_tags at save time. Measured over 200 calls on identical
// input: five different tags came back first (user 95, config.yaml 29,
// identity 28, error 24, hosts 24). Two memories with the same tag set landed
// in different groups by luck. Sorting makes the key deterministic.
//
// ⚠️ Sorting fixes the INSTABILITY, not the arbitrariness: the
// alphabetically-first tag is still a poor stand-in for what a memory is about,
// and {code,error} and {code,web} still group together under "code" while
// {error,web} does not. Whether this should key on the whole tag set instead is
// a real change in which memories merge, so it is filed as a decision rather
// than made here — noteboard todo f06202e9.
//
// The two prefixes keep the namespaces apart: without them a memory literally
// tagged "untagged" would land in the same group as the untagged ones, which is
// the exact collision this function exists to stop.
func compactionGroupKeyForMemory(memoryID string, tags []string) (key, primaryTag string) {
	if len(tags) == 0 {
		return "memory-id:" + memoryID, "untagged"
	}
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	return "tag:" + sorted[0], sorted[0]
}

// CompactionResult describes what was compacted.
type CompactionResult struct {
	OriginalIDs []string
	NewID       string
	Tags        []string
	Count       int
}

// Compact finds old, low-access, low-importance memories, groups them by tags,
// creates compacted summaries, and soft-deletes originals.
func (s *Store) Compact(minAge time.Duration, minCount int) ([]CompactionResult, error) {
	cutoff := time.Now().Add(-minAge).Unix()

	query := `
	SELECT id, content, importance, access_count, created_at
	FROM memories
	WHERE created_at < ? AND access_count < ? AND importance < 0.7 AND importance > 0
	`
	rows, err := s.db.Query(query, cutoff, minCount)
	if err != nil {
		return nil, fmt.Errorf("query compactable: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id      string
		content string
	}
	var candidates []candidate
	var candidateIDs []string

	for rows.Next() {
		var id, content string
		var importance float64
		var accessCount int
		var createdAt int64
		if err := rows.Scan(&id, &content, &importance, &accessCount, &createdAt); err != nil {
			continue
		}
		candidates = append(candidates, candidate{id: id, content: content})
		candidateIDs = append(candidateIDs, id)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Fetch tags for candidates
	tagMap := make(map[string][]string)
	if len(candidateIDs) > 0 {
		placeholders := make([]string, len(candidateIDs))
		args := make([]interface{}, len(candidateIDs))
		for i, id := range candidateIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		tagQuery := fmt.Sprintf("SELECT memory_id, tag FROM memory_tags WHERE memory_id IN (%s)",
			strings.Join(placeholders, ","))
		tagRows, err := s.db.Query(tagQuery, args...)
		if err == nil {
			defer tagRows.Close()
			for tagRows.Next() {
				var memID, tag string
				if err := tagRows.Scan(&memID, &tag); err == nil {
					tagMap[memID] = append(tagMap[memID], tag)
				}
			}
		}
	}

	// Group by primary tag. Compaction MERGES content and destroys the
	// originals, so the grouping key has to be a real identity. It was not, in
	// two separate ways, and both are load-bearing — read before changing.
	groups := make(map[string][]candidate)
	groupTags := make(map[string]map[string]bool)
	groupPrimaryTag := make(map[string]string)
	for _, c := range candidates {
		tags := tagMap[c.id]
		key, primaryTag := compactionGroupKeyForMemory(c.id, tags)
		groups[key] = append(groups[key], c)
		groupPrimaryTag[key] = primaryTag
		if groupTags[key] == nil {
			groupTags[key] = make(map[string]bool)
		}
		for _, t := range tags {
			groupTags[key][t] = true
		}
	}

	var results []CompactionResult

	for groupKey, members := range groups {
		if len(members) < 2 {
			continue // don't compact single memories
		}

		// Build combined content
		var parts []string
		var origIDs []string
		for _, m := range members {
			parts = append(parts, m.content)
			origIDs = append(origIDs, m.id)
		}
		// This cut has to respect rune boundaries. Compaction saves the merged
		// content and then soft-deletes the originals, so a rune split here is
		// not a display glitch — it is the surviving copy of the memory.
		combined := strings.Join(parts, "\n---\n")
		if len(combined) > 2000 {
			combined = truncateAtRuneBoundary(combined, 2000) + "..."
		}

		// Collect all tags for the group
		var allTags []string
		for t := range groupTags[groupKey] {
			allTags = append(allTags, t)
		}

		newID := fmt.Sprintf("compact-%s-%d", groupPrimaryTag[groupKey], time.Now().UnixNano())
		newMem := Memory{
			ID:         newID,
			Content:    fmt.Sprintf("[Compacted from %d memories]\n%s", len(members), combined),
			Tags:       allTags,
			Importance: 0.5,
			Source:     "compaction",
			OriginalID: origIDs[0],
		}

		if err := s.Save(newMem); err != nil {
			continue
		}

		// Soft-delete originals
		for _, id := range origIDs {
			s.db.Exec("UPDATE memories SET importance = 0 WHERE id = ?", id)
		}

		results = append(results, CompactionResult{
			OriginalIDs: origIDs,
			NewID:       newID,
			Tags:        allTags,
			Count:       len(members),
		})
	}

	return results, nil
}