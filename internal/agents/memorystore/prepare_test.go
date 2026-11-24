package memorystore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestPrepareSession(t *testing.T) {
	// Create temp dir for test repo
	tmpDir, err := os.MkdirTemp("", "prepare-session-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create some test files with different modification times
	testFiles := []struct {
		path   string
		age    time.Duration
		create bool
	}{
		{"recent1.go", 30 * time.Minute, true},  // 30 min ago
		{"recent2.py", 2 * time.Hour, true},      // 2 hours ago
		{"old.txt", 48 * time.Hour, true},        // 2 days ago (should not appear)
		{"subdir/nested.js", 1 * time.Hour, true}, // 1 hour ago
	}

	for _, tf := range testFiles {
		if !tf.create {
			continue
		}
		path := filepath.Join(tmpDir, tf.path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test content"), 0644); err != nil {
			t.Fatal(err)
		}
		// Set modification time
		modTime := time.Now().Add(-tf.age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}

	// Create temp DB
	dbFile, err := os.CreateTemp("", "prepare-session-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dbFile.Name())

	store, err := NewStore(dbFile.Name())
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Prepare session
	cfg := PrepareSessionConfig{
		RootDir:        tmpDir,
		IdentityText:   "I am a test agent",
		AgentName:      "test-agent",
		RecencyWindow:  24 * time.Hour,
		RecentFilesTTL: 10 * time.Minute,
	}

	if err := store.PrepareSession(context.Background(), cfg); err != nil {
		t.Fatalf("PrepareSession failed: %v", err)
	}

	// Test 1: Identity is loaded and always-load
	t.Run("Identity", func(t *testing.T) {
		identity, err := store.Get("identity")
		if err != nil {
			t.Fatalf("failed to get identity: %v", err)
		}
		if identity.Content != "I am a test agent" {
			t.Errorf("expected identity content 'I am a test agent', got '%s'", identity.Content)
		}
		if !identity.AlwaysLoad {
			t.Error("expected identity to be always-load")
		}
		if identity.Importance != 1.0 {
			t.Errorf("expected importance 1.0, got %f", identity.Importance)
		}
		if !hasTag(identity.Tags, "identity") {
			t.Error("expected identity tag")
		}
	})

	// Test 2: Memory instructions are loaded
	t.Run("MemoryInstructions", func(t *testing.T) {
		instructions, err := store.Get("memory-instructions")
		if err != nil {
			t.Fatalf("failed to get memory instructions: %v", err)
		}
		if !instructions.AlwaysLoad {
			t.Error("expected memory instructions to be always-load")
		}
		if !hasTag(instructions.Tags, "instructions") {
			t.Error("expected instructions tag")
		}
	})

	// Test 3: Recent files are loaded with TTL
	t.Run("RecentFiles", func(t *testing.T) {
		// Search for recent files
		results, err := store.Search("recent", 100)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}

		// Should find recent files (recent1.go, recent2.py, nested.js)
		// Should NOT find old.txt (older than 24h)
		recentCount := 0
		for _, m := range results {
			if testHasTag(m.Tags, "recent") {
				recentCount++
				
				// Verify it has an expiration
				if m.ExpiresAt == nil {
					t.Errorf("recent file memory %s should have ExpiresAt", m.ID)
				} else {
					// Should expire in ~10 minutes
					ttl := time.Until(*m.ExpiresAt)
					if ttl < 9*time.Minute || ttl > 11*time.Minute {
						t.Errorf("expected TTL ~10min, got %v", ttl)
					}
				}
				
				// Verify importance scoring
				if m.Importance < 0.4 || m.Importance > 0.8 {
					t.Errorf("expected importance 0.4-0.8, got %f", m.Importance)
				}
			}
		}

		if recentCount < 3 {
			t.Errorf("expected at least 3 recent files, got %d", recentCount)
		}
	})

	// Test 4: Old files are NOT loaded
	t.Run("OldFilesExcluded", func(t *testing.T) {
		results, err := store.Search("old.txt", 100)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}

		for _, m := range results {
			if testHasTag(m.Tags, "file:old.txt") {
				t.Error("old.txt should not be loaded (older than recency window)")
			}
		}
	})
}

func TestPrepareSession_WithIdentityFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "prepare-session-file-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create identity file
	identityPath := filepath.Join(tmpDir, "identity.md")
	identityContent := "# Test Agent\nI help with testing."
	if err := os.WriteFile(identityPath, []byte(identityContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create temp DB
	dbFile, err := os.CreateTemp("", "prepare-session-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dbFile.Name())

	store, err := NewStore(dbFile.Name())
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Prepare session with identity file
	cfg := PrepareSessionConfig{
		RootDir:       tmpDir,
		IdentityFile:  identityPath,
		AgentName:     "test-agent",
		RecencyWindow: 0, // disable recent files for this test
	}

	if err := store.PrepareSession(context.Background(), cfg); err != nil {
		t.Fatalf("PrepareSession failed: %v", err)
	}

	// Verify identity loaded from file
	identity, err := store.Get("identity")
	if err != nil {
		t.Fatalf("failed to get identity: %v", err)
	}
	if identity.Content != identityContent {
		t.Errorf("expected identity from file, got '%s'", identity.Content)
	}
}

func testHasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
