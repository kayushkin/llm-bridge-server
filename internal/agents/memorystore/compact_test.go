package memorystore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCompact_NoEligible(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Save a high-importance memory
	err = store.Save(Memory{
		ID:         "mem-1",
		Content:    "important memory",
		Tags:       []string{"test"},
		Importance: 0.9,
		Source:     "user",
	})
	if err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	results, err := store.Compact(24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no compaction, got %d results", len(results))
	}
}

func TestCompact_GroupsAndCompacts(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Save old, low-importance, low-access memories with same tag
	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		err = store.Save(Memory{
			ID:          fmt.Sprintf("old-mem-%d", i),
			Content:     fmt.Sprintf("old content %d", i),
			Tags:        []string{"project-x"},
			Importance:  0.3,
			AccessCount: 1,
			Source:      "agent",
			CreatedAt:   oldTime,
		})
		if err != nil {
			t.Fatalf("failed to save: %v", err)
		}
	}

	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 compaction group, got %d", len(results))
	}

	r := results[0]
	if r.Count != 3 {
		t.Errorf("expected 3 compacted memories, got %d", r.Count)
	}
	if len(r.OriginalIDs) != 3 {
		t.Errorf("expected 3 original IDs, got %d", len(r.OriginalIDs))
	}

	// Verify originals are soft-deleted
	for _, id := range r.OriginalIDs {
		m, err := store.Get(id)
		if err != nil {
			continue // may fail due to importance=0 filter
		}
		if m.Importance > 0 {
			t.Errorf("expected importance 0 for %s, got %f", id, m.Importance)
		}
	}

	// Verify new compacted memory exists
	newMem, err := store.Get(r.NewID)
	if err != nil {
		t.Fatalf("failed to get compacted memory: %v", err)
	}
	if newMem.Source != "compaction" {
		t.Errorf("expected source 'compaction', got %q", newMem.Source)
	}
}
