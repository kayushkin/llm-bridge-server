// Package textutil holds string helpers shared by the server and its command
// binaries.
//
// Everything here counts runes, never bytes. Cutting a Go string at a byte
// offset splits whatever rune straddles that offset, and the result is not
// valid UTF-8. Nothing reports it: encoding/json substitutes U+FFFD rather
// than returning an error, so a display name reaches the dash as a replacement
// character with no error raised anywhere along the path.
package textutil

// TruncateToRuneLimit returns text limited to at most maxRunes runes. It cuts
// only on a rune boundary, so the result is always valid UTF-8 whenever text
// is. A non-positive maxRunes yields the empty string.
//
// It appends nothing. A caller that wants an ellipsis appends its own, which
// keeps the question of whether the ellipsis counts against the limit where
// the caller can answer it.
func TruncateToRuneLimit(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	// Ranging a string yields the byte offset of each rune's first byte, so
	// the offset seen on the (maxRunes+1)th rune is exactly where to cut.
	runeCount := 0
	for byteOffset := range text {
		runeCount++
		if runeCount > maxRunes {
			return text[:byteOffset]
		}
	}
	return text
}
