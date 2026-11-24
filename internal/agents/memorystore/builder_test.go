package memorystore

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestBuildContext(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "build-context-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	store, err := NewStore(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Set up test memories
	memories := []Memory{
		{
			ID:         "identity",
			Content:    "I am a test agent",
			Tags:       []string{"identity"},
			Importance: 1.0,
			Source:     "system",
			AlwaysLoad: true,
		},
		{
			ID:         uuid.NewString(),
			Content:    "User prefers blue color",
			Tags:       []string{"preference", "user-info"},
			Importance: 0.8,
			Source:     "user",
		},
		{
			ID:         uuid.NewString(),
			Content:    "How to implement feature X in Go",
			Tags:       []string{"go", "feature-x", "code"},
			Importance: 0.7,
			Source:     "agent",
		},
		{
			ID:         uuid.NewString(),
			Content:    "Test file location is /tests/",
			Tags:       []string{"test", "location"},
			Importance: 0.5,
			Source:     "agent",
		},
		{
			ID:         uuid.NewString(),
			Content:    "Old archived decision",
			Tags:       []string{"decision", "archived"},
			Importance: 0.3,
			Source:     "agent",
		},
		{
			ID:         uuid.NewString(),
			Content:    "Low importance filler content that is very long and uses many tokens to test budget limits and size filtering" + string(make([]byte, 1000)),
			Tags:       []string{"filler"},
			Importance: 0.2,
			Source:     "system",
		},
	}

	// Create an expired memory
	pastTime := time.Now().Add(-1 * time.Hour)
	expiredMemory := Memory{
		ID:         uuid.NewString(),
		Content:    "This should not appear (expired)",
		Tags:       []string{"expired"},
		Importance: 0.9,
		Source:     "system",
		ExpiresAt:  &pastTime,
	}
	memories = append(memories, expiredMemory)

	for _, m := range memories {
		if err := store.Save(m); err != nil {
			t.Fatalf("failed to save memory %s: %v", m.ID, err)
		}
	}

	// Test 1: Basic context building with tag matching
	t.Run("TagMatching", func(t *testing.T) {
		req := BuildContextRequest{
			Tags:              []string{"go", "code"},
			TokenBudget:       10000,
			MinImportance:     0.0,
			IncludeAlwaysLoad: true,
		}

		results, tokens, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		// Should include identity (always-load) and the Go/code memory
		if len(results) < 2 {
			t.Errorf("expected at least 2 memories, got %d", len(results))
		}

		// First result should be identity (AlwaysLoad)
		if results[0].ID != "identity" {
			t.Errorf("expected first result to be identity, got %s", results[0].ID)
		}

		// Should include the Go-related memory
		foundGo := false
		for _, m := range results {
			if hasTag(m.Tags, "go") {
				foundGo = true
				break
			}
		}
		if !foundGo {
			t.Error("expected to find Go-related memory")
		}

		// Should NOT include expired memory
		for _, m := range results {
			if hasTag(m.Tags, "expired") {
				t.Error("expired memory should not be included")
			}
		}

		if tokens <= 0 {
			t.Error("expected positive token count")
		}
	})

	// Test 2: Token budget enforcement
	t.Run("TokenBudget", func(t *testing.T) {
		req := BuildContextRequest{
			Tags:              []string{"go", "preference", "code"},
			TokenBudget:       100, // very small budget
			MinImportance:     0.0,
			IncludeAlwaysLoad: true,
		}

		results, tokens, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		if tokens > req.TokenBudget {
			// AlwaysLoad memories can exceed budget, so this might happen
			// but at least identity should be included
			foundIdentity := false
			for _, m := range results {
				if m.ID == "identity" {
					foundIdentity = true
					break
				}
			}
			if !foundIdentity {
				t.Error("identity should always be included even if over budget")
			}
		}
	})

	// Test 3: Exclude tags
	t.Run("ExcludeTags", func(t *testing.T) {
		req := BuildContextRequest{
			Tags:              []string{"test", "code"},
			TokenBudget:       10000,
			ExcludeTags:       []string{"test"},
			IncludeAlwaysLoad: true,
		}

		results, _, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		// Should NOT include test-tagged memories
		for _, m := range results {
			if hasTag(m.Tags, "test") {
				t.Error("memory with excluded tag should not be included")
			}
		}
	})

	// Test 4: Importance threshold
	t.Run("MinImportance", func(t *testing.T) {
		req := BuildContextRequest{
			Tags:              []string{"decision", "preference"},
			TokenBudget:       10000,
			MinImportance:     0.5,
			IncludeAlwaysLoad: false, // disable to test threshold
		}

		results, _, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		// All results should have importance >= 0.5
		for _, m := range results {
			if m.Importance < 0.5 && !m.AlwaysLoad {
				t.Errorf("memory %s has importance %f < 0.5", m.ID, m.Importance)
			}
		}

		// Should NOT include archived decision (importance 0.3)
		for _, m := range results {
			if hasTag(m.Tags, "archived") {
				t.Error("low importance memory should be filtered out")
			}
		}
	})

	// Test 5: AlwaysLoad priority
	t.Run("AlwaysLoadFirst", func(t *testing.T) {
		req := BuildContextRequest{
			Tags:              []string{},
			TokenBudget:       10000,
			MinImportance:     0.0,
			IncludeAlwaysLoad: true,
		}

		results, _, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		// First memories should be AlwaysLoad
		if len(results) > 0 && !results[0].AlwaysLoad {
			t.Error("first memory should be AlwaysLoad")
		}
	})

	// Test 6: MaxChunkSize filtering
	t.Run("MaxChunkSize", func(t *testing.T) {
		req := BuildContextRequest{
			Tags:              []string{"filler"},
			TokenBudget:       100000,
			MinImportance:     0.0,
			MaxChunkSize:      100, // exclude large chunks
			IncludeAlwaysLoad: false,
		}

		results, _, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		// Should NOT include the large filler content
		for _, m := range results {
			if hasTag(m.Tags, "filler") {
				t.Error("large chunk should be filtered by MaxChunkSize")
			}
		}
	})

	// Test 7: TruncateThreshold
	t.Run("TruncateThreshold", func(t *testing.T) {
		// Save a large memory
		largeContent := ""
		for i := 0; i < 200; i++ {
			largeContent += "This is a line of content that makes this memory quite large. "
		}
		store.Save(Memory{
			ID:         "large-truncatable",
			Content:    largeContent,
			Tags:       []string{"trunctest"},
			Importance: 0.8,
			Tokens:     (len(largeContent) + 2) / 3,
		})

		req := BuildContextRequest{
			Tags:              []string{"trunctest"},
			TokenBudget:       100000,
			MinImportance:     0.0,
			IncludeAlwaysLoad: false,
			TruncateThreshold: 100,  // Truncate anything over 100 tokens
			TruncatePreview:   200,  // 200 char preview
		}

		results, _, err := store.BuildContext(req)
		if err != nil {
			t.Fatalf("BuildContext failed: %v", err)
		}

		found := false
		for _, m := range results {
			if m.ID == "large-truncatable" {
				found = true
				if len(m.Content) > 500 {
					t.Errorf("expected truncated content, got %d chars", len(m.Content))
				}
				if !contains(m.Content, "memory_expand") {
					t.Error("truncated content should contain memory_expand hint")
				}
			}
		}
		if !found {
			t.Error("large-truncatable memory not found in results")
		}
	})
}
