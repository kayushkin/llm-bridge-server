package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHandlerBuildsAMethodNameOutOfACompactSummary walks this package's own
// source. The defect it guards against is not something a unit test on the
// handler can see: handleCompactSession talks to a concrete *harness.Manager,
// so there is no seam to assert the method string through, and that is exactly
// why "compact:<summary>" survived — no test could have caught it.
//
// Every harness switches on the exact string "compact". Anything that appends
// to that name produces a request the whole fleet answers with "unknown
// method", on a goroutine, after the HTTP handler has already returned 200.
func TestNoHandlerBuildsAMethodNameOutOfACompactSummary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for lineNumber, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, `"compact:"`) {
				t.Errorf("%s:%d builds a compact method name with a payload glued to it — the summary belongs in the params, see harness.BuildCompactParams:\n\t%s",
					name, lineNumber+1, strings.TrimSpace(line))
			}
		}
	}
}
