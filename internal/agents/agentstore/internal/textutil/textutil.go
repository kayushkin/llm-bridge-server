// Package textutil holds string operations that respect UTF-8 rune boundaries.
//
// Go strings are bytes. Indexing one at a fixed byte offset splits whatever rune
// straddles that offset, and the result is not valid UTF-8. Nothing reports it:
// encoding/json substitutes U+FFFD for the invalid bytes rather than returning an
// error, so a corrupted string reaches its consumer with no error raised anywhere
// along the path. It is silent by construction, which is why this class survived
// across the fleet.
//
// The name and the semantics here deliberately match
// inber-party/internal/textutil, so a reader who greps the fleet for
// UpperFirstRune finds one function rather than two spellings of it.
package textutil

import (
	"unicode"
	"unicode/utf8"
)

// UpperFirstRune returns s with its first rune upper-cased and the rest left
// alone. The empty string is returned unchanged.
//
// The obvious spelling of this, strings.ToUpper(s[:1]) + s[1:], is wrong in two
// separate ways.
//
// It corrupts any string whose first rune is multi-byte, and corrupts it worse
// than a plain byte cut does: strings.ToUpper replaces the stray leading byte
// with a three-byte U+FFFD and the continuation bytes are left stranded behind
// it, so "🎯quest" comes back as invalid UTF-8 rather than merely
// mis-capitalised. A tail cut at least leaves a recognizable prefix.
//
// It also panics on the empty string, because ""[:1] is out of range. That is
// the louder failure of the two and the easier one to miss in review, since the
// expression reads as though it were guarded.
func UpperFirstRune(s string) string {
	// Belt-and-braces, not load-bearing, and sabotage.sh scores it as such.
	// Removing this early return changes no behavior: DecodeRuneInString("")
	// returns (RuneError, 0), so the invalid-lead-byte guard below already
	// returns "" unchanged. It is kept because stating the empty case here is
	// clearer than resting on a non-obvious stdlib return value -- but it is not
	// the thing standing between this function and the old code's panic.
	if s == "" {
		return s
	}
	first, width := utf8.DecodeRuneInString(s)
	if first == utf8.RuneError && width <= 1 {
		// s already starts with an invalid byte. Upper-casing it would replace
		// that byte with U+FFFD and change the string's length; leave it be.
		return s
	}
	return string(unicode.ToUpper(first)) + s[width:]
}
