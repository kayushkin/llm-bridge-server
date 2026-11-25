// Package boundedtext renders a string or an error at a length that does not
// depend on the input that produced it.
//
// It exists because of one measured surprise. Six of the seven stdlib parsers
// this repo uses quote their whole input back inside their own error message,
// and time.Parse does it twice:
//
//	time.Parse            10 073 bytes   embeds the input, twice
//	url.Parse              5 064         embeds the input
//	strconv.ParseFloat     5 046         embeds the input
//	strconv.ParseBool      5 045         embeds the input
//	strconv.Atoi           5 040         embeds the input
//	time.ParseDuration     5 025         embeds the input
//	time.LoadLocation         18         does not
//	json.Unmarshal            52 / 55    does not
//
// (measured against strings.Repeat("x", 5000)). encoding/json — the one you
// would guess is worst, because it handles the biggest payloads — does not.
// So the intuition points the wrong way, and a line like
//
//	log.Printf("... %q ... (%v)", truncate(value, 60), err)
//
// is unbounded no matter how carefully the value is cut. The truncate reads as
// the bound and is decoration. Both halves have to be bounded, and on this box
// bounding only one of them still rendered 15 KB for a 5 KB field.
//
// The error is worth carrying at its realistic length rather than dropping:
// for an ordinary bad timestamp it says something the value alone does not,
// such as "month out of range".
package boundedtext

import "unicode/utf8"

const (
	// valueLimit is how much of a value read from somewhere this process does
	// not control is worth showing. Enough to recognize the value; not enough
	// to bury the line it sits in.
	valueLimit = 60

	// errorLimit is how much of a parser's message is worth showing. Every real
	// diagnosis this repo's parsers emit — "month out of range", "extra text",
	// "invalid syntax" — fits well inside it, and anything past it is the
	// parser quoting the input back.
	errorLimit = 80
)

// Value renders a string this process read from somewhere it does not control —
// a remote field, a query parameter, an environment variable — short enough to
// stay readable in a log line or an error.
func Value(s string) string { return cut(s, valueLimit) }

// ErrorMessage renders an error's message bounded, for the case where the error
// came from a parser and so may be carrying its whole input.
//
// A nil error renders empty rather than "%!v(nil)", so a caller need not guard.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return cut(err.Error(), errorLimit)
}

// cut trims s to at most limit bytes, on a rune boundary, marking that it did.
//
// Cutting on a byte boundary would split a multi-byte rune and put a U+FFFD in
// the log, which reads as data corruption upstream rather than as this
// function's own truncation.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// One byte of the budget goes to the ellipsis, which is 3 bytes in UTF-8;
	// keep the whole result inside limit by cutting to limit-3 first.
	end := limit - len("…")
	if end <= 0 {
		return "…"
	}
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
