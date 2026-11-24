package memorystore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// nullString returns a sql.NullString from a string.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// nullInt64 returns a sql.NullInt64 from a time.Time.
func nullInt64(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{Valid: false}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func nullInt64Ptr(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{Valid: false}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// loadLazyContent loads content for lazy-loaded references from their source.
func (s *Store) loadLazyContent(m *Memory) error {
	switch m.RefType {
	case "file", "identity":
		if m.RefTarget == "" {
			return fmt.Errorf("%s reference missing ref_target path", m.RefType)
		}
		data, err := os.ReadFile(m.RefTarget)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.RefTarget, err)
		}
		m.Content = string(data)
		m.Tokens = len(m.Content) / 4
		return nil
	default:
		return nil // content already in DB
	}
}

// truncateAtRuneBoundary returns the longest prefix of s that is no longer than
// maxBytes and does not end part-way through a multi-byte UTF-8 sequence.
//
// Cutting a Go string at a fixed byte offset splits whatever rune straddles that
// offset, and the result is not valid UTF-8. Nothing reports it: encoding/json
// substitutes U+FFFD rather than failing, so the reader sees a replacement
// character and no error is raised anywhere along the way. Any code that cuts
// user or agent text down to a byte budget has to walk the cut back to a
// boundary first.
//
// The walk-back costs at most three byte comparisons and allocates nothing,
// which is why it is preferred here over converting to []rune.
func truncateAtRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is the first byte past the prefix. While it is a continuation
	// byte, a rune straddles the cut, so move the cut earlier.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// DefaultMemoryPath returns the default path for the memory database.
func DefaultMemoryPath(rootDir string) string {
	return filepath.Join(rootDir, ".inber", "memory.db")
}