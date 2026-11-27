package localtools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rune-boundary suite next door slides a four-byte rune across every cut in
// this package and scores 17/17 against mutation. It pins the CUTS. It pins no
// BUDGET: every one of its tests either supplies its own byte budget as a test
// constant (web_fetch's `max_chars: 64`, the scheduler's `budget = 500`) or is
// written in terms of the production constant it would have to hold in place
// (`straddlingLeads(maxWholeFileReadBytes)`). A fixture written in terms of a
// constant agrees with itself for every value of it, so all six of shell's
// budgets, the whole-file byte cap, both task-plan budgets, the scheduler's run
// budget and web_fetch's default could each be moved to an adjacent value with
// the entire package green.
//
// Each test below straddles one boundary: an input of exactly N, which must
// come back whole, and an input of exactly N+1, which must be cut to N. That
// pair reddens whichever way the literal moves. Where the cut keeps part of a
// string, the assertion marks a CHARACTER rather than counting bytes — a cut
// that keeps the wrong end of the input returns exactly the right length.
//
// These are insurance, not defect reports. Every value checked here was already
// correct; nothing was holding it there.
//
// ⚠️ This is a FLOOR, not a total: every budget named above is declared in
// package localtools, and so is every budget this file can reach. It is not the list
// of budgets that decide what a tool returns. readSingleFile is the whole of the
// difference — it is cut by two independent limits and only one of them is here.
// It is the only such site: every other schema cut this package makes
// (TruncateAtRuneBoundary, TruncateList) is handed its budget as an argument
// declared here, so those budgets are in scope. TruncateFileRead alone carries
// its own, which is what puts it out of reach.
// maxWholeFileReadBytes (fs.go) is; schema.TruncateFileRead's line window is
// not, and its fileReadWholeFileLimit, fileReadKeepFirst and fileReadKeepLast
// are unreachable from this package because they are unexported in schema.
// All three are held next door in schema/truncate_test.go, though not all in
// this file's way: fileReadWholeFileLimit is straddled, while fileReadKeepFirst
// and fileReadKeepLast are asserted by value off a single oversized fixture.
// Read the two suites together before concluding a budget is covered, and when
// a cut moves to another package its pin has to move with it — a budget that
// leaves package localtools leaves this list silently.

// markedRun returns n bytes whose last byte is a marker, so an assertion can
// name the byte the budget decides on rather than only the number of them.
//
// ⚠️ The marker has to be a byte the REST of the result cannot contain, not
// merely one the filler avoids. 'Z' reads like a safe choice and is not: the
// scheduler tool prints an RFC3339 timestamp, whose zone designator is a
// literal Z, so `strings.Contains(out, "Z")` was satisfied 400 bytes before
// the cut it was asked about. '~' appears in none of these tools' banners.
const budgetMarker = '~'

func markedRun(n int, marker byte) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("a", n-1) + string(marker)
}

// ---- tools/shell.go: truncateShellOutput ----

// maxChars is the ceiling that decides whether the character branch fires at
// all. A single line, so the line limit cannot fire and mask the answer.
func TestShellCharacterCeilingIsPinnedToItsExactValue(t *testing.T) {
	const maxChars = 50000

	whole := markedRun(maxChars, budgetMarker)
	if got := truncateShellOutput(whole); got != whole {
		t.Errorf("output of exactly %d bytes was not returned whole: %d bytes back, truncated=%t",
			maxChars, len(got), strings.Contains(got, "characters omitted"))
	}

	over := markedRun(maxChars+1, budgetMarker)
	got := truncateShellOutput(over)
	if !strings.Contains(got, "characters omitted") {
		t.Errorf("output of exactly %d bytes (one over the ceiling) was returned whole", maxChars+1)
	}
	if len(got) >= len(over) {
		t.Fatalf("input was not truncated at all, so the test proves nothing")
	}
}

// headChars and tailChars decide how much of an over-long output survives at
// each end. The markers pin which bytes those are, so a budget that moves by
// one is caught even though the total length barely changes.
func TestShellHeadAndTailCharacterBudgetsArePinnedToTheirExactValues(t *testing.T) {
	const (
		headChars = 25000
		tailChars = 20000
	)

	// Four markers, one per byte the two budgets decide on:
	//   H  the last byte the head may keep      (offset headChars-1)
	//   X  the first byte the head must drop    (offset headChars)
	//   Y  the last byte the tail must drop     (offset len-tailChars-1)
	//   T  the first byte the tail keeps        (offset len-tailChars)
	// The middle filler makes the whole string longer than the ceiling so the
	// character branch is the one that fires.
	in := markedRun(headChars, 'H') + "X" +
		strings.Repeat("m", 20000) +
		"Y" + "T" + strings.Repeat("b", tailChars-1)

	out := truncateShellOutput(in)
	head, rest, found := strings.Cut(out, "\n\n[... ")
	if !found {
		t.Fatalf("output has no truncation marker; input was %d bytes", len(in))
	}
	_, tail, _ := strings.Cut(rest, " ...]\n\n")

	if len(head) != headChars {
		t.Errorf("head is %d bytes, want exactly %d", len(head), headChars)
	}
	if !strings.HasSuffix(head, "H") {
		t.Errorf("head does not end on the byte at offset %d; the head budget moved", headChars-1)
	}
	if strings.Contains(head, "X") {
		t.Errorf("head kept the byte at offset %d, which is past its budget", headChars)
	}

	if len(tail) != tailChars {
		t.Errorf("tail is %d bytes, want exactly %d", len(tail), tailChars)
	}
	if !strings.HasPrefix(tail, "T") {
		t.Errorf("tail does not start at %d bytes from the end; the tail budget moved", tailChars)
	}
	if strings.Contains(tail, "Y") {
		t.Errorf("tail kept the byte just before its budget, so it started one byte early")
	}
}

// maxLines, headLines and tailLines are the second, independent branch: it
// fires only for output that is under the character ceiling and over the line
// ceiling, which is why no character-budget fixture reaches it.
func TestShellLineBudgetsArePinnedToTheirExactValues(t *testing.T) {
	const (
		maxLines  = 500
		headLines = 250
		tailLines = 200
	)

	lines := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("line%d", i)
		}
		return strings.Join(out, "\n")
	}

	whole := lines(maxLines)
	if got := truncateShellOutput(whole); got != whole {
		t.Errorf("output of exactly %d lines was not returned whole", maxLines)
	}

	over := lines(maxLines + 1)
	out := truncateShellOutput(over)
	if !strings.Contains(out, "lines omitted") {
		t.Fatalf("output of exactly %d lines (one over the ceiling) was returned whole", maxLines+1)
	}

	head, rest, _ := strings.Cut(out, "\n\n[... ")
	_, tail, _ := strings.Cut(rest, " ...]\n\n")

	if got := len(strings.Split(head, "\n")); got != headLines {
		t.Errorf("head is %d lines, want exactly %d", got, headLines)
	}
	// Name the lines, not just how many: a head that kept the wrong 250 is the
	// same length as one that kept the right 250.
	if !strings.HasSuffix(head, "line"+fmt.Sprint(headLines-1)) {
		t.Errorf("head does not end at line %d; the head-line budget moved", headLines-1)
	}
	if got := len(strings.Split(tail, "\n")); got != tailLines {
		t.Errorf("tail is %d lines, want exactly %d", got, tailLines)
	}
	if want := "line" + fmt.Sprint(maxLines+1-tailLines); !strings.HasPrefix(tail, want) {
		t.Errorf("tail does not start at %q; the tail-line budget moved", want)
	}
}

// ---- tools/fs.go ----

// maxWholeFileReadBytes decides whether a whole-file read is cut at all, and
// the banner it writes is read by inber's cache, so the value is load-bearing
// past this package.
func TestWholeFileByteCapIsPinnedToItsExactValue(t *testing.T) {
	// ⚠️ Spelled out, NOT `= maxWholeFileReadBytes`. Writing it in terms of the
	// constant is what the neighboring rune test does, and it is why the cap
	// was unpinned in the first place: the fixture moves with the value, so the
	// two agree at every value and the test can never fail for a wrong one.
	// This literal has to be updated by hand when the cap changes on purpose,
	// which is the entire point of it.
	const byteCap = 100_000

	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "big.txt")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// One long line, so the line window cannot fire and mask the byte cap.
	whole := readSingleFile(write(t, markedRun(byteCap, budgetMarker)), 0, 0)
	if strings.Contains(whole, "partial read") {
		t.Errorf("a file of exactly %d bytes was reported as a partial read", byteCap)
	}
	// A file cut at exactly its own length loses no bytes, so the banner above
	// stays silent — droppedBytes is 0 — and the only trace the cut ran at all
	// is this suffix. Without it, widening the guard to `>=` is invisible here.
	if strings.Contains(whole, "... (truncated)") {
		t.Errorf("a file of exactly %d bytes was marked truncated", byteCap)
	}

	over := readSingleFile(write(t, markedRun(byteCap+1, budgetMarker)), 0, 0)
	if want := fmt.Sprintf("[partial read — %d of %d bytes", byteCap, byteCap+1); !strings.Contains(over, want) {
		t.Errorf("a file of exactly %d bytes did not report %q", byteCap+1, want)
	}
	if strings.Contains(over, string(budgetMarker)) {
		t.Errorf("the read kept the byte at offset %d, which is past the cap", byteCap)
	}
}

// maxEntries caps the recursive walk. The walk reports the cap in its
// stopped-early notice, so the value is assertable directly — this test names
// it, which is the one thing the notice's own test in fs_list_test.go does not
// do.
//
// Measured rather than assumed, by moving the constant and re-running both:
// TestTheWalkCapNoticeSurvivesTheDisplayCut walks a 1200-entry fixture, so it
// catches a cap moved to 10000 (the walk then completes and reports a total)
// but stays GREEN at 999 and at 1001 — its fixture straddles those, so the
// walk still stops early and it still finds the strings it looks for. The
// assertions below fail on all three. Off by one is the move this test exists
// to catch; the order-of-magnitude move is already covered next door.
func TestRecursiveWalkEntryCapIsPinnedToItsExactValue(t *testing.T) {
	const maxEntries = 1000

	list := func(t *testing.T, n int) string {
		t.Helper()
		dir := t.TempDir()
		for i := 0; i < n; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		in, err := json.Marshal(map[string]any{"path": dir, "recursive": true})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ListFiles().Run(context.Background(), string(in))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// One entry over the cap: the walk gives up and says so, naming the number
	// of entries it managed to see. That number is the cap.
	over := list(t, maxEntries+1)
	if want := fmt.Sprintf("Listing stopped after %d items", maxEntries); !strings.Contains(over, want) {
		t.Errorf("the stopped-early notice does not report %q, so the walk cap is not %d:\n%s",
			want, maxEntries, tailOf(over))
	}

	// A walk that stopped cannot know the size of the tree, so it must not
	// print a total. This is the half that makes the count above load-bearing
	// rather than decorative: a figure labeled "Total" is read as the size of
	// the directory, and at the cap it is the size of the walk.
	if strings.Contains(over, "Total:") {
		t.Errorf("a listing that stopped at its cap still reports a total, which it cannot know:\n%s",
			tailOf(over))
	}

	// One entry under the cap the walk completes, so it reports a true total
	// and never claims it stopped. This is what separates a moved cap from a
	// directory that simply held fewer files: both show 50 rows, and only the
	// footer tells them apart.
	under := list(t, maxEntries-1)
	if want := fmt.Sprintf("Total: %d items", maxEntries-1); !strings.Contains(under, want) {
		t.Errorf("a directory of %d entries did not report %q, so the walk cap fired below its value:\n%s",
			maxEntries-1, want, tailOf(under))
	}
	if strings.Contains(under, "Listing stopped") {
		t.Errorf("a walk that enumerated %d entries in full claims it stopped early:\n%s",
			maxEntries-1, tailOf(under))
	}
}

// tailOf returns the last few lines of a listing, which is where every footer
// this file asserts on lives. A failure message carrying the whole listing
// would be a thousand paths long.
func tailOf(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return strings.Join(lines, "\n")
}

// The shallow listing hands its rows to schema.TruncateList with a literal cap.
func TestShallowListingItemCapIsPinnedToItsExactValue(t *testing.T) {
	const maxItems = 50

	run := func(t *testing.T, n int) string {
		t.Helper()
		dir := t.TempDir()
		for i := 0; i < n; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		in, err := json.Marshal(map[string]any{"path": dir})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ListFiles().Run(context.Background(), string(in))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if out := run(t, maxItems); strings.Contains(out, "more items") {
		t.Errorf("a listing of exactly %d entries was truncated", maxItems)
	}
	over := run(t, maxItems+1)
	if !strings.Contains(over, "[...1 more items...]") {
		t.Errorf("a listing of exactly %d entries did not drop exactly one", maxItems+1)
	}
}

// The recursive listing hands its rows to schema.TruncateList at a SECOND call
// site, with its own copy of the same literal cap. The shallow test above
// cannot reach this one: moving the recursive cap by one left the whole
// package green, scored UNNOTICED by scripts/sabotage-truncation.py.
//
// It is the call site where the number matters more. The shallow listing draws
// on os.ReadDir, which returns the directory whole, so its footer always
// carries a true total and a reader can subtract to recover the shown count.
// The recursive listing can stop at maxWalkedEntries, and then its population
// is PopulationStoppedEarly — a count of what the WALK saw, not of the tree —
// so the number of rows on screen is not reconstructible from the footer. This
// test therefore counts the rows rather than reading the footer, and asserts
// the count in both regimes.
//
// ⚠️ The cap is written here as a literal, deliberately, and not as
// maxListedEntries. The two call sites share that constant, so a case naming it
// moves both at once and could not say which listing a test reached — which is
// the whole distinction this test exists to draw.
func TestRecursiveListingItemCapIsPinnedToItsExactValue(t *testing.T) {
	const maxItems = 50

	// Every file goes one level down, so the entries this test counts are
	// reachable ONLY by the recursive walk: run the same fixture shallow and
	// the listing is one row, the subdirectory. A fixture flat enough for the
	// shallow path to answer would let this test pass without ever reaching
	// the call site it is written for.
	run := func(t *testing.T, entries int) string {
		t.Helper()
		dir := t.TempDir()
		nested := filepath.Join(dir, "nested")
		if err := os.Mkdir(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		// The directory itself is entry number one.
		for i := 0; i < entries-1; i++ {
			if err := os.WriteFile(filepath.Join(nested, fmt.Sprintf("f%04d.txt", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		in, err := json.Marshal(map[string]any{"path": dir, "recursive": true})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ListFiles().Run(context.Background(), string(in))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Exactly at the cap the listing is shown whole: no marker, no footer.
	if out := run(t, maxItems); strings.Contains(out, "more items") {
		t.Errorf("a recursive listing of exactly %d entries was truncated:\n%s", maxItems, tailOf(out))
	} else if got := shownRows(out); got != maxItems {
		t.Errorf("a recursive listing of exactly %d entries showed %d rows", maxItems, got)
	}

	// One over, and exactly one is dropped. Asserting the marker separates a
	// cap moved UP: at 51 the population is complete and the list fits, so
	// TruncateList returns the bare rows and no marker is printed at all.
	over := run(t, maxItems+1)
	if !strings.Contains(over, "[...1 more items...]") {
		t.Errorf("a recursive listing of %d entries did not drop exactly one:\n%s", maxItems+1, tailOf(over))
	}
	if got := shownRows(over); got != maxItems {
		t.Errorf("a recursive listing of %d entries showed %d rows, not %d", maxItems+1, got, maxItems)
	}

	// The regime the footer cannot describe. Past maxWalkedEntries the walk
	// gives up, so the footer counts what the walk saw and says the total is
	// unknown; the shown-row count is pinned here by counting, and by nothing
	// else. It is also what separates this assertion from the walk cap: move
	// maxWalkedEntries and this number does not budge.
	stopped := run(t, 1200)
	if !strings.Contains(stopped, "Listing stopped after") {
		t.Fatalf("a recursive listing of 1200 entries did not stop early, so the "+
			"stopped-early regime went untested:\n%s", tailOf(stopped))
	}
	if got := shownRows(stopped); got != maxItems {
		t.Errorf("a recursive listing that stopped at its walk cap showed %d rows, not %d", got, maxItems)
	}
}

// shownRows counts the entry rows of a listing. TruncateList writes the rows,
// then a blank line, then the marker and footer, so the rows are everything
// before the first blank line. An untruncated listing has no blank line and no
// footer, and is rows all the way down.
func shownRows(out string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			break
		}
		n++
	}
	return n
}

// ---- tools/web_fetch.go ----

// The default budget is reached only by a call that names no max_chars, and
// every existing web_fetch test names one.
func TestWebFetchDefaultCharacterBudgetIsPinnedToItsExactValue(t *testing.T) {
	const defaultMaxChars = 50000

	fetch := func(t *testing.T, body string, args map[string]any) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, body)
		}))
		defer server.Close()
		args["url"] = server.URL
		in, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		out, err := WebFetch().Run(context.Background(), string(in))
		if err != nil {
			t.Fatalf("fetch failed: %v", err)
		}
		return out
	}

	whole := fetch(t, markedRun(defaultMaxChars, budgetMarker), map[string]any{})
	if strings.Contains(whole, "(truncated)") {
		t.Errorf("a %d-byte page was truncated at the default budget", defaultMaxChars)
	}
	if !strings.HasSuffix(whole, string(budgetMarker)) {
		t.Errorf("a %d-byte page did not come back whole", defaultMaxChars)
	}

	over := fetch(t, markedRun(defaultMaxChars+1, budgetMarker), map[string]any{})
	if !strings.Contains(over, "(truncated)") {
		t.Errorf("a %d-byte page was not truncated at the default budget", defaultMaxChars+1)
	}
	if strings.Contains(over, string(budgetMarker)) {
		t.Errorf("the cut kept the byte at offset %d, past the default budget", defaultMaxChars)
	}
}

// The unset-budget guard is `maxChars <= 0`. A request for exactly one
// character is the adjacent value it must NOT swallow.
func TestWebFetchHonorsACharacterBudgetOfOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, strings.Repeat("a", 100))
	}))
	defer server.Close()

	in, err := json.Marshal(map[string]any{"url": server.URL, "max_chars": 1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := WebFetch().Run(context.Background(), string(in))
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if got := strings.TrimSuffix(out, "\n... (truncated)"); got != "a" {
		t.Errorf("max_chars=1 kept %d bytes, want 1 — the guard treated it as unset", len(got))
	}
}

// ---- tools/scheduler.go ----

func TestSchedulerRunOutputBudgetIsPinnedToItsExactValue(t *testing.T) {
	const budget = 500

	runs := func(t *testing.T, output string) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"id":1,"status":"success","started_at":"2026-08-08T00:00:00Z","output":%s}]`,
				mustJSONString(t, output))
		}))
		defer server.Close()
		out, err := handleRuns(context.Background(), server.URL, http.DefaultClient, 1)
		if err != nil {
			t.Fatalf("runs failed: %v", err)
		}
		return out
	}

	if out := runs(t, markedRun(budget, budgetMarker)); strings.Contains(out, "... (truncated)") {
		t.Errorf("run output of exactly %d bytes was truncated", budget)
	}
	over := runs(t, markedRun(budget+1, budgetMarker))
	if !strings.Contains(over, "... (truncated)") {
		t.Errorf("run output of exactly %d bytes was not truncated", budget+1)
	}
	if strings.Contains(over, string(budgetMarker)) {
		t.Errorf("the cut kept the byte at offset %d, past the budget", budget)
	}
}

// ---- tools/task_plan.go ----

func TestBuildOutputBudgetIsPinnedToItsExactValue(t *testing.T) {
	const budget = 2000

	build := func(t *testing.T, payload string) buildResult {
		t.Helper()
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, "payload.txt"), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		restore := TaskPlanBuildCommand
		TaskPlanBuildCommand = "cat payload.txt"
		defer func() { TaskPlanBuildCommand = restore }()
		return runBuild(context.Background(), repoRoot)
	}

	whole := build(t, markedRun(budget, budgetMarker))
	if strings.Contains(whole.Output, "...(truncated)") {
		t.Errorf("build output of exactly %d bytes was truncated", budget)
	}
	if !strings.HasSuffix(whole.Output, string(budgetMarker)) {
		t.Errorf("build output of exactly %d bytes did not come back whole", budget)
	}

	over := build(t, markedRun(budget+1, budgetMarker))
	if !strings.Contains(over.Output, "...(truncated)") {
		t.Errorf("build output of exactly %d bytes was not truncated", budget+1)
	}
	if strings.Contains(over.Output, string(budgetMarker)) {
		t.Errorf("the cut kept the byte at offset %d, past the budget", budget)
	}
}

func TestBuildErrorTaskOutputBudgetIsPinnedToItsExactValue(t *testing.T) {
	const budget = 500

	plan := func(t *testing.T, buildOutput string) string {
		t.Helper()
		repoRoot := t.TempDir()
		if err := AddBuildErrorTask(repoRoot, buildOutput); err != nil {
			t.Fatal(err)
		}
		written, err := os.ReadFile(filepath.Join(repoRoot, taskPlanFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(written)
	}

	whole := plan(t, markedRun(budget, budgetMarker))
	if !strings.Contains(whole, string(budgetMarker)) {
		t.Errorf("build output of exactly %d bytes was cut short of its last byte", budget)
	}

	over := plan(t, markedRun(budget+1, budgetMarker))
	if strings.Contains(over, string(budgetMarker)) {
		t.Errorf("the cut kept the byte at offset %d, past the budget", budget)
	}
	if !strings.Contains(over, "...") {
		t.Errorf("build output of exactly %d bytes was not truncated", budget+1)
	}
}
