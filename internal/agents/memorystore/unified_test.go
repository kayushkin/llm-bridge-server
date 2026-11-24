package memorystore

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func TestUnifiedFields(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "memory-unified-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	store, err := NewStore(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Test AlwaysLoad (identity)
	t.Run("AlwaysLoad", func(t *testing.T) {
		identity := Memory{
			ID:         uuid.NewString(),
			Content:    "I am Claxon, an AI coding agent",
			Tags:       []string{"identity"},
			Importance: 1.0,
			Source:     "system",
			AlwaysLoad: true,
		}

		if err := store.Save(identity); err != nil {
			t.Fatalf("failed to save identity: %v", err)
		}

		retrieved, err := store.Get(identity.ID)
		if err != nil {
			t.Fatalf("failed to retrieve identity: %v", err)
		}

		if !retrieved.AlwaysLoad {
			t.Error("expected AlwaysLoad=true, got false")
		}
		if retrieved.Tokens == 0 {
			t.Error("expected auto-computed tokens, got 0")
		}
	})

	// Test ExpiresAt (recent file)
	t.Run("ExpiresAt", func(t *testing.T) {
		expiresAt := time.Now().Add(10 * time.Minute)
		recentFile := Memory{
			ID:         uuid.NewString(),
			Content:    "Recently modified: agent/agent.go",
			Tags:       []string{"recent", "file:agent/agent.go"},
			Importance: 0.6,
			Source:     "system",
			ExpiresAt:  &expiresAt,
		}

		if err := store.Save(recentFile); err != nil {
			t.Fatalf("failed to save recent file: %v", err)
		}

		retrieved, err := store.Get(recentFile.ID)
		if err != nil {
			t.Fatalf("failed to retrieve recent file: %v", err)
		}

		if retrieved.ExpiresAt == nil {
			t.Fatal("expected ExpiresAt to be set")
		}
		// Unix timestamp loses nanosecond precision, so compare seconds only
		if retrieved.ExpiresAt.Unix() != expiresAt.Unix() {
			t.Errorf("expected ExpiresAt=%v, got %v", expiresAt, retrieved.ExpiresAt)
		}
	})

	// Test expired memories are excluded from search
	t.Run("ExpiredExcludedFromSearch", func(t *testing.T) {
		// Create an expired memory
		pastTime := time.Now().Add(-1 * time.Hour)
		expired := Memory{
			ID:         uuid.NewString(),
			Content:    "This is expired content",
			Tags:       []string{"expired"},
			Importance: 0.8,
			Source:     "system",
			ExpiresAt:  &pastTime,
		}

		if err := store.Save(expired); err != nil {
			t.Fatalf("failed to save expired memory: %v", err)
		}

		// Search should not return it
		results, err := store.Search("expired content", 10)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}

		for _, m := range results {
			if m.ID == expired.ID {
				t.Error("expired memory should not appear in search results")
			}
		}
	})

	// Test Tokens field
	t.Run("Tokens", func(t *testing.T) {
		mem := Memory{
			ID:         uuid.NewString(),
			Content:    "This is a test with some content",
			Tags:       []string{"test"},
			Importance: 0.5,
			Source:     "user",
		}

		if err := store.Save(mem); err != nil {
			t.Fatalf("failed to save memory: %v", err)
		}

		retrieved, err := store.Get(mem.ID)
		if err != nil {
			t.Fatalf("failed to retrieve memory: %v", err)
		}

		expectedTokens := (len(mem.Content) + 2) / 3
		if retrieved.Tokens != expectedTokens {
			t.Errorf("expected Tokens=%d, got %d", expectedTokens, retrieved.Tokens)
		}
	})
}
