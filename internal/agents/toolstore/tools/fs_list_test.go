package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listFiles runs the tool the way a model would call it.
func listFiles(t *testing.T, path string, recursive bool) string {
	t.Helper()
	out, err := ListFiles().Run(context.Background(), fmt.Sprintf(`{"path":%q,"recursive":%v}`, path, recursive))
	if err != nil {
		t.Fatalf("list_files(%q, recursive=%v) returned an error: %v", path, recursive, err)
	}
	return out
}

// repoWithVendoredTree builds a directory that a .gitignore would hide most of,
// with fileCount files inside the ignored subtree.
func repoWithVendoredTree(t *testing.T, fileCount int) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"src", "node_modules/pkg", "build", ".git/objects"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	write := func(rel string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write(".gitignore")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\nbuild/\n*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	write("README.md")
	write("debug.log")
	write("src/app.go")
	write("build/out.bin")
	write(".git/objects/loose")
	for i := 0; i < fileCount; i++ {
		write(filepath.Join("node_modules/pkg", fmt.Sprintf("f%d.js", i)))
	}
	return root
}

// The defect this file was written for: the description told the model the
// recursive listing was gitignore-filtered, and it reads no ignore file at all.
// Both halves are asserted, because "does not mention gitignore" alone is also
// true of a description that says nothing.
func TestListFilesDescriptionDoesNotPromiseIgnoreFiltering(t *testing.T) {
	description := ListFiles().Description

	for _, promise := range []string{"gitignore", ".gitignore", "respects"} {
		if strings.Contains(strings.ToLower(description), strings.ToLower(promise)) {
			t.Errorf("list_files description claims %q, but the walk reads no ignore file:\n%s", promise, description)
		}
	}
	// It has to say what it does skip, or the model has no way to predict the
	// listing it gets back.
	for _, required := range []string{"dot-director", "node_modules"} {
		if !strings.Contains(description, required) {
			t.Errorf("list_files description never mentions %q, so what it skips is undiscoverable:\n%s", required, description)
		}
	}
}

// The description and the behavior are one claim, so they are pinned together.
// If someone later teaches the walk to read .gitignore, this test fails and
// makes them go and correct the sentence in the same change.
func TestRecursiveListingWalksIgnoredDirectoriesAsItsDescriptionSays(t *testing.T) {
	root := repoWithVendoredTree(t, 3)
	out := listFiles(t, root, true)

	// .gitignore names all three of these; the walk lists them anyway.
	for _, ignored := range []string{"node_modules/", "build/out.bin", "debug.log"} {
		if !strings.Contains(out, ignored) {
			t.Errorf("%q is absent from the recursive listing — the walk now honors an ignore file, so the tool description must stop saying it does not:\n%s", ignored, out)
		}
	}
	// The one exclusion it really does have.
	if strings.Contains(out, ".git/objects") {
		t.Errorf("a dot-directory was walked, which is the tool's only documented exclusion:\n%s", out)
	}
}

// The walk cap fires at 1000 entries and the display cap at 50, so the notice
// saying the tree is bigger than the walk was entry 1001 of 1001 — always past
// the display cut, and never once read by the model it was written for.
func TestTheWalkCapNoticeSurvivesTheDisplayCut(t *testing.T) {
	root := repoWithVendoredTree(t, 1200)
	out := listFiles(t, root, true)

	if !strings.Contains(out, "1000") {
		t.Errorf("a walk that stopped at its 1000-entry cap never says so in the output the model reads:\n%s", lastLines(out))
	}
	if !strings.Contains(out, "more than this") {
		t.Errorf("the listing does not tell the model the tree is larger than what was walked:\n%s", lastLines(out))
	}
	// The old footer stated the cap as the size of the tree.
	if strings.Contains(out, "Total:") {
		t.Errorf("a listing that stopped early still reports a total, which it cannot know:\n%s", lastLines(out))
	}
}

// Control: a directory small enough to be walked and shown in full must carry
// no disclosure at all. This is what separates the fix from a footer that
// fires on every listing.
func TestASmallDirectoryIsListedWholeWithNoDisclosure(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	for _, recursive := range []bool{false, true} {
		out := listFiles(t, root, recursive)
		for _, banner := range []string{"more items", "Total:", "stopped after"} {
			if strings.Contains(out, banner) {
				t.Errorf("recursive=%v: a 3-entry directory was reported with %q:\n%s", recursive, banner, out)
			}
		}
		if !strings.Contains(out, "a.go") || !strings.Contains(out, "c.go") {
			t.Errorf("recursive=%v: entries missing from a listing that fits whole:\n%s", recursive, out)
		}
	}
}

// Control: the non-recursive branch reads one directory with os.ReadDir, so it
// always knows the true total and must keep reporting it.
func TestANonRecursiveListingStillReportsATrueTotal(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 60; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	out := listFiles(t, root, false)
	if !strings.Contains(out, "Total: 60 items") {
		t.Errorf("a fully-enumerated directory of 60 entries did not report its real total:\n%s", lastLines(out))
	}
	if !strings.Contains(out, "[...10 more items...]") {
		t.Errorf("60 entries shown 50 at a time should disclose 10 omitted:\n%s", lastLines(out))
	}
}

func lastLines(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, "\n")
}
