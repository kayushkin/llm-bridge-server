package localtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recent_files answers "what has been worked on lately", and an agent uses it
// to decide where to look before it reads anything. Measured on `main`: a
// panic() on the first line of RecentFiles, formatRecentFiles OR parseDuration
// left `go test ./...` green.
//
// ⚠️ The three finder functions — findRecentlyModified, findRecentlyModifiedGit
// and findRecentlyModifiedMtime — are the exception, and the distinction is the
// point. cancellation_test.go calls findRecentlyModified, so a panic() there
// reddens the suite and the reach guard reads it as covered. Nothing asserts
// which files it returns: that test pins that a canceled call stops, not that
// an uncanceled one is right. A reach guard answers "somebody calls this",
// which is weaker than "somebody checks this", so the finders are characterised
// here alongside the two rows the census could see.

func runRecentFiles(t *testing.T, root string, params map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RecentFiles(root).Run(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("recent_files failed: %v", err)
	}
	return out
}

func writeFileAged(t *testing.T, root, rel string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("body of "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// gitCommitAll makes root a git repo and commits everything in it, stamped at
// `age` before now. The stamp is what separates the git finder from the mtime
// finder: git reads commit dates, the walk reads mtimes, and they can disagree.
func gitCommitAll(t *testing.T, root string, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age).Format(time.RFC3339)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "fixture")
}

func requireLists(t *testing.T, out string, rel string) {
	t.Helper()
	if !strings.Contains(out, "**"+rel+"**") {
		t.Errorf("want %q listed, got:\n%s", rel, out)
	}
}

func requireDoesNotList(t *testing.T, out string, rel string) {
	t.Helper()
	if strings.Contains(out, "**"+rel+"**") {
		t.Errorf("%q should not be listed, got:\n%s", rel, out)
	}
}

// ---- which files come back ----

func TestOutsideAGitRepoTheWindowIsMeasuredFromModificationTime(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "fresh.go", 10*time.Minute)
	writeFileAged(t, root, "stale.go", 72*time.Hour)

	out := runRecentFiles(t, root, map[string]any{"since": "24h"})

	requireLists(t, out, "fresh.go")
	requireDoesNotList(t, out, "stale.go")
	// The row carries the file's own age, not the age of the scan. Asserting
	// only which files come back leaves the timestamp free to be time.Now(),
	// which is right for every row at the moment the walk runs and wrong the
	// moment anything caches the answer.
	if !strings.Contains(out, "**fresh.go** (2 lines, 10m ago)") {
		t.Errorf("want the row labeled with the file's own age, got:\n%s", out)
	}
}

// ⚠️ The two finders answer different questions and the tool does not say
// which one it used. Inside a git repo with a commit in the window, the
// listing is the commit's file list — so a file edited five minutes ago and
// not yet committed does not appear, while a file untouched for three days
// does if it rode in on that commit. An agent asking "what am I working on"
// gets the answer to "what was committed" instead.
func TestInsideAGitRepoTheWindowIsMeasuredFromCommitsRatherThanEdits(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "committed.go", 0)
	gitCommitAll(t, root, 1*time.Minute)
	// Committed, then backdated on disk: old to the walk, recent to git.
	writeFileAged(t, root, "committed.go", 72*time.Hour)
	// Edited just now and never committed: recent to the walk, invisible to git.
	writeFileAged(t, root, "uncommitted.go", 1*time.Minute)

	out := runRecentFiles(t, root, map[string]any{"since": "24h"})

	requireLists(t, out, "committed.go")
	requireDoesNotList(t, out, "uncommitted.go")
}

// When git answers with nothing the tool does not report "nothing" — an empty
// git result is indistinguishable from git being unavailable, so it falls
// through to the walk. That is what makes the previous test's behavior
// conditional on there being any commit in the window at all.
func TestAGitRepoWithNoCommitsInTheWindowFallsBackToModificationTime(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "committed.go", 0)
	gitCommitAll(t, root, 90*24*time.Hour)
	writeFileAged(t, root, "uncommitted.go", 1*time.Minute)

	out := runRecentFiles(t, root, map[string]any{"since": "24h"})

	requireLists(t, out, "uncommitted.go")
}

func TestTheWalkSkipsTheHeavyDirectories(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "keep.go", 1*time.Minute)
	writeFileAged(t, root, ".git/config", 1*time.Minute)
	writeFileAged(t, root, "node_modules/p/index.js", 1*time.Minute)
	writeFileAged(t, root, "vendor/v/v.go", 1*time.Minute)

	out := runRecentFiles(t, root, map[string]any{"since": "24h"})

	requireLists(t, out, "keep.go")
	for _, skipped := range []string{
		filepath.Join(".git", "config"),
		filepath.Join("node_modules", "p", "index.js"),
		filepath.Join("vendor", "v", "v.go"),
	} {
		requireDoesNotList(t, out, skipped)
	}
}

// ⚠️ The walk applies no extension filter and no ignore list at all, unlike
// repo_map in the same package. Every file under the root that is new enough
// is listed, binaries included.
func TestTheWalkFiltersOnAgeAloneAndNotOnFileType(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "logo.png", 1*time.Minute)
	writeFileAged(t, root, "a.out", 1*time.Minute)

	out := runRecentFiles(t, root, map[string]any{"since": "24h"})

	requireLists(t, out, "logo.png")
	requireLists(t, out, "a.out")
}

func TestAnEmptyResultSaysSoAndQuotesTheWindowBack(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "stale.go", 72*time.Hour)

	out := runRecentFiles(t, root, map[string]any{"since": "2h"})

	if out != "No files modified in the last 2h." {
		t.Errorf("want the empty-result sentence naming the window, got %q", out)
	}
}

// The default is documented on the schema and applied in the Run body; it is
// also the string echoed back by the empty-result sentence, so one constant
// serves as both the window and the message.
func TestAnAbsentWindowDefaultsToTwentyFourHours(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "recent.go", 12*time.Hour)
	writeFileAged(t, root, "old.go", 36*time.Hour)

	out := runRecentFiles(t, root, map[string]any{})

	requireLists(t, out, "recent.go")
	requireDoesNotList(t, out, "old.go")

	empty := runRecentFiles(t, t.TempDir(), map[string]any{})
	if empty != "No files modified in the last 24h." {
		t.Errorf("the default window is not echoed as 24h: %q", empty)
	}
}

func TestAnUnreadableRootIsReportedRatherThanReadAsQuiet(t *testing.T) {
	_, err := RecentFiles(filepath.Join(t.TempDir(), "no-such-dir")).Run(context.Background(), `{}`)
	if err == nil {
		t.Fatal("a root that does not exist was reported as no recent files")
	}
	if !strings.Contains(err.Error(), "failed to find recent files") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

// Run has three failure producers — the request that will not parse, the
// window string that will not parse, and the walk that fails — and only two of
// them wrap what they say. "it returned an error" cannot tell them apart, so
// this test names the producer twice over: the cause is a JSON syntax problem,
// which no other arm can raise, and the message is not the walk's.
//
// Both halves are load-bearing and neither subsumes the other. Measured
// 2026-08-21 on this branch: rewriting the arm to
// fmt.Errorf("failed to find recent files: %w", err) — a malformed request
// reported as a failed walk, which sends whoever reads it to the filesystem —
// leaves errors.As satisfied, because the syntax error survives the wrap. Only
// the second assertion reddens on it.
func TestUnparseableInputIsReportedRatherThanDefaulted(t *testing.T) {
	_, err := RecentFiles(t.TempDir()).Run(context.Background(), "{not json")
	if err == nil {
		t.Fatal("a malformed input was accepted as an empty request")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("a malformed request must be reported as one, and only the unmarshal arm raises a *json.SyntaxError; got %v", err)
	}
	if strings.Contains(err.Error(), "failed to find recent files") {
		t.Errorf("a request that will not parse was reported as the walk's failure: %v", err)
	}
}

// ---- the window string ----

func TestTheWindowAcceptsDaysOnTopOfGoDurations(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"90s", 90 * time.Second},
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"0d", 0},
	} {
		got, err := parseDuration(tc.in)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ⚠️ The days branch reads its number with Sscanf("%d"), which stops at the
// first non-digit and reports success on what it got. So anything ending in
// "d" is accepted as a day count: "1h30md" is one day rather than ninety
// minutes, and "3xd" is three days. The caller is told nothing — the window is
// silently the wrong size, and every file outside it is reported as not having
// been touched. Measured, not inferred.
func TestAnythingEndingInDIsReadAsADayCountUpToItsFirstNonDigit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"1h30md", 24 * time.Hour},
		{"3xd", 72 * time.Hour},
	} {
		got, err := parseDuration(tc.in)
		if err != nil {
			t.Errorf("parseDuration(%q) was rejected: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAWindowThatIsNotADurationIsRejectedByName(t *testing.T) {
	for _, bad := range []string{"yesterday", "2 hours", "d", ""} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) was accepted", bad)
		}
	}

	_, err := RecentFiles(t.TempDir()).Run(context.Background(), `{"since":"yesterday"}`)
	if err == nil {
		t.Fatal("an unparseable window was accepted")
	}
	if !strings.Contains(err.Error(), "invalid duration 'yesterday'") {
		t.Errorf("the error does not quote the window back: %v", err)
	}
}

// ⚠️ A negative window puts the cutoff in the future, so nothing is newer than
// it and the tool answers "no files modified in the last -24h" — a sentence
// that reads like a quiet repo rather than a bad request.
func TestANegativeWindowIsAcceptedAndReadsAsAQuietRepo(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "fresh.go", 1*time.Minute)

	out := runRecentFiles(t, root, map[string]any{"since": "-24h"})

	if out != "No files modified in the last -24h." {
		t.Errorf("want the empty-result sentence, got %q", out)
	}
}

// ---- how the list is rendered ----

func TestTheListIsNumberedAndHeadedWithATotal(t *testing.T) {
	files := []recentFile{
		{Path: writeTempFile(t, "one.go", "a\nb\n"), RelativePath: "one.go", ModTime: time.Now()},
		{Path: writeTempFile(t, "two.go", "c\n"), RelativePath: "two.go", ModTime: time.Now()},
	}

	out, err := formatRecentFiles(files, false)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(out, "# Recently Modified Files (2 total)\n\n") {
		t.Errorf("want a header carrying the count, got:\n%s", out)
	}
	if !strings.Contains(out, "1. **one.go**") || !strings.Contains(out, "2. **two.go**") {
		t.Errorf("want a 1-based numbering in list order, got:\n%s", out)
	}
}

// ⚠️ The line count is newline count plus one, which is a count of lines only
// for a file whose last line has no terminator. Every normal text file reports
// one line more than it has, and an empty file reports one line rather than
// none.
func TestTheLineCountIsNewlinesPlusOne(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{"", 1},
		{"one line, no newline", 1},
		{"one line\n", 2},
		{"a\nb\nc\n", 4},
	} {
		files := []recentFile{{
			Path:         writeTempFile(t, "f.go", tc.body),
			RelativePath: "f.go",
			ModTime:      time.Now(),
		}}
		out, err := formatRecentFiles(files, false)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("(%d lines,", tc.want); !strings.Contains(out, want) {
			t.Errorf("body %q: want %q, got:\n%s", tc.body, want, out)
		}
	}
}

// A file that cannot be read is still listed, at zero lines. Dropping it would
// hide a file the caller was told about by the finder; reporting zero at least
// distinguishes it from an empty file, which reports one.
func TestAFileThatCannotBeReadIsStillListedAtZeroLines(t *testing.T) {
	files := []recentFile{{
		Path:         filepath.Join(t.TempDir(), "vanished.go"),
		RelativePath: "vanished.go",
		ModTime:      time.Now(),
	}}

	out, err := formatRecentFiles(files, true)
	if err != nil {
		t.Fatalf("an unreadable file failed the whole listing: %v", err)
	}
	if !strings.Contains(out, "**vanished.go** (0 lines,") {
		t.Errorf("want the file listed at zero lines, got:\n%s", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("content was fenced for a file that could not be read:\n%s", out)
	}
}

func TestTheAgeIsBucketedIntoFourSpellings(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{59 * time.Minute, "59m ago"},
		{3 * time.Hour, "3h ago"},
		{23 * time.Hour, "23h ago"},
		{50 * time.Hour, "2d ago"},
		// Truncation, not rounding: six days and change is still 6d.
		{6*24*time.Hour + 23*time.Hour, "6d ago"},
	} {
		files := []recentFile{{
			Path:         writeTempFile(t, "f.go", "x"),
			RelativePath: "f.go",
			ModTime:      time.Now().Add(-tc.age),
		}}
		out, err := formatRecentFiles(files, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("age %v: want %q, got:\n%s", tc.age, tc.want, out)
		}
	}
}

// ⚠️ The age is computed from the file's mtime whatever finder produced the
// row, so inside a git repo a file listed because of a recent commit is
// labeled with the age of the file on disk. The two can be days apart, and
// the label is the only timestamp the caller sees.
func TestTheAgeShownIsAlwaysTheModificationTimeEvenOnAGitSourcedRow(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "committed.go", 0)
	gitCommitAll(t, root, 1*time.Minute)
	writeFileAged(t, root, "committed.go", 72*time.Hour)

	out := runRecentFiles(t, root, map[string]any{"since": "24h"})

	if !strings.Contains(out, "**committed.go** (2 lines, 3d ago)") {
		t.Errorf("want the row labeled with its mtime age, got:\n%s", out)
	}
}

func TestContentIsFencedOnlyWhenItWasAskedFor(t *testing.T) {
	files := []recentFile{{
		Path:         writeTempFile(t, "f.go", "package f\n"),
		RelativePath: "f.go",
		ModTime:      time.Now(),
	}}

	without, err := formatRecentFiles(files, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "package f") {
		t.Errorf("metadata-only output carried the file body:\n%s", without)
	}

	with, err := formatRecentFiles(files, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with, "```\npackage f\n") {
		t.Errorf("want the body inside a fence, got:\n%s", with)
	}
}

func TestIncludeContentTravelsFromTheRequestToTheOutput(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "note.txt", 1*time.Minute)

	quiet := runRecentFiles(t, root, map[string]any{"since": "24h"})
	loud := runRecentFiles(t, root, map[string]any{"since": "24h", "include_content": true})

	if strings.Contains(quiet, "body of note.txt") {
		t.Errorf("the body was included without being asked for:\n%s", quiet)
	}
	if !strings.Contains(loud, "body of note.txt") {
		t.Errorf("include_content did not reach the formatter:\n%s", loud)
	}
}

// ---- the tool's own declaration ----

func TestTheToolDeclaresItsWindowAndItsContentSwitch(t *testing.T) {
	impl := RecentFiles(t.TempDir())

	if impl.Name != "recent_files" {
		t.Errorf("tool name is %q", impl.Name)
	}
	raw, err := json.Marshal(impl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"since", "include_content"} {
		if !strings.Contains(string(raw), `"`+param+`"`) {
			t.Errorf("the schema does not declare %q: %s", param, raw)
		}
	}
	if strings.Contains(string(raw), `"required":["`) {
		t.Errorf("recent_files declares a required parameter: %s", raw)
	}
}
