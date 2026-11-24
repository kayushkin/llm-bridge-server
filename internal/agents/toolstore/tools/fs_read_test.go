package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The footer a read appends is a claim other services act on. inber's read
// cache (agent/read_cache.go) parses "[complete file — N lines]" and then
// answers every later read of that path — full or ranged — with a stub, so a
// read that says "complete" when it truncated locks the rest of the file out
// of the session. These tests pin the footer to what the read actually
// returned.
const completeFooterPrefix = "[complete file"

func readFileTool(t *testing.T, in map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReadFile().Run(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	return out
}

func writeTempFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func numberedLines(count int, filler string) string {
	var b strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, filler)
	}
	return b.String()
}

func TestReadCountsLinesNotSplitFragments(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// strings.Split on a trailing newline yields an empty final element.
		// It is not a line: nothing can read, edit, or address it by number.
		{"trailing newline", "a\nb\nc\n", "[complete file — 3 lines]"},
		{"no trailing newline", "a\nb\nc", "[complete file — 3 lines]"},
		{"one line", "solo\n", "[complete file — 1 lines]"},
		{"empty file", "", "[complete file — 0 lines]"},
		{"only a newline", "\n", "[complete file — 1 lines]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := readFileTool(t, map[string]any{"path": writeTempFile(t, "f.txt", tc.body)})
			if !strings.Contains(out, tc.want) {
				t.Errorf("footer does not say %q\ngot tail: %q", tc.want, tail(out))
			}
		})
	}
}

func TestReadReportsByteCapAsPartial(t *testing.T) {
	// A minified bundle: one line, far past the byte cap, and short enough in
	// lines that the line window never fires. Deriving completeness from a
	// line count cannot see this cut at all.
	body := strings.Repeat("x", 500_000)
	out := readFileTool(t, map[string]any{"path": writeTempFile(t, "bundle.min.js", body)})

	if strings.Contains(out, completeFooterPrefix) {
		t.Errorf("byte-truncated read claims to be complete\ntail: %q", tail(out))
	}
	want := fmt.Sprintf("[partial read — %d of %d bytes of a 1-line file.", maxWholeFileReadBytes, len(body))
	if !strings.Contains(out, want) {
		t.Errorf("footer does not report the byte cut as %q\ntail: %q", want, tail(out))
	}
	if !strings.Contains(out, "Use offset/limit") {
		t.Errorf("truncated read does not tell the model how to reach the rest\ntail: %q", tail(out))
	}
}

func TestReadReportsLineWindowByItsRealRange(t *testing.T) {
	// Over the 2000-line limit, so the window keeps the first 500 and the
	// last 50. The old footer called that "the first ~N lines", which named
	// a range the result does not hold.
	out := readFileTool(t, map[string]any{"path": writeTempFile(t, "big.txt", numberedLines(3000, ""))})

	if strings.Contains(out, completeFooterPrefix) {
		t.Errorf("line-truncated read claims to be complete\ntail: %q", tail(out))
	}
	if !strings.Contains(out, "[partial read — lines 1-500 and 2951-3000 of 3000.") {
		t.Errorf("footer does not report the kept window\ntail: %q", tail(out))
	}
	// The reported range has to match the bytes, at both ends and in the hole.
	for _, present := range []string{"line 1 ", "line 500 ", "line 2951 ", "line 3000 "} {
		if !strings.Contains(out, present) {
			t.Errorf("footer claims %q is present, and it is not", present)
		}
	}
	if strings.Contains(out, "line 1500 ") {
		t.Errorf("footer claims lines 501-2950 were dropped, and line 1500 is present")
	}
}

func TestReadReportsRangeAskedFor(t *testing.T) {
	path := writeTempFile(t, "f.txt", "a\nb\nc\n")

	if out := readFileTool(t, map[string]any{"path": path, "offset": 2}); !strings.Contains(out, "[showing lines 2-3 of 3 — end of file]") {
		t.Errorf("tail range misreported\ntail: %q", tail(out))
	}
	if out := readFileTool(t, map[string]any{"path": path, "offset": 2, "limit": 1}); !strings.Contains(out, "[showing lines 2-2 of 3. Use offset=3 to continue]") {
		t.Errorf("mid range misreported\ntail: %q", tail(out))
	}
	// The last line is addressable; one past it is not.
	if out := readFileTool(t, map[string]any{"path": path, "offset": 3}); !strings.Contains(out, "[showing lines 3-3 of 3 — end of file]") {
		t.Errorf("last line not addressable\ntail: %q", tail(out))
	}
	if out := readFileTool(t, map[string]any{"path": path, "offset": 4}); !strings.Contains(out, "offset 4 beyond file length (3 lines)") {
		t.Errorf("offset past the end not rejected\ngot: %q", out)
	}
}

// Complements — these hold before and after the change, so the tests above
// cannot be passing because reads stopped working.

func TestReadReturnsContentUnchanged(t *testing.T) {
	body := "package main\n\nfunc main() {}\n"
	out := readFileTool(t, map[string]any{"path": writeTempFile(t, "main.go", body)})
	if !strings.HasPrefix(out, body) {
		t.Errorf("file content altered\ngot: %q", out)
	}
}

func TestBatchReadStillReadsEachFileWhole(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"one.txt", "two.txt"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name+" body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	out := readFileTool(t, map[string]any{"paths": paths})
	for _, name := range []string{"one.txt body", "two.txt body"} {
		if !strings.Contains(out, name) {
			t.Errorf("batch read dropped %q", name)
		}
	}
}

func tail(s string) string {
	if len(s) <= 160 {
		return s
	}
	return "…" + s[len(s)-160:]
}
