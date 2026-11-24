package boundedtext

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestValueIsBoundedRegardlessOfInput(t *testing.T) {
	for _, n := range []int{0, 1, 59, 60, 61, 5000, 100000} {
		got := Value(strings.Repeat("x", n))
		if len(got) > valueLimit {
			t.Errorf("Value of a %d-byte string is %d bytes, over the %d limit", n, len(got), valueLimit)
		}
	}
}

func TestErrorMessageIsBoundedRegardlessOfInput(t *testing.T) {
	// The real shape: a parser error carrying its own input. Asserting on a
	// hand-built long error would not test the case this package exists for.
	_, err := time.Parse(time.RFC3339, strings.Repeat("x", 5000))
	if err == nil {
		t.Fatal("expected time.Parse to fail")
	}
	if len(err.Error()) < 5000 {
		t.Fatalf("premise broken: time.Parse no longer embeds its input (%d bytes)", len(err.Error()))
	}
	if got := ErrorMessage(err); len(got) > errorLimit {
		t.Errorf("ErrorMessage is %d bytes, over the %d limit", len(got), errorLimit)
	}
}

// TestAShortMessageIsUntouched pins that the bound is a ceiling and not a
// reformat: every real diagnosis has to arrive whole.
func TestAShortMessageIsUntouched(t *testing.T) {
	_, err := time.Parse(time.RFC3339, "2026-13-01T00:00:00Z")
	if err == nil {
		t.Fatal("expected a bad month to fail")
	}
	got := ErrorMessage(err)
	if got != err.Error() {
		t.Errorf("a %d-byte message was altered: %q", len(err.Error()), got)
	}
	if !strings.Contains(got, "month out of range") {
		t.Errorf("the diagnosis did not survive: %q", got)
	}
}

// TestTheAtoiRangeErrorIsBounded covers the other embedding parser noteboard
// uses. strconv.Atoi's range error quotes the whole input back, which is the
// second of the three copies at the remind.lead site.
func TestTheAtoiRangeErrorIsBounded(t *testing.T) {
	_, err := strconv.Atoi(strings.Repeat("9", 5000))
	if err == nil {
		t.Fatal("expected an out-of-range Atoi to fail")
	}
	if len(err.Error()) < 5000 {
		t.Fatalf("premise broken: strconv.Atoi no longer embeds its input (%d bytes)", len(err.Error()))
	}
	if got := ErrorMessage(err); len(got) > errorLimit {
		t.Errorf("ErrorMessage is %d bytes, over the %d limit", len(got), errorLimit)
	}
}

// TestBothHalvesTogetherStayBounded is the case the 3x site needs and neither
// half's own test can make. Bounding one half of
//
//	fmt.Errorf("%q: %s", value, err)
//
// leaves the statement as long as it was, and the surviving truncate call reads
// exactly like a bound that holds.
func TestBothHalvesTogetherStayBounded(t *testing.T) {
	value := strings.Repeat("9", 5000)
	_, err := strconv.Atoi(value)
	if err == nil {
		t.Fatal("expected an out-of-range Atoi to fail")
	}

	unbounded := fmt.Sprintf("value %q: %s", value, err)
	valueOnly := fmt.Sprintf("value %q: %s", Value(value), err)
	both := fmt.Sprintf("value %q: %s", Value(value), ErrorMessage(err))

	if len(valueOnly) < len(unbounded)/2 {
		t.Fatalf("premise broken: bounding the value alone already shortened the line to %d from %d",
			len(valueOnly), len(unbounded))
	}
	if len(valueOnly) <= 1000 {
		t.Errorf("bounding the value alone gave %d bytes; this case exists because it does not", len(valueOnly))
	}
	if len(both) > valueLimit+errorLimit+len("value \"\": ") {
		t.Errorf("bounding both halves still gave %d bytes", len(both))
	}
}

func TestANilErrorRendersEmptyRatherThanAVerbPlaceholder(t *testing.T) {
	if got := ErrorMessage(nil); got != "" {
		t.Errorf("ErrorMessage(nil) = %q, want empty", got)
	}
}

// TestACutNeverSplitsARune pins the rune-boundary walk. Cutting on a byte
// boundary puts a U+FFFD in the response, which reads as data corruption
// upstream rather than as this package's own truncation.
func TestACutNeverSplitsARune(t *testing.T) {
	// Offsets chosen so the limit lands mid-rune for at least one of them:
	// "é" is 2 bytes and "€" is 3.
	for _, unit := range []string{"é", "€", "🙂"} {
		s := strings.Repeat(unit, 5000)
		got := Value(s)
		if !utf8.ValidString(got) {
			t.Errorf("Value of a %q run is not valid UTF-8: %q", unit, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("Value of a %q run contains U+FFFD: %q", unit, got)
		}
		if len(got) > valueLimit {
			t.Errorf("Value of a %q run is %d bytes, over the %d limit", unit, len(got), valueLimit)
		}
	}
}

// TestAMixedRunIsCutOnABoundary is the harder half: a string whose multi-byte
// rune straddles the limit exactly, rather than a uniform run where the walk
// may never have to move.
func TestAMixedRunIsCutOnABoundary(t *testing.T) {
	for pad := 0; pad < 8; pad++ {
		s := strings.Repeat("a", pad) + strings.Repeat("🙂", 100)
		got := Value(s)
		if !utf8.ValidString(got) {
			t.Errorf("pad %d: %q is not valid UTF-8", pad, got)
		}
		if len(got) > valueLimit {
			t.Errorf("pad %d: %d bytes, over the %d limit", pad, len(got), valueLimit)
		}
	}
}

func TestTheEllipsisMarksThatSomethingWasCut(t *testing.T) {
	if got := Value(strings.Repeat("x", 5000)); !strings.HasSuffix(got, "…") {
		t.Errorf("a cut value does not say it was cut: %q", got)
	}
	if got := Value("short"); strings.HasSuffix(got, "…") {
		t.Errorf("an uncut value claims it was cut: %q", got)
	}
	if got := ErrorMessage(errors.New(strings.Repeat("x", 5000))); !strings.HasSuffix(got, "…") {
		t.Errorf("a cut message does not say it was cut: %q", got)
	}
}
