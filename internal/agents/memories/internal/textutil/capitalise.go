// Package textutil holds string operations that Go's standard library either
// gets wrong or has deprecated.
//
// The name and the semantics here deliberately match
// inber-party/internal/textutil, so a reader who greps the fleet for
// TitleFirstRuneOfEachWord finds one function rather than two spellings of it.
package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// TitleFirstRuneOfEachWord returns s with the first rune of every word
// title-cased and every other rune left exactly as it was.
//
// It exists to replace strings.Title, which has been deprecated since Go 1.18
// and which breaks words at the ASCII apostrophe. Under strings.Title
// "don't panic" comes back "Don'T Panic" and "claxon's quest" comes back
// "Claxon'S Quest". Both of those reach quest names built from free text, so
// the defect is user-visible rather than theoretical.
//
// This function is strings.Title with exactly one deliberate difference: an
// ASCII apostrophe between two word characters does not end a word. It must be
// flanked to join, which is what keeps "don't" one word while leaving a leading
// or trailing apostrophe behaving as it always did, so "'lead" stays "'Lead"
// and "dogs'" stays "Dogs'". Everything else is preserved on purpose, because
// these call sites render existing user-facing strings and a wider change would
// be a silent redesign of their output:
//
//   - A hyphen, dot, slash or space still ends a word, so "code-introspection"
//     stays "Code-Introspection".
//   - A digit or an underscore still does not, so "x2go" stays "X2go" and
//     "code_introspection" stays "Code_introspection".
//   - The rest of each word keeps its own case, so "the ELF guard" stays
//     "The ELF Guard".
//   - The first rune is mapped with unicode.ToTitle, not unicode.ToUpper, so a
//     digraph title-cases rather than upper-casing: "ǆdict" stays "ǅdict".
//
// Only the ASCII apostrophe was ever affected. U+2019, the typographic
// apostrophe, is not a separator under strings.Title either, so "it’s" was
// never broken and is not changed here.
//
// golang.org/x/text/cases.Title is the replacement the deprecation notice
// names, and it is deliberately not used: it lower-cases the remainder of each
// word, so "the ELF guard" would become "The Elf Guard", and it would add a
// module dependency to two repos that have none for the sake of a display
// nicety.
//
// One further difference from strings.Title, which is a fix rather than a
// preservation: strings.Title maps every invalid UTF-8 byte to U+FFFD, so it
// silently corrupts input it cannot decode. Invalid bytes are copied through
// here untouched.
func TitleFirstRuneOfEachWord(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	atWordStart := true
	prevIsWordRune := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			// Not decodable. Copy the byte rather than writing U+FFFD over it.
			// strings.Title classifies U+FFFD as part of a word, so this byte
			// neither starts nor ends one.
			b.WriteByte(s[i])
			atWordStart = false
			prevIsWordRune = true
			i++
			continue
		}
		if atWordStart {
			b.WriteRune(unicode.ToTitle(r))
		} else {
			b.WriteRune(r)
		}

		if r == '\'' {
			// An apostrophe joins a word only when it is flanked by word
			// runes. Unflanked, it separates exactly as strings.Title has it.
			next, nextSize := utf8.DecodeRuneInString(s[i+size:])
			nextIsWordRune := nextSize > 0 && isWordRune(next)
			atWordStart = !(prevIsWordRune && nextIsWordRune)
		} else {
			atWordStart = !isWordRune(r)
		}

		prevIsWordRune = isWordRune(r)
		i += size
	}

	return b.String()
}

// isWordRune reports whether r is part of a word rather than a break between
// two.
//
// It is the negation of the unexported strings.isSeparator that strings.Title
// uses, and it deliberately still answers false for an apostrophe. Whether a
// particular apostrophe joins its neighbors is a question about position, not
// about the rune, so TitleFirstRuneOfEachWord decides that at the call site.
func isWordRune(r rune) bool {
	if r <= 0x7F {
		switch {
		case '0' <= r && r <= '9':
			return true
		case 'a' <= r && r <= 'z':
			return true
		case 'A' <= r && r <= 'Z':
			return true
		case r == '_':
			return true
		}
		return false
	}
	// Beyond ASCII, strings.Title can only treat spaces as separators, so
	// everything that is not a space is part of a word. Matching that is what
	// keeps every non-apostrophe input rendering as it always did.
	return !unicode.IsSpace(r)
}
