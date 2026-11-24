package memorystore

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// TestCompactionContentStaysValidUTF8 pins the defect that compaction cut its
// merged content at a fixed byte offset. Compaction is the worst place in this
// package for that: the merged content is written to the database and the
// originals are then soft-deleted, so a split rune is not a display glitch that
// a reload fixes — it is the surviving copy of the memory.
//
// The padding slides a four-byte rune across the 2000-byte cut. pad=0 puts a
// rune boundary exactly on the cut and is the known-negative control: it passes
// against the unfixed code too, which is what proves the other three detect the
// straddle and not merely the presence of non-ASCII input.
func TestCompactionContentStaysValidUTF8(t *testing.T) {
	for pad := 0; pad < 4; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewStore(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}
			defer store.Close()

			// Long enough that the 2000-byte cut lands inside this first
			// memory's content, so the straddle offset is predictable.
			runic := strings.Repeat("a", pad) + strings.Repeat(fourByteRune, 600)

			oldTime := time.Now().Add(-10 * 24 * time.Hour)
			for i := 0; i < 3; i++ {
				content := runic
				if i > 0 {
					content = fmt.Sprintf("old content %d", i)
				}
				err = store.Save(Memory{
					ID:          fmt.Sprintf("old-mem-%d", i),
					Content:     content,
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

			compacted, err := store.Get(results[0].NewID)
			if err != nil {
				t.Fatalf("failed to get compacted memory: %v", err)
			}
			if !utf8.ValidString(compacted.Content) {
				t.Errorf("compacted content is not valid UTF-8: the 2000-byte cut split a rune\n"+
					"last 8 bytes: % x", tailBytes(compacted.Content, 8))
			}
		})
	}
}

// TestTruncateMemoryToPreviewStaysValidUTF8 pins the same defect on the preview
// path, which reaches it by a narrower route.
//
// The word-boundary walk-back in truncateMemoryToPreview is accidentally
// rune-safe: it looks for a space or a newline, and neither byte can occur inside
// a multi-byte UTF-8 sequence, so a cut it finds always lands on a boundary. The
// bug lives in the fallback, when no such byte is found past halfway — and that
// fallback is selected by text with no ASCII whitespace, which is exactly the
// text most likely to be multi-byte. The failure mode and the input that triggers
// it are correlated, so it does not show up under English test data at any length.
func TestTruncateMemoryToPreviewStaysValidUTF8(t *testing.T) {
	// No spaces and no newlines, so the walk-back finds nothing and the fallback
	// cut is the one under test.
	content := strings.Repeat(fourByteRune, 600)

	// 100 is a rune boundary (100 % 4 == 0) and is the known-negative control.
	for _, previewChars := range []int{100, 101, 102, 103} {
		t.Run(fmt.Sprintf("previewChars=%d", previewChars), func(t *testing.T) {
			out := truncateMemoryToPreview(Memory{
				ID:      "mem-1",
				Content: content,
				Tokens:  len(content) / 3,
			}, previewChars)

			if out.Content == content {
				t.Fatalf("content was not truncated, so this test is not exercising the cut")
			}
			if !utf8.ValidString(out.Content) {
				t.Errorf("preview is not valid UTF-8: the %d-byte cut split a rune", previewChars)
			}
		})
	}
}

func tailBytes(s string, n int) []byte {
	if len(s) < n {
		n = len(s)
	}
	return []byte(s[len(s)-n:])
}
