package aiauth

import "unicode/utf8"

// Cutting a Go string at a fixed byte offset splits whatever rune straddles
// that offset, and the result is not valid UTF-8. Nothing reports it — the
// value simply arrives wrong, as a replacement glyph.
//
// These two carry the semantics settled for the fleet unicode byte-cut sweep.
// The canonical pair is tool-store's schema.TruncateAtRuneBoundary and
// schema.SuffixAtRuneBoundary; aiauth keeps a package-local copy rather than
// importing tool-store, because a 20-line helper is not worth a module edge
// from an auth CLI to the tool registry. The behaviour is the canonical one so
// the copies cannot drift apart silently. They stay unexported: MaskKey is the
// only consumer and it lives in this package, so there is nothing to widen the
// public API for.
//
// The walk costs at most three byte comparisons and allocates nothing, which is
// why it is preferred here over converting to []rune.

// truncateAtRuneBoundary returns the longest prefix of s that is no longer than
// maxBytes and does not end part-way through a multi-byte UTF-8 sequence.
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

// suffixAtRuneBoundary returns the longest suffix of s that is no longer than
// maxBytes and does not begin part-way through a multi-byte UTF-8 sequence.
//
// This is the other half of the defect and it walks the opposite way. A prefix
// cut leaves the leading bytes of a straddling rune at the end of the result;
// a suffix cut leaves its trailing continuation bytes at the start. Walking
// back would lengthen the suffix past its budget, so the cut moves forward and
// drops the partial rune.
func suffixAtRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is the first byte of the suffix. While it is a continuation
	// byte, a rune straddles the cut, so move the cut later.
	cut := len(s) - maxBytes
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
