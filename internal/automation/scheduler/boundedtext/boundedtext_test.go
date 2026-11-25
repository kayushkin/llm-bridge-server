package boundedtext

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAShortValuePassesThroughUnchanged(t *testing.T) {
	if got := Value("2026-01-01T00:00:00Z"); got != "2026-01-01T00:00:00Z" {
		t.Errorf("Value rewrote a value that already fit: %q", got)
	}
}

func TestALongValueIsBounded(t *testing.T) {
	got := Value(strings.Repeat("x", 5000))
	if len(got) > valueLimit {
		t.Errorf("Value returned %d bytes, over the %d-byte limit", len(got), valueLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Value cut %q without saying it had cut anything", got)
	}
}

// TestACutValueIsStillValidUTF8 pins the reason this does not reuse the
// byte-slicing truncate helpers elsewhere in the repo: a cut through a
// multi-byte rune puts U+FFFD in the log, which reads as corruption upstream
// rather than as this function's own truncation.
func TestACutValueIsStillValidUTF8(t *testing.T) {
	// "é" is two bytes, so a 60-byte budget lands mid-rune for some lengths.
	for n := 1; n <= 80; n++ {
		got := Value(strings.Repeat("é", n))
		if !utf8.ValidString(got) {
			t.Fatalf("Value(%d×'é') returned invalid UTF-8: %q", n, got)
		}
		if len(got) > valueLimit {
			t.Fatalf("Value(%d×'é') returned %d bytes, over the %d-byte limit", n, len(got), valueLimit)
		}
	}
}

func TestANilErrorRendersEmpty(t *testing.T) {
	if got := ErrorMessage(nil); got != "" {
		t.Errorf("ErrorMessage(nil) = %q, want empty", got)
	}
}

func TestAShortErrorKeepsItsDiagnosis(t *testing.T) {
	_, err := time.Parse(time.RFC3339, "2026-13-01T00:00:00Z")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if got := ErrorMessage(err); !strings.Contains(got, "month out of range") {
		t.Errorf("ErrorMessage dropped the diagnosis: %q", got)
	}
}

// TestEveryEmbeddingParserIsBounded is the measurement this package exists for,
// re-taken on every run rather than trusted from the doc comment. Each of these
// stdlib calls quotes its whole input back inside its own error.
func TestEveryEmbeddingParserIsBounded(t *testing.T) {
	oversized := strings.Repeat("x", 5000)
	cases := map[string]error{}

	_, err := time.Parse(time.RFC3339, oversized)
	cases["time.Parse"] = err
	_, err = time.ParseDuration(oversized)
	cases["time.ParseDuration"] = err
	_, err = strconv.Atoi(oversized)
	cases["strconv.Atoi"] = err
	_, err = strconv.ParseFloat(oversized, 64)
	cases["strconv.ParseFloat"] = err
	_, err = strconv.ParseBool(oversized)
	cases["strconv.ParseBool"] = err

	for name, parseErr := range cases {
		if parseErr == nil {
			t.Fatalf("%s accepted 5000 x's; the case list is wrong, not the code", name)
		}
		if len(parseErr.Error()) <= errorLimit {
			t.Errorf("%s rendered %d bytes raw — it no longer embeds its input, so this package's premise needs re-measuring",
				name, len(parseErr.Error()))
		}
		if got := ErrorMessage(parseErr); len(got) > errorLimit {
			t.Errorf("ErrorMessage(%s) returned %d bytes, over the %d-byte limit", name, len(got), errorLimit)
		}
	}
}

func TestAWrappedParserErrorIsBoundedToo(t *testing.T) {
	_, inner := time.Parse(time.RFC3339, strings.Repeat("x", 5000))
	wrapped := errors.New("event \"standup\" start: " + inner.Error())
	if got := ErrorMessage(wrapped); len(got) > errorLimit {
		t.Errorf("ErrorMessage returned %d bytes for a wrapped error, over the %d-byte limit", len(got), errorLimit)
	}
}
