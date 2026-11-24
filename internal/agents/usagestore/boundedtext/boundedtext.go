// Package boundedtext renders a string or an error at a length that does not
// depend on the input that produced it.
//
// It exists because usage-store has no auth and listens on every interface, so
// the length of a 400 body was the caller's to choose. Measured against the
// deployed binary on :8185 with a 5 000-byte occurred_at_str:
//
//	POST /api/usage/spend/topups   10 096 bytes   before
//	                                  ~110        after
//
// The amplification is time.Parse's, not this repo's. time.Parse quotes its
// whole input back inside its own error and does it twice:
//
//	parsing time "xxx…" as "2006-01-02": cannot parse "xxx…" as "2006"
//
// So err.Error() is already 2x the input before any statement here adds to it,
// and a statement that also prints the value with %q is 3x. Both halves have to
// be bounded: cutting the value alone leaves the wrapped parser error carrying
// two full copies, and the truncate call sitting there then reads exactly like a
// bound that holds.
//
// The error is worth carrying at its realistic length rather than dropping. For
// an ordinary bad date it says something the value alone does not — "month out
// of range", "day out of range", "extra text" — and every such diagnosis this
// repo's callers can produce fits inside errorLimit.
package boundedtext

import "unicode/utf8"

const (
	// valueLimit is how much of a value read from somewhere this process does
	// not control is worth showing. Enough to recognize the value; not enough
	// to bury the line it sits in.
	valueLimit = 60

	// errorLimit is how much of a parser's message is worth showing. Anything
	// past it is the parser quoting the input back.
	errorLimit = 80
)

// Value renders a string this process read from somewhere it does not control —
// a request body field, a query parameter, an environment variable — short
// enough to stay readable in a log line or a response body.
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

// Error wraps err so that its Error() is bounded, for a sink that takes an error
// rather than a string. The bound has to be applied where the error is built,
// not where it is rendered: a caller that wraps a bounded error with fmt.Errorf
// and %w gets a bounded whole, but one that wraps the raw error does not.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return boundedError{msg: ErrorMessage(err), wrapped: err}
}

type boundedError struct {
	msg     string
	wrapped error
}

func (e boundedError) Error() string { return e.msg }

// Unwrap keeps errors.Is and errors.As working on the original error. Only the
// rendered message is bounded; the error's identity is not touched.
func (e boundedError) Unwrap() error { return e.wrapped }

// cut trims s to at most limit bytes, on a rune boundary, marking that it did.
//
// Cutting on a byte boundary would split a multi-byte rune and put a U+FFFD in
// the response, which reads as data corruption upstream rather than as this
// function's own truncation.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// The ellipsis is 3 bytes in UTF-8; keep the whole result inside limit by
	// cutting to limit-3 first.
	end := limit - len("…")
	if end <= 0 {
		return "…"
	}
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
