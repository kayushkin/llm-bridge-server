package logging

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// capture returns a Logger whose entries land in the returned slice pointer
// instead of NATS, through the recordingPublisher that logging_test.go uses.
func capture() (*Logger, *[]LogEntry) {
	l, rec := loggerWithRecorder()
	return l, &rec.entries
}

func snippetOf(t *testing.T, e LogEntry) string {
	t.Helper()
	v, ok := e.Metadata["output_snippet"]
	if !ok {
		t.Fatalf("entry has no output_snippet: %+v", e.Metadata)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("output_snippet is %T, want string", v)
	}
	return s
}

// A job's combined stdout+stderr reaches the log bus through LogJobFinish's
// output_snippet, so a snippet cut mid-rune is stored broken. json.Marshal
// swaps the partial rune for U+FFFD and reports no error, so nothing along
// that path can report the damage.
//
// The rune is slid across the cut one byte at a time rather than placed once:
// a single hand-picked offset lands off the boundary and passes against the
// unfixed code, which is how this shape survived everywhere else in the fleet.
func TestJobOutputSnippetNeverPublishesASplitRune(t *testing.T) {
	const clef = "\U0001D11E" // 4 bytes, so three of the four offsets straddle
	if len(clef) != 4 {
		t.Fatalf("fixture rune is %d bytes, want 4", len(clef))
	}

	// budget-3 .. budget-1 straddle the cut; budget-4 and budget are aligned
	// and must hold against the unfixed code too, which is what makes this a
	// test for the straddle rather than a test for non-ASCII input.
	for _, start := range []int{
		outputSnippetBudget - 4,
		outputSnippetBudget - 3,
		outputSnippetBudget - 2,
		outputSnippetBudget - 1,
		outputSnippetBudget,
	} {
		straddles := start > outputSnippetBudget-4 && start < outputSnippetBudget
		name := "aligned"
		if straddles {
			name = "straddling"
		}
		t.Run(name+"-at-"+itoa(start), func(t *testing.T) {
			l, got := capture()
			// Padding past the budget guarantees the truncation branch runs.
			output := strings.Repeat("a", start) + clef + strings.Repeat("b", 200)

			l.LogJobFinish(1, "job", "* * * * *", 2, "success", output, time.Second)

			if len(*got) != 1 {
				t.Fatalf("published %d entries, want 1", len(*got))
			}
			snippet := snippetOf(t, (*got)[0])

			if !utf8.ValidString(snippet) {
				t.Errorf("snippet is not valid UTF-8 — a rune was cut in half\n  rune starts at byte %d, budget is %d\n  tail bytes: % x",
					start, outputSnippetBudget, tail(snippet, 12))
			}

			// Validity alone is not falsifiable: a helper that returns ""
			// is valid UTF-8 and within budget. Pin how much was dropped —
			// a boundary walk-back can never lose more than the width of
			// the straddling rune.
			body := strings.TrimSuffix(snippet, truncationMarker)
			if len(body) > outputSnippetBudget {
				t.Errorf("snippet body is %d bytes, over the %d budget", len(body), outputSnippetBudget)
			}
			if min := outputSnippetBudget - (len(clef) - 1); len(body) < min {
				t.Errorf("snippet body is %d bytes, want at least %d — dropped more than one rune's worth", len(body), min)
			}
		})
	}
}

// Output that fits is published whole, marker and all absent. Without this a
// helper that truncates everything unconditionally still passes the test above.
func TestJobOutputShorterThanTheBudgetIsPublishedWhole(t *testing.T) {
	l, got := capture()
	output := "héllo — a short line with multibyte runes in it"

	l.LogJobFinish(1, "job", "* * * * *", 2, "success", output, time.Second)

	snippet := snippetOf(t, (*got)[0])
	if snippet != output {
		t.Errorf("snippet = %q, want the output unchanged %q", snippet, output)
	}
}

// An all-ASCII output at exactly the budget must not gain a marker, which pins
// the guard's comparison rather than the cut it protects.
func TestJobOutputExactlyAtTheBudgetIsNotTruncated(t *testing.T) {
	l, got := capture()
	output := strings.Repeat("a", outputSnippetBudget)

	l.LogJobFinish(1, "job", "* * * * *", 2, "success", output, time.Second)

	snippet := snippetOf(t, (*got)[0])
	if snippet != output {
		t.Errorf("snippet was altered at exactly the budget: len=%d, marker present=%v",
			len(snippet), strings.Contains(snippet, truncationMarker))
	}
}

func tail(s string, n int) []byte {
	b := []byte(s)
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
