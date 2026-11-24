package memorystore

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func TestStore(t *testing.T) {
	// Create temporary database
	dbPath := "/tmp/test_memory_" + uuid.New().String() + ".db"
	defer os.Remove(dbPath)

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// Test Save
	m1 := Memory{
		ID:         uuid.New().String(),
		Content:    "The capital of France is Paris.",
		Tags:       []string{"geography", "fact"},
		Importance: 0.8,
		Source:     "user",
	}
	if err := store.Save(m1); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Test Get
	retrieved, err := store.Get(m1.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if retrieved.Content != m1.Content {
		t.Errorf("Content mismatch: got %q, want %q", retrieved.Content, m1.Content)
	}
	if len(retrieved.Tags) != 2 {
		t.Errorf("Tags count mismatch: got %d, want 2", len(retrieved.Tags))
	}
	// Importance gets bumped on access (1.01x)
	expectedImportance := math.Min(1.0, m1.Importance*1.01)
	if math.Abs(retrieved.Importance-expectedImportance) > 0.0001 {
		t.Errorf("Importance mismatch: got %f, want %f", retrieved.Importance, expectedImportance)
	}

	// Get again to check access tracking
	retrieved2, _ := store.Get(m1.ID)
	if retrieved2.AccessCount != 2 {
		t.Errorf("Access count should be 2 after second Get, got %d", retrieved2.AccessCount)
	}

	// Test Search
	m2 := Memory{
		ID:         uuid.New().String(),
		Content:    "Paris is known for the Eiffel Tower.",
		Tags:       []string{"geography", "landmark"},
		Importance: 0.7,
		Source:     "agent",
	}
	if err := store.Save(m2); err != nil {
		t.Fatalf("Save m2 failed: %v", err)
	}

	// Give async access update time to complete
	time.Sleep(50 * time.Millisecond)

	results, err := store.Search("Paris France", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) == 0 {
		t.Errorf("Search returned no results")
	}
	// First result should be one of our memories
	found := false
	for _, r := range results {
		if r.ID == m1.ID || r.ID == m2.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Search did not return our memories")
	}

	// Test Forget
	if err := store.Forget(m1.ID); err != nil {
		t.Fatalf("Forget failed: %v", err)
	}

	// Search should not return forgotten memory
	results, err = store.Search("Paris France", 10)
	if err != nil {
		t.Fatalf("Search after forget failed: %v", err)
	}
	for _, r := range results {
		if r.ID == m1.ID {
			t.Errorf("Search returned forgotten memory")
		}
	}

	// Test ListRecent
	recent, err := store.ListRecent(10, 0.5)
	if err != nil {
		t.Fatalf("ListRecent failed: %v", err)
	}
	if len(recent) == 0 {
		t.Errorf("ListRecent returned no results")
	}
}

func TestEmbedding(t *testing.T) {
	embedder := NewEmbedder()

	text1 := "The quick brown fox jumps over the lazy dog."
	text2 := "A fast brown fox leaps above a sleepy dog."
	text3 := "Python is a programming language."

	emb1 := embedder.Embed(text1)
	emb2 := embedder.Embed(text2)
	emb3 := embedder.Embed(text3)

	// Check vector size
	if len(emb1) != 256 {
		t.Errorf("Embedding size mismatch: got %d, want 256", len(emb1))
	}

	// Similar sentences should have higher similarity
	sim12 := CosineSimilarity(emb1, emb2)
	sim13 := CosineSimilarity(emb1, emb3)

	if sim12 <= sim13 {
		t.Errorf("Similar sentences should have higher similarity: sim12=%f, sim13=%f", sim12, sim13)
	}

	// Cosine similarity should be in [-1, 1]
	if sim12 < -1 || sim12 > 1 {
		t.Errorf("Cosine similarity out of range: %f", sim12)
	}
}

func TestDecayImportance(t *testing.T) {
	dbPath := "/tmp/test_decay_" + uuid.New().String() + ".db"
	defer os.Remove(dbPath)

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// Create a memory with old access time
	m := Memory{
		ID:           uuid.New().String(),
		Content:      "Old memory",
		Tags:         []string{"test"},
		Importance:   0.8,
		Source:       "test",
		LastAccessed: time.Now().Add(-48 * time.Hour), // 2 days ago
		CreatedAt:    time.Now().Add(-48 * time.Hour),
	}
	if err := store.Save(m); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Apply decay
	if err := store.DecayImportance(); err != nil {
		t.Fatalf("DecayImportance failed: %v", err)
	}

	// Check that importance was reduced
	retrieved, err := store.Get(m.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if retrieved.Importance >= m.Importance {
		t.Errorf("Importance should have decayed: was %f, now %f", m.Importance, retrieved.Importance)
	}
}
