// Package textcut shortens a string to a bound, without ever cutting a rune in
// half and without overrunning the bound it was given.
//
// It exists because this repo wrote the same helper twice, once in
// cmd/ask and once in cmd/kanban-classifier, and both copies had the same two
// defects: they measured with len (bytes) a bound their caller meant in
// characters, and they appended a three-byte ellipsis after reserving one byte
// or none for it. Neither copy stayed inside its bound, and the byte cut landed
// mid-rune on any non-ASCII value. What reaches a store or a UI is then a name
// cut to a third of its length with a replacement character where a word was,
// because encoding/json substitutes U+FFFD for the broken bytes and returns a
// nil error.
//
// The unit is the caller's decision and it is not always the same one, so it is
// named in the function rather than assumed: use TruncateToRuneLimit where the
// bound is a count of characters somebody reads or was promised, and count
// bytes where the bound is a size budget.
package textcut

import "unicode/utf8"

// Ellipsis marks a value that was shortened. One rune, three bytes; the
// functions here count it against the bound rather than adding it afterwards.
const Ellipsis = "…"

// TruncateToRuneLimit caps s at maxRunes CODE POINTS, the ellipsis included, and
// returns s unchanged when it already fits.
//
// Use it where the bound is a count of characters: something a human reads in a
// list, or a length another program was promised. Cutting such a bound in bytes
// measures the value against a number nobody meant — 60 characters of Japanese
// is 180 bytes, so a byte cut at 60 keeps 22 characters and discards two thirds
// of a value that broke no rule.
func TruncateToRuneLimit(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	kept := 0
	// Ranging a string yields the byte index of each rune START, so the slice
	// below can never land inside one.
	for i := range s {
		if kept == maxRunes-1 {
			return s[:i] + Ellipsis
		}
		kept++
	}
	return s
}
