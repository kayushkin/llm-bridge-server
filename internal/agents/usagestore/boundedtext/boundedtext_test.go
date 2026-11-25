package boundedtext

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestValueIsBoundedWhateverTheInput(t *testing.T) {
	for _, n := range []int{0, 1, 59, 60, 61, 5000, 100000} {
		got := Value(strings.Repeat("x", n))
		if len(got) > valueLimit {
			t.Errorf("Value(%d x's) = %d bytes, want <= %d", n, len(got), valueLimit)
		}
	}
}

func TestValueLeavesAShortStringExactlyAlone(t *testing.T) {
	// A bound that rewrites values it did not need to touch is a bound nobody
	// can read a log through.
	for _, s := range []string{"", "2026-13-01", strings.Repeat("x", valueLimit)} {
		if got := Value(s); got != s {
			t.Errorf("Value(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestErrorMessageIsBoundedForTheParserThisRepoActuallyUses(t *testing.T) {
	// Not a synthetic error: the real one, from the real call the repaired site
	// makes. If time.Parse ever stops embedding its input this test still holds
	// and the package becomes cheap rather than wrong.
	_, err := time.Parse("2006-01-02", strings.Repeat("x", 5000))
	if err == nil {
		t.Fatal("time.Parse accepted 5000 x's; the premise of this package is gone")
	}
	if len(err.Error()) < 5000 {
		t.Fatalf("time.Parse error is %d bytes for a 5000-byte input; it no longer embeds its input", len(err.Error()))
	}
	if got := ErrorMessage(err); len(got) > errorLimit {
		t.Errorf("ErrorMessage = %d bytes, want <= %d", len(got), errorLimit)
	}
}

func TestErrorMessageKeepsTheDiagnosisForAnOrdinaryBadDate(t *testing.T) {
	// The point of carrying the error at all. "month out of range" says
	// something the value alone does not, and it has to survive the bound.
	_, err := time.Parse("2006-01-02", "2026-13-01")
	if err == nil {
		t.Fatal("2026-13-01 parsed")
	}
	got := ErrorMessage(err)
	if !strings.Contains(got, "month out of range") {
		t.Errorf("ErrorMessage = %q, want it to still contain \"month out of range\"", got)
	}
}

func TestErrorMessageOnNilRendersEmptyRatherThanAVerbArtifact(t *testing.T) {
	if got := ErrorMessage(nil); got != "" {
		t.Errorf("ErrorMessage(nil) = %q, want \"\"", got)
	}
}

func TestErrorBoundsTheMessageAndKeepsTheChain(t *testing.T) {
	sentinel := errors.New("sentinel")
	long := fmt.Errorf("%w: %s", sentinel, strings.Repeat("x", 5000))
	bounded := Error(long)
	if len(bounded.Error()) > errorLimit {
		t.Errorf("Error(...).Error() = %d bytes, want <= %d", len(bounded.Error()), errorLimit)
	}
	if !errors.Is(bounded, sentinel) {
		t.Error("errors.Is lost the sentinel; bounding the message must not change the error's identity")
	}
}

func TestErrorStaysBoundedWhenACallerWrapsItWithPercentW(t *testing.T) {
	// How the repaired site uses it. Wrapping a bounded error re-renders it, so
	// the whole has to stay bounded by the prefix plus the bound, not by luck.
	_, err := time.Parse("2006-01-02", strings.Repeat("x", 5000))
	wrapped := fmt.Errorf("occurred_at_str: %w", Error(err))
	if len(wrapped.Error()) > len("occurred_at_str: ")+errorLimit {
		t.Errorf("wrapped = %d bytes, want <= %d", len(wrapped.Error()), len("occurred_at_str: ")+errorLimit)
	}
}

func TestErrorOnNilIsNil(t *testing.T) {
	if Error(nil) != nil {
		t.Error("Error(nil) is not nil; a caller would have to guard")
	}
}

func TestCutNeverSplitsARune(t *testing.T) {
	// A split rune renders U+FFFD, which reads as corruption upstream rather
	// than as this function's own truncation. Every offset around the limit has
	// to land on a boundary, so the multi-byte width is swept, not sampled.
	for _, r := range []string{"é", "€", "😀"} {
		for n := 1; n <= 120; n++ {
			got := cut(strings.Repeat(r, n), valueLimit)
			if !utf8.ValidString(got) {
				t.Fatalf("cut(%d x %q) = %q, which is not valid UTF-8", n, r, got)
			}
			if len(got) > valueLimit {
				t.Fatalf("cut(%d x %q) = %d bytes, want <= %d", n, r, len(got), valueLimit)
			}
		}
	}
}

func TestCutMarksThatItCut(t *testing.T) {
	got := cut(strings.Repeat("x", 5000), valueLimit)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("cut = %q, want it to end in an ellipsis so a reader knows it is not the whole value", got)
	}
}
