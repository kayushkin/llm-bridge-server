package memorystore

import (
	"fmt"
	"strings"
	"testing"
	"time"

)

// Boundary VALUES, as opposed to boundary mechanisms.
//
// The suite next door pins the rune-boundary walk-back: that it runs, that it
// stops in the right place, that no call site reverts to a plain byte cut.
// Every one of those tests is written in terms of the budget it is handed, so
// none of them can fail for a wrong value of that budget. Sabotage-scored by
// scripts/sabotage-truncation.py: 6 of 6 mechanism mutations caught, 3 of 18
// value moves caught.
//
// So these are second tests, not replacements. Each one spells the number out
// as a literal, because a test that names a boundary through the constant it
// would have to pin moves with it.
//
// Two rules from the sweep this file belongs to:
//
//   - Straddle, do not merely exceed. A fixture far past a cut pins the cut to
//     a band, not to a value. Every boundary here is measured at N and at N+1.
//   - Mark a character, do not assert a length. A cut that keeps the wrong end
//     of a string returns exactly the right number of bytes. '~' is the marker
//     throughout: it appears nowhere in the filler, in the compaction header,
//     in the ellipsis, or in the truncation hint.

// ---------------------------------------------------------------------------
// util.go — truncateAtRuneBoundary's own guard
// ---------------------------------------------------------------------------

// TestTruncateAtRuneBoundaryKeepsASingleByteBudget pins the guard's threshold at
// zero rather than at one.
//
// The suite next door sweeps maxBytes from 1 upward, but only over multi-byte
// runes, and for those a 1-byte budget correctly yields "" — the walk-back eats
// the straddling rune. So `maxBytes <= 0` could widen to `maxBytes <= 1` with
// every existing case still green. It takes a one-byte rune to tell the two
// apart, and the suite had none at that budget.
func TestTruncateAtRuneBoundaryKeepsASingleByteBudget(t *testing.T) {
	if got := truncateAtRuneBoundary("hello", 1); got != "h" {
		t.Errorf("maxBytes=1 over single-byte runes: got %q, want %q — the guard "+
			"rejects budgets it should honor", got, "h")
	}
	if got := truncateAtRuneBoundary("hello", 2); got != "he" {
		t.Errorf("maxBytes=2 over single-byte runes: got %q, want %q", got, "he")
	}
}

// ---------------------------------------------------------------------------
// compaction.go — the 2000-byte merge budget and the selection thresholds
// ---------------------------------------------------------------------------

// compactionHeader is the prefix Compact writes ahead of the merged content.
// Spelled out rather than derived, so a change to the format is a test failure
// and not a silently-retargeted assertion.
func compactionHeader(members int) string {
	return fmt.Sprintf("[Compacted from %d memories]\n", members)
}

// saveCompactable stores one memory per element of contents, all under the same
// tag, all old enough and cheap enough to be compaction candidates.
func saveCompactable(t *testing.T, tag string, contents []string, importance float64) *Store {
	t.Helper()
	store := openTestStore(t)
	t.Cleanup(func() { store.Close() })

	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	for i, content := range contents {
		err := store.Save(Memory{
			ID:          fmt.Sprintf("%s-mem-%d", tag, i),
			Content:     content,
			Tags:        []string{tag},
			Importance:  importance,
			AccessCount: 1,
			Source:      "agent",
			CreatedAt:   oldTime,
		})
		if err != nil {
			t.Fatalf("failed to save %s-mem-%d: %v", tag, i, err)
		}
	}
	return store
}

// TestCompactionKeepsExactlyTwoThousandBytes pins the merge budget to 2000 in
// both directions.
//
// Nothing in the existing suite looks at the length of the compacted content —
// it asserts only that the content is valid UTF-8, which is true of a prefix of
// any length. The budget could move to 1999 or 2001 with the suite green.
//
// Both members carry the marker at the same two offsets, so the assertion holds
// whichever order the unordered candidate query returns them in.
func TestCompactionKeepsExactlyTwoThousandBytes(t *testing.T) {
	member := func(filler byte) string {
		b := make([]byte, 2500)
		for i := range b {
			b[i] = filler
		}
		b[1999] = '~' // the last byte a 2000-byte budget may keep
		b[2000] = 'Q' // the first byte it may not
		return string(b)
	}

	store := saveCompactable(t, "budget", []string{member('a'), member('b')}, 0.3)
	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 compaction group, got %d", len(results))
	}
	compacted, err := store.Get(results[0].NewID)
	if err != nil {
		t.Fatalf("failed to get compacted memory: %v", err)
	}

	kept := strings.TrimPrefix(compacted.Content, compactionHeader(2))
	if kept == compacted.Content {
		t.Fatalf("compacted content does not start with %q", compactionHeader(2))
	}
	if !strings.HasSuffix(kept, "...") {
		t.Fatalf("merged content of 5005 bytes was not truncated at all")
	}
	kept = strings.TrimSuffix(kept, "...")

	if !strings.HasSuffix(kept, "~") {
		t.Errorf("the 2000-byte budget kept %d bytes ending %q — the marker at "+
			"offset 1999 is the last byte it may keep", len(kept), tailBytes(kept, 4))
	}
	if strings.ContainsRune(kept, 'Q') {
		t.Errorf("the 2000-byte budget kept the byte at offset 2000, which is one " +
			"byte past its budget")
	}
}

// TestCompactionCutFiresExactlyAboveItsBudget straddles the guard that decides
// whether to cut at all. `len(combined) > 2000` and the 2000 handed to the cut
// are two separate literals that have to agree, and only the second of them is
// touched by the test above.
//
// Only the LENGTH of the merged string matters here, and the length is the same
// whichever order the candidate query returns the two members in — so this pins
// the guard without depending on an ordering the query does not promise.
func TestCompactionCutFiresExactlyAboveItsBudget(t *testing.T) {
	// combined = first + "\n---\n" + second, and the separator is 5 bytes.
	for _, tc := range []struct {
		name       string
		lens       [2]int
		wantCut    bool
		wantMerged int
	}{
		{"merged content is exactly the 2000-byte budget", [2]int{997, 998}, false, 2000},
		{"merged content is one byte over the budget", [2]int{998, 998}, true, 2001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := strings.Repeat("a", tc.lens[0])
			second := strings.Repeat("b", tc.lens[1])
			if got := len(first) + len("\n---\n") + len(second); got != tc.wantMerged {
				t.Fatalf("fixture builds %d merged bytes, want %d", got, tc.wantMerged)
			}

			store := saveCompactable(t, "guard", []string{first, second}, 0.3)
			results, err := store.Compact(7*24*time.Hour, 3)
			if err != nil {
				t.Fatalf("Compact failed: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("expected 1 compaction group, got %d", len(results))
			}
			compacted, err := store.Get(results[0].NewID)
			if err != nil {
				t.Fatalf("failed to get compacted memory: %v", err)
			}
			kept := strings.TrimPrefix(compacted.Content, compactionHeader(2))

			if gotCut := strings.HasSuffix(kept, "..."); gotCut != tc.wantCut {
				t.Errorf("%d merged bytes against a 2000-byte budget: truncated=%v, want %v",
					tc.wantMerged, gotCut, tc.wantCut)
			}
			wantLen := tc.wantMerged
			if tc.wantCut {
				wantLen = 2000 + len("...")
			}
			if len(kept) != wantLen {
				t.Errorf("%d merged bytes against a 2000-byte budget: stored %d bytes, want %d",
					tc.wantMerged, len(kept), wantLen)
			}
		})
	}
}

// TestCompactionNeedsExactlyTwoMembers straddles `len(members) < 2`. The
// existing fixture uses three, which is over the line rather than on it.
func TestCompactionNeedsExactlyTwoMembers(t *testing.T) {
	t.Run("two members compact", func(t *testing.T) {
		store := saveCompactable(t, "pair", []string{"alpha", "beta"}, 0.3)
		results, err := store.Compact(7*24*time.Hour, 3)
		if err != nil {
			t.Fatalf("Compact failed: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("a group of exactly 2 must compact: got %d groups", len(results))
		}
		if results[0].Count != 2 {
			t.Errorf("compacted group counts %d members, want 2", results[0].Count)
		}
	})
	t.Run("one member does not compact", func(t *testing.T) {
		store := saveCompactable(t, "single", []string{"alpha"}, 0.3)
		results, err := store.Compact(7*24*time.Hour, 3)
		if err != nil {
			t.Fatalf("Compact failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("a group of 1 must not compact: got %d groups", len(results))
		}
	})
}

// TestCompactionImportanceWindowIsPinnedToItsValues straddles both ends of
// `importance < 0.7 AND importance > 0`.
//
// The lower end is pinned at 0.1 rather than at 0: Save rewrites an importance
// of exactly 0 to 0.5, so a memory on the wrong side of that bound cannot be
// stored through the public API and the bound is unobservable from here. What
// this pins is that 0.1 is INSIDE the window — which is what fails if the floor
// drifts up to 0.1.
func TestCompactionImportanceWindowIsPinnedToItsValues(t *testing.T) {
	for _, tc := range []struct {
		name        string
		importance  float64
		wantCompact bool
	}{
		{"0.1 is above the floor", 0.1, true},
		{"0.65 is below the 0.7 ceiling", 0.65, true},
		{"0.7 is the ceiling itself and is excluded", 0.7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := saveCompactable(t, "window", []string{"alpha", "beta"}, tc.importance)
			results, err := store.Compact(7*24*time.Hour, 3)
			if err != nil {
				t.Fatalf("Compact failed: %v", err)
			}
			got := len(results) == 1
			if got != tc.wantCompact {
				t.Errorf("importance %v: compacted=%v, want %v (the window is "+
					"0 < importance < 0.7)", tc.importance, got, tc.wantCompact)
			}
		})
	}
}

// TestCompactedMemoryTakesImportanceOneHalf pins the importance the merged
// memory is written with. Nothing else reads it back.
//
// The row is read straight out of the database rather than through Get, and
// that is not a shortcut. Get calls updateAccess, which multiplies importance
// by 1.01 before answering, so Get(newID).Importance is 0.505 — the product of
// two constants. An assertion on it would fail for a drift in either and could
// not say which.
func TestCompactedMemoryTakesImportanceOneHalf(t *testing.T) {
	store := saveCompactable(t, "newimportance", []string{"alpha", "beta"}, 0.3)
	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 compaction group, got %d", len(results))
	}

	var importance float64
	err = store.db.QueryRow("SELECT importance FROM memories WHERE id = $1",
		results[0].NewID).Scan(&importance)
	if err != nil {
		t.Fatalf("failed to read the compacted row: %v", err)
	}
	if importance != 0.5 {
		t.Errorf("compacted memory was stored with importance %v, want 0.5", importance)
	}
}

// ---------------------------------------------------------------------------
// builder.go — truncateMemoryToPreview's preview budget and its walk-back
// ---------------------------------------------------------------------------

// previewOf splits a truncated memory back into the preview and the hint.
// Asserting on the preview alone is what lets these tests spell an exact
// expected string: the hint carries the memory's id and two token counts.
func previewOf(t *testing.T, content string) string {
	t.Helper()
	preview, _, found := strings.Cut(content, "\n\n[... truncated")
	if !found {
		t.Fatalf("content was not truncated, so this test is not exercising the cut")
	}
	return preview
}

// TestPreviewCutIsPinnedToItsBudget pins the preview cut to exactly
// previewChars bytes when no word break is available past halfway.
//
// The fallback path — no space, no newline — is the one the rune-boundary bug
// lived in, and the existing test drives it with text made only of four-byte
// runes and asserts only that the result is valid UTF-8. Any budget produces a
// valid prefix, so the budget itself was free.
func TestPreviewCutIsPinnedToItsBudget(t *testing.T) {
	// 250 bytes: past 2*100, so the >50% skip rule does not fire. No space and
	// no newline anywhere, so the walk-back finds nothing and the plain cut is
	// what survives.
	content := strings.Repeat("a", 99) + "~" + strings.Repeat("Q", 150)
	if len(content) != 250 {
		t.Fatalf("fixture is %d bytes, want 250", len(content))
	}

	out := truncateMemoryToPreview(Memory{ID: "mem-1", Content: content, Tokens: 84}, 100)
	preview := previewOf(t, out.Content)

	if want := strings.Repeat("a", 99) + "~"; preview != want {
		t.Errorf("a 100-byte preview kept %d bytes ending %q — offset 99 is the "+
			"last byte it may keep and offset 100 the first it may not",
			len(preview), tailBytes(preview, 4))
	}
}

// TestPreviewSkipRuleStraddlesHalfTheContent pins `previewChars*2 >=
// len(content)`: the rule that truncating is not worth it when the preview
// would be more than half of what it replaces.
//
// Nothing in the existing suite sits near this line — its fixture is 2400 bytes
// against a 100-byte preview, twelve times past it.
func TestPreviewSkipRuleStraddlesHalfTheContent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		contentLen    int
		wantTruncated bool
	}{
		{"exactly twice the preview is left whole", 200, false},
		{"one byte over twice the preview is truncated", 201, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Repeat("a", tc.contentLen)
			out := truncateMemoryToPreview(Memory{ID: "mem-1", Content: content, Tokens: 67}, 100)
			gotTruncated := out.Content != content
			if gotTruncated != tc.wantTruncated {
				t.Errorf("%d bytes against a 100-byte preview: truncated=%v, want %v "+
					"(the rule is preview*2 >= len(content) leaves it whole)",
					tc.contentLen, gotTruncated, tc.wantTruncated)
			}
		})
	}
}

// TestPreviewLeavesContentOneByteOverTheBudgetWhole pins the upper half of the
// straddle on `len(content) <= previewChars`, and it is the executable form of
// why the sabotage list carries that row as a control rather than a hole.
//
// That guard is a fast path with nothing under it: for every previewChars >= 1
// the >50% rule four lines below returns the memory unchanged for exactly the
// same inputs, so the guard can move by one byte with no observable effect.
// Swept 2026-08-15 over previewChars 0..60 at the only length where the two can
// disagree — one input differs, previewChars=0, and the one call site defaults
// a previewChars of 0 to 300 before it gets here.
//
// This test is what makes that claim falsifiable. If the >50% rule is ever
// narrowed so it stops dominating, this goes red and the control row in
// scripts/sabotage-truncation.py has to become a real case.
func TestPreviewLeavesContentOneByteOverTheBudgetWhole(t *testing.T) {
	content := strings.Repeat("a", 101)
	out := truncateMemoryToPreview(Memory{ID: "mem-1", Content: content, Tokens: 34}, 100)
	if out.Content != content {
		t.Errorf("101 bytes against a 100-byte preview was truncated to %d bytes — "+
			"one byte over the budget is still under the >50%% rule and must be "+
			"left whole", len(out.Content))
	}
}

// TestBuildContextPreviewDefaultsToThreeHundredBytes pins the preview budget
// that actually ships.
//
// Every other test in this file hands truncateMemoryToPreview a budget, so none
// of them can fail for a wrong default. The default is the number production
// uses: BuildContext is the function's only caller and it substitutes 300 for
// any TruncatePreview of zero or less.
func TestBuildContextPreviewDefaultsToThreeHundredBytes(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	// No space, no newline, so the walk-back finds no break and the plain cut
	// stands. The marker sits on the last byte a 300-byte preview may keep.
	content := strings.Repeat("a", 299) + "~" + strings.Repeat("Q", 900)
	if err := store.Save(Memory{
		ID:      "big",
		Content: content,
		Tags:    []string{"defaults"},
		Tokens:  400,
	}); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	got, _, err := store.BuildContext(BuildContextRequest{
		Tags:              []string{"defaults"},
		IncludeAlwaysLoad: true,
		TruncateThreshold: 100, // below the memory's 400 tokens, so it truncates
		// TruncatePreview deliberately left at zero: the default is the subject.
	})
	if err != nil {
		t.Fatalf("BuildContext failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 memory back, got %d", len(got))
	}
	if want := strings.Repeat("a", 299) + "~"; previewOf(t, got[0].Content) != want {
		preview := previewOf(t, got[0].Content)
		t.Errorf("the default preview kept %d bytes ending %q, want 300 ending %q",
			len(preview), tailBytes(preview, 4), tailBytes(want, 4))
	}
}

// saveOneBigMemory stores a single memory with an explicit token count and
// content long enough that a 300-byte preview is under the >50% rule.
func saveOneBigMemory(t *testing.T, tag string, content string, tokens int) *Store {
	t.Helper()
	store := openTestStore(t)
	t.Cleanup(func() { store.Close() })
	if err := store.Save(Memory{ID: "big", Content: content, Tags: []string{tag}, Tokens: tokens}); err != nil {
		t.Fatalf("failed to save: %v", err)
	}
	return store
}

// TestBuildContextHonorsAOneBytePreview pins the lower half of the straddle on
// `if truncatePreview <= 0`. The test above pins what happens when the caller
// asks for nothing; this pins that a caller asking for one byte gets one byte
// and not the 300-byte default.
func TestBuildContextHonorsAOneBytePreview(t *testing.T) {
	store := saveOneBigMemory(t, "onebyte", "~"+strings.Repeat("Q", 999), 400)
	got, _, err := store.BuildContext(BuildContextRequest{
		Tags:              []string{"onebyte"},
		IncludeAlwaysLoad: true,
		TruncateThreshold: 100,
		TruncatePreview:   1,
	})
	if err != nil {
		t.Fatalf("BuildContext failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 memory back, got %d", len(got))
	}
	if preview := previewOf(t, got[0].Content); preview != "~" {
		t.Errorf("a requested preview of 1 byte kept %d bytes (%q) — a budget of 1 "+
			"is a real request, not an unset one", len(preview), preview)
	}
}

// TestTruncateThresholdStraddlesItsOwnValue pins `m.Tokens > truncateThreshold`
// as strictly greater.
//
// A memory sitting exactly on the threshold is the only input that separates
// `>` from `>=`, and no fixture in this package sat there — every one of them is
// hundreds of tokens past it.
func TestTruncateThresholdStraddlesItsOwnValue(t *testing.T) {
	content := strings.Repeat("a", 299) + "~" + strings.Repeat("Q", 900)
	for _, tc := range []struct {
		name          string
		tokens        int
		wantTruncated bool
	}{
		{"exactly on the 100-token threshold is left whole", 100, false},
		{"one token over the threshold is truncated", 101, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := saveOneBigMemory(t, "threshold", content, tc.tokens)
			got, _, err := store.BuildContext(BuildContextRequest{
				Tags:              []string{"threshold"},
				IncludeAlwaysLoad: true,
				TruncateThreshold: 100,
			})
			if err != nil {
				t.Fatalf("BuildContext failed: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 memory back, got %d", len(got))
			}
			gotTruncated := got[0].Content != content
			if gotTruncated != tc.wantTruncated {
				t.Errorf("%d tokens against a threshold of 100: truncated=%v, want %v",
					tc.tokens, gotTruncated, tc.wantTruncated)
			}
		})
	}
}

// TestPreviewWordBreakStraddlesHalfway pins the halfway mark the word and line
// walk-backs are measured against: `> previewChars/2`, which for a 100-byte
// preview is offset 50 exclusive.
//
// The existing fixture is made of four-byte runes with no ASCII whitespace in
// it at all, deliberately — it exists to drive the fallback. So neither of these
// two branches was reached by any test, and neither the halfway mark nor the
// choice between them was pinned.
func TestPreviewWordBreakStraddlesHalfway(t *testing.T) {
	// Every fixture is 250 bytes with the marker at offset 99, so the untouched
	// preview is always the same: whatever changes is where the break lands.
	build := func(breakAt int, breakByte string) string {
		if breakAt >= 99 {
			t.Fatalf("break at %d would overwrite the marker", breakAt)
		}
		return strings.Repeat("a", breakAt) + breakByte +
			strings.Repeat("a", 98-breakAt) + "~" + strings.Repeat("Q", 150)
	}

	for _, tc := range []struct {
		name        string
		content     string
		wantPreview string
	}{
		{
			// 50 is not > 50, so this break is rejected and the plain cut stands.
			"a newline exactly on halfway is rejected",
			build(50, "\n"),
			strings.Repeat("a", 50) + "\n" + strings.Repeat("a", 48) + "~",
		},
		{
			"a newline one byte past halfway is taken",
			build(51, "\n"),
			strings.Repeat("a", 51),
		},
		{
			// 40 is past a third of the preview but short of halfway.
			"a space short of halfway is rejected",
			build(40, " "),
			strings.Repeat("a", 40) + " " + strings.Repeat("a", 58) + "~",
		},
		{
			"a space one byte past halfway is taken",
			build(51, " "),
			strings.Repeat("a", 51),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.content) != 250 {
				t.Fatalf("fixture is %d bytes, want 250", len(tc.content))
			}
			out := truncateMemoryToPreview(Memory{ID: "mem-1", Content: tc.content, Tokens: 84}, 100)
			if preview := previewOf(t, out.Content); preview != tc.wantPreview {
				t.Errorf("preview is %d bytes ending %q, want %d bytes ending %q",
					len(preview), tailBytes(preview, 4),
					len(tc.wantPreview), tailBytes(tc.wantPreview, 4))
			}
		})
	}
}

// TestTruncatedPreviewSpellsItsTokenCounts pins the two token estimates in the
// truncation hint, and the memory's own recomputed token count.
//
// Both are `(n + 2) / 3`, and the divisor was reachable by no assertion: the
// hint text was never read back and Tokens was never compared to a number.
// The expected values below are written as literals for the same reason — an
// expectation computed with the same expression cannot fail when it drifts.
func TestTruncatedPreviewSpellsItsTokenCounts(t *testing.T) {
	content := strings.Repeat("a", 99) + "~" + strings.Repeat("Q", 150)
	out := truncateMemoryToPreview(Memory{ID: "mem-1", Content: content, Tokens: 84}, 100)

	// 84 declared tokens, less (100+2)/3 = 34 for the 100 bytes kept, is 50.
	if want := "(84 tokens, 50 omitted)"; !strings.Contains(out.Content, want) {
		t.Errorf("truncation hint does not carry %q:\n%s", want, out.Content)
	}
	// The truncated memory is 189 bytes — 100 of preview plus an 89-byte hint —
	// and (189+2)/3 is 63.
	if out.Tokens != 63 {
		t.Errorf("truncated memory reports %d tokens for its %d bytes, want 63",
			out.Tokens, len(out.Content))
	}
	if len(out.Content) != 189 {
		t.Errorf("truncated memory is %d bytes, want 189 — the token count above "+
			"is a literal and moves with this length", len(out.Content))
	}
}
