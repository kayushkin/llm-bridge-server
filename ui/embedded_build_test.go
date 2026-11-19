package ui

import (
	"io/fs"
	"regexp"
	"testing"
)

// TestBuiltFrontEndNamesFilesTheBuildHolds checks the three things that must
// line up for the server to serve the front end: scripts/build-ui.sh writes
// where this package embeds, the build was made for /ui/, and every file its
// index.html asks for is in the build. It has nothing to check in a clone
// where the front end has not been built.
func TestBuiltFrontEndNamesFilesTheBuildHolds(t *testing.T) {
	build := Build()
	index, err := fs.ReadFile(build, "index.html")
	if err != nil {
		t.Skipf("the front end is not built here (run scripts/build-ui.sh): %v", err)
	}
	named := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllSubmatch(index, -1)
	if len(named) == 0 {
		t.Fatalf("index.html names no script or stylesheet: %s", index)
	}
	servedUnder := regexp.MustCompile(`^/ui/(.+)$`)
	for _, match := range named {
		address := string(match[1])
		inBuild := servedUnder.FindStringSubmatch(address)
		if inBuild == nil {
			t.Errorf("index.html names %q, which is not under /ui/: the build was not made with --base /ui/", address)
			continue
		}
		if _, err := fs.Stat(build, inBuild[1]); err != nil {
			t.Errorf("index.html names %q, which the build does not hold: %v", address, err)
		}
	}
}
