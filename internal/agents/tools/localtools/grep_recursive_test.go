package localtools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ripgrep tool advertises a "recursive" key in its InputSchema and in its
// description, and until this change its handler never read the field. A
// caller asking for a non-recursive search got a recursive one, with no error
// and no mention of the field either way — the silent shape this whole family
// of cards is about.
//
// The axis these cases vary is the one an unconsidered list gets wrong here:
// whether the key is ABSENT, true, or false. Two of those three arrive at a
// plain Go bool as the same zero value, so a case list that only ever sends
// the key cannot see a handler that has broken the default.

func grepTool(t *testing.T, in map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Grep().Run(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("ripgrep failed: %v", err)
	}
	return out
}

// treeWithNestedMatch builds a directory holding one matching file at the top
// level and another one level down, which is the smallest tree that can tell a
// recursive search from a non-recursive one.
func treeWithNestedMatch(t *testing.T) (root, topFile, nestedFile string) {
	t.Helper()
	root = t.TempDir()
	topFile = filepath.Join(root, "top.txt")
	if err := os.WriteFile(topFile, []byte("findme at the top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedFile = filepath.Join(sub, "nested.txt")
	if err := os.WriteFile(nestedFile, []byte("findme one level down\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, topFile, nestedFile
}

func TestRecursiveFalseDoesNotDescendIntoSubdirectories(t *testing.T) {
	root, _, _ := treeWithNestedMatch(t)

	out := grepTool(t, map[string]any{
		"pattern":   "findme",
		"paths":     []string{root},
		"recursive": false,
	})

	if !strings.Contains(out, "top.txt") {
		t.Errorf("recursive=false dropped the file directly inside the search path; want top.txt in:\n%s", out)
	}
	if strings.Contains(out, "nested.txt") {
		t.Errorf("recursive=false still searched a subdirectory — the field is advertised and not honored; got:\n%s", out)
	}
}

func TestRecursiveTrueDescendsIntoSubdirectories(t *testing.T) {
	root, _, _ := treeWithNestedMatch(t)

	out := grepTool(t, map[string]any{
		"pattern":   "findme",
		"paths":     []string{root},
		"recursive": true,
	})

	if !strings.Contains(out, "top.txt") || !strings.Contains(out, "nested.txt") {
		t.Errorf("recursive=true must search the whole tree; want top.txt and nested.txt in:\n%s", out)
	}
}

// The case that pins the default, and the only one that can see a handler
// which reads the field into a plain bool. Omitting the key must mean the
// default the schema states — "default: true" — not Go's zero value.
func TestOmittingRecursiveSearchesRecursively(t *testing.T) {
	root, _, _ := treeWithNestedMatch(t)

	out := grepTool(t, map[string]any{
		"pattern": "findme",
		"paths":   []string{root},
	})

	if !strings.Contains(out, "nested.txt") {
		t.Errorf("a call that omits recursive must get the advertised default (true), got:\n%s", out)
	}
}

// A path naming a file rather than a directory has no depth to limit, so
// recursive=false must not stop it being searched.
func TestRecursiveFalseStillSearchesAFileNamedDirectly(t *testing.T) {
	_, _, nested := treeWithNestedMatch(t)

	out := grepTool(t, map[string]any{
		"pattern":   "findme",
		"paths":     []string{nested},
		"recursive": false,
	})

	if !strings.Contains(out, "findme one level down") {
		t.Errorf("recursive=false must still search a path that names a file; got:\n%s", out)
	}
}

// The tool's description promises "matching lines with file:line prefix".
// Both rg and grep drop the filename when exactly one file is searched, so
// the promise held only for searches that happened to match in more than one
// file. A single-file search is the only case that can see this.
func TestASingleFileSearchStillPrefixesTheFilename(t *testing.T) {
	_, _, nested := treeWithNestedMatch(t)

	out := grepTool(t, map[string]any{
		"pattern": "findme",
		"paths":   []string{nested},
	})

	if !strings.Contains(out, "nested.txt:1:") {
		t.Errorf("want the promised file:line prefix even for one file, got:\n%s", out)
	}
}

// The caller keeps the last word: the tool's -H goes in before the caller's
// flags precisely so a caller who wants bare lines can still ask for them.
func TestACallerFlagCanStillSuppressTheFilename(t *testing.T) {
	root, _, _ := treeWithNestedMatch(t)

	out := grepTool(t, map[string]any{
		"pattern": "findme",
		"paths":   []string{root},
		"flags":   "-I", // rg's "never print the path"; -h is --help in rg
	})

	if strings.Contains(out, "top.txt") {
		t.Errorf("the tool's own -H must not override a caller flag that comes after it; got:\n%s", out)
	}
	if !strings.Contains(out, "findme") {
		t.Errorf("the search itself should still have run; got:\n%s", out)
	}
}

// filesDirectlyInside is what the grep fallback uses to express a
// non-recursive search, because grep has no max-depth flag and reports a
// directory operand as an error. It is unit-tested rather than reached through
// the tool because rg is installed on this host, so the fallback branch never
// runs here — and an untested fallback is exactly the code that is wrong on
// the machine that needs it.
func TestFilesDirectlyInsideExpandsDirectoriesAndKeepsFiles(t *testing.T) {
	root, topFile, nestedFile := treeWithNestedMatch(t)

	got := filesDirectlyInside([]string{root})
	if len(got) != 1 || got[0] != topFile {
		t.Errorf("a directory operand must expand to the regular files directly inside it; want [%s], got %v", topFile, got)
	}

	got = filesDirectlyInside([]string{nestedFile})
	if len(got) != 1 || got[0] != nestedFile {
		t.Errorf("an operand naming a file must pass through unchanged; want [%s], got %v", nestedFile, got)
	}
}

// The empty result is the dangerous one: grep invoked with no path operand
// reads stdin, and a tool call blocked on stdin never returns. The handler
// must answer instead of running the command.
func TestNonRecursiveSearchOfAnEmptyDirectoryReturnsRatherThanReadingStdin(t *testing.T) {
	empty := t.TempDir()

	if got := filesDirectlyInside([]string{empty}); len(got) != 0 {
		t.Errorf("an empty directory must expand to no operands, got %v", got)
	}

	out := grepTool(t, map[string]any{
		"pattern":   "findme",
		"paths":     []string{empty},
		"recursive": false,
	})
	if out != "no matches found" {
		t.Errorf("want the no-matches answer, got %q", out)
	}
}

// Everything below asserts the argv rather than the search result, because
// the grep fallback cannot run on a host that has ripgrep installed — which is
// every host that develops this tool. Reaching it through Grep().Run would
// silently exercise the rg branch and pass for the wrong reason.

func boolPtr(b bool) *bool { return &b }

func argvString(args []string) string { return strings.Join(args, " ") }

func TestTheGrepFallbackNamesTheFilesItselfWhenNotRecursive(t *testing.T) {
	root, topFile, _ := treeWithNestedMatch(t)

	bin, args, nothing := searchCommand("findme", []string{root}, "", boolPtr(false), false)
	if bin != "grep" {
		t.Fatalf("want the grep fallback, got %q", bin)
	}
	if nothing {
		t.Fatal("a directory holding a file is something to search")
	}
	argv := argvString(args)
	if strings.Contains(argv, "-r") {
		t.Errorf("a non-recursive search must not pass -r; got %q", argv)
	}
	if !strings.Contains(argv, topFile) {
		t.Errorf("grep cannot search a directory without -r, so the file must be named; got %q", argv)
	}
	if strings.Contains(argv, root+" ") || strings.HasSuffix(argv, root) {
		t.Errorf("the directory itself must not be an operand — grep answers 'Is a directory'; got %q", argv)
	}
}

func TestTheGrepFallbackRecursesWithDashRWhenAsked(t *testing.T) {
	root, _, _ := treeWithNestedMatch(t)

	bin, args, nothing := searchCommand("findme", []string{root}, "", boolPtr(true), false)
	if bin != "grep" || nothing {
		t.Fatalf("want a runnable grep fallback, got bin=%q nothing=%v", bin, nothing)
	}
	argv := argvString(args)
	if !strings.Contains(argv, "-rn") {
		t.Errorf("a recursive grep needs -r; got %q", argv)
	}
	if !strings.Contains(argv, root) {
		t.Errorf("the directory is the operand for a recursive search; got %q", argv)
	}
}

// The description's file:line promise is made once and has to be kept by
// whichever binary runs. Asserting it only through Grep().Run tests the rg
// branch twice and the fallback never.
func TestBothBinariesAreAskedToPrintTheFilename(t *testing.T) {
	root, _, _ := treeWithNestedMatch(t)

	for _, tc := range []struct {
		name        string
		rgAvailable bool
		recursive   *bool
	}{
		{"rg recursive", true, boolPtr(true)},
		{"rg non-recursive", true, boolPtr(false)},
		{"grep recursive", false, boolPtr(true)},
		{"grep non-recursive", false, boolPtr(false)},
	} {
		_, args, nothing := searchCommand("findme", []string{root}, "", tc.recursive, tc.rgAvailable)
		if nothing {
			t.Fatalf("%s: nothing to search in a directory holding a file", tc.name)
		}
		found := false
		for _, a := range args {
			if a == "-H" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want -H so the promised file:line prefix survives a single-file search, got %q",
				tc.name, argvString(args))
		}
	}
}

// The case the sabotage run showed was pinned by nothing: with rg installed,
// removing this guard changes no observable result, because rg with no operand
// searches the working directory anyway. Only the argv can see it.
func TestTheGrepFallbackRefusesToRunWithNoOperand(t *testing.T) {
	empty := t.TempDir()

	_, args, nothing := searchCommand("findme", []string{empty}, "", boolPtr(false), false)
	if !nothing {
		t.Errorf("an empty directory leaves grep no operand, and grep with no operand reads stdin forever; argv was %q", argvString(args))
	}
}

// Same blind spot, other end: rg defaults to the working directory on its own,
// so dropping the explicit "." is invisible through the rg branch and breaks
// only the fallback.
func TestAnAbsentPathBecomesAnExplicitDotForBothBinaries(t *testing.T) {
	for _, rgAvailable := range []bool{true, false} {
		_, args, nothing := searchCommand("findme", nil, "", nil, rgAvailable)
		if nothing {
			t.Fatalf("rgAvailable=%v: a default search of . is something to search", rgAvailable)
		}
		if args[len(args)-1] != "." {
			t.Errorf("rgAvailable=%v: want an explicit \".\" operand rather than relying on the binary's default, got %q",
				rgAvailable, argvString(args))
		}
	}
}

// Every property the schema advertises should be one the handler reads. This
// pins the specific gap that was found rather than the general rule, because
// the general rule needs a scan and this needs a test.
func TestEveryAdvertisedRipgrepPropertyIsReadByTheHandler(t *testing.T) {
	props := Grep().InputSchema.Properties
	for _, key := range []string{"pattern", "paths", "recursive", "flags"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("schema no longer advertises %q — this test is aimed at the wrong keys", key)
		}
	}
	if len(props) != 4 {
		t.Errorf("the schema advertises %d properties but only 4 are covered by cases here; add a case for the new one rather than deleting this check", len(props))
	}
}
