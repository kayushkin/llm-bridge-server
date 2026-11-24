// Package boundedtext renders a string or an error at a length that does not
// depend on the input that produced it.
//
// It exists because noteboard hands a caller's own input back inside its 400s,
// and this service has no auth at all: ~/CLAUDE.md records it listening on
// *:8191 with no firewall rule, so anything that can route to this host can
// choose the length of a response body.
//
// Measured against the deployed binary on :8191 before the repair, with a 5 KB
// query parameter or field:
//
//	POST /api/items  schedule.remind.lead     15 183 bytes
//	GET  .../occurrences?from=               10 116
//	GET  .../occurrences?to=                 10 114
//	GET  /api/items/<id>/<action>             5 034
//	POST /api/items  schedule.remind.channels  5 114
//	POST /api/items  schedule.tzid             5 076
//	POST /api/items  schedule.mode             5 072
//
// Three sizes, and the reason for each is worth knowing before repairing one:
//
//   - 1x is the statement quoting the value with %q and nothing more.
//   - 2x is a stdlib parser quoting its whole input back inside its own error.
//     time.Parse does it twice, so a bare err.Error() is already 2x.
//   - 3x is both at once: the statement prints the value AND wraps the parser's
//     error, which is carrying its own copy. That is remind.lead above.
//
// So bounding one half is not a partial fix, it is an invisible one. At the 3x
// site a 60-byte cut on the value alone would still have rendered 10 KB, and
// the truncate call sitting there reads exactly like a bound that holds.
//
// The intuition also points the wrong way about WHICH parser is dangerous.
// Measured here against strings.Repeat("x", 5000):
//
//	time.Parse            10 073 bytes   embeds the input, twice
//	strconv.Atoi           5 040         embeds the input
//	time.LoadLocation         18         does not
//
// and noteboard's own parseRRule does not either — a 5 KB rrule answers 70
// bytes and a 5 KB remind.nag answers 75. Two of the six schedule fields that
// look identical in the source are not defects, which is why every site this
// package is applied to was measured rather than pattern-matched.
//
// The error is worth carrying at its realistic length rather than dropping: for
// an ordinary bad timestamp it says something the value alone does not, such as
// "month out of range". A repair that blanks the message satisfies every
// ceiling and destroys the only thing the field is for.
package boundedtext

import "unicode/utf8"

const (
	// valueLimit is how much of a value read from somewhere this process does
	// not control is worth showing. Enough to recognize the value; not enough
	// to bury the line it sits in.
	valueLimit = 60

	// errorLimit is how much of a parser's message is worth showing. Every real
	// diagnosis noteboard's parsers emit — "month out of range", "value out of
	// range", "extra text", "invalid syntax" — fits well inside it, and
	// anything past it is the parser quoting the input back.
	errorLimit = 80
)

// Value renders a string this process read from somewhere it does not control —
// a request field, a query parameter, a URL path segment — short enough to stay
// readable in the line it sits in.
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
// the response, which reads as data corruption upstream rather than as this
// function's own truncation.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// The ellipsis is 3 bytes in UTF-8 and has to fit inside the budget too, so
	// cut to limit-3 first and let the rune walk move left from there.
	end := limit - len("…")
	if end <= 0 {
		return "…"
	}
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
