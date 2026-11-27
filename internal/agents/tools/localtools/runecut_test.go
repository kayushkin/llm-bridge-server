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
	"unicode/utf8"
)

// Every tool in this file cuts a string down to a byte budget. Cutting at a
// fixed byte offset splits the rune straddling it, and nothing reports the
// result: encoding/json substitutes U+FFFD rather than failing, so the model
// receives a replacement character and no error is raised anywhere.
//
// The tests slide a four-byte rune across each cut rather than picking one
// string. A hand-picked string lands off the boundary and passes against the
// unfixed code, which is how this survived here. The offsets where the rune
// sits clear of the cut are the known-negative control: they pass either way.
const musicalSymbolGClef = "\U0001D11E" // 𝄞, four bytes

// straddlingLeads returns the pad lengths that put a four-byte rune across a
// cut at budget, plus one aligned lead either side as the control.
func straddlingLeads(budget int) []int {
	return []int{budget - 4, budget - 3, budget - 2, budget - 1, budget}
}

func TestWebFetchDoesNotSplitARuneAtTheCharacterCap(t *testing.T) {
	const maxChars = 64
	for _, lead := range straddlingLeads(maxChars) {
		body := strings.Repeat("a", lead) + musicalSymbolGClef + strings.Repeat("b", 200)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, body)
		}))

		in, err := json.Marshal(map[string]any{"url": server.URL, "max_chars": maxChars})
		if err != nil {
			t.Fatal(err)
		}
		out, err := WebFetch().Run(context.Background(), string(in))
		server.Close()
		if err != nil {
			t.Fatalf("lead=%d: fetch failed: %v", lead, err)
		}

		if !utf8.ValidString(out) {
			t.Errorf("lead=%d: fetched text is not valid UTF-8: %q", lead, out)
		}
		// Falsifiability: a cut that gave up everything would also be valid.
		if want := maxChars - 3; len(out) < want {
			t.Errorf("lead=%d: kept %d bytes, lost more than the straddling rune is wide", lead, len(out))
		}
	}
}

func TestReadFileDoesNotSplitARuneAtTheByteCap(t *testing.T) {
	for _, lead := range straddlingLeads(maxWholeFileReadBytes) {
		// One long line, so the byte cap is the only limit that fires and the
		// line-window truncation cannot mask what it did.
		body := strings.Repeat("a", lead) + musicalSymbolGClef + strings.Repeat("b", 5000)
		path := filepath.Join(t.TempDir(), "big.txt")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		out := readSingleFile(path, 0, 0)
		if !utf8.ValidString(out) {
			t.Errorf("lead=%d: file content is not valid UTF-8", lead)
		}

		// The banner reports how much was read. Walking the cut back changes
		// that number, so it has to be the kept length and not the cap.
		kept := maxWholeFileReadBytes
		if lead < maxWholeFileReadBytes && lead+4 > maxWholeFileReadBytes {
			kept = lead
		}
		want := fmt.Sprintf("[partial read — %d of %d bytes", kept, len(body))
		if !strings.Contains(out, want) {
			t.Errorf("lead=%d: banner does not report the bytes actually kept, want %q", lead, want)
		}
	}
}

func TestShellOutputTruncationDoesNotSplitARune(t *testing.T) {
	// truncateShellOutput cuts a head and a tail. They walk opposite ways, so
	// both need the rune slid across them.
	const (
		headChars = 25000
		tailChars = 20000
		filler    = 60000
	)

	t.Run("head cut", func(t *testing.T) {
		for _, lead := range straddlingLeads(headChars) {
			s := strings.Repeat("a", lead) + musicalSymbolGClef + strings.Repeat("b", filler)
			assertShellOutputIntact(t, lead, s)
		}
	})

	t.Run("tail cut", func(t *testing.T) {
		for _, trail := range straddlingLeads(tailChars) {
			s := strings.Repeat("a", filler) + musicalSymbolGClef + strings.Repeat("b", trail)
			assertShellOutputIntact(t, trail, s)
		}
	})
}

func assertShellOutputIntact(t *testing.T, offset int, s string) {
	t.Helper()
	out := truncateShellOutput(s)
	if !utf8.ValidString(out) {
		t.Errorf("offset=%d: truncated shell output is not valid UTF-8", offset)
	}
	if len(out) >= len(s) {
		t.Fatalf("offset=%d: input was not truncated at all, so the test proves nothing", offset)
	}
	// The banner states how many CHARACTERS were dropped, and it is derived
	// from the two halves kept, so a cut that moved has to move the figure with
	// it. Account in characters, which is the unit the banner names: a byte
	// identity here would hold only while the code counted bytes, and it would
	// then be pinning the overstatement the character count exists to fix.
	var omitted int
	if _, err := fmt.Sscanf(out[strings.Index(out, "[..."):], "[... %d characters omitted ...]", &omitted); err != nil {
		t.Fatalf("offset=%d: no omitted-characters banner in output: %v", offset, err)
	}
	head, tail, found := strings.Cut(out, "\n\n[... ")
	if !found {
		t.Fatalf("offset=%d: output has no truncation marker", offset)
	}
	_, keptTail, _ := strings.Cut(tail, " ...]\n\n")
	got := utf8.RuneCountInString(head) + utf8.RuneCountInString(keptTail) + omitted
	if want := utf8.RuneCountInString(s); got != want {
		t.Errorf("offset=%d: head+tail+omitted = %d characters, but the input was %d characters",
			offset, got, want)
	}
}

func TestSchedulerRunOutputTruncationDoesNotSplitARune(t *testing.T) {
	const budget = 500
	for _, lead := range straddlingLeads(budget) {
		output := strings.Repeat("a", lead) + musicalSymbolGClef + strings.Repeat("b", 200)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"id":1,"status":"success","started_at":"2026-08-08T00:00:00Z","output":%s}]`,
				mustJSONString(t, output))
		}))

		out, err := handleRuns(context.Background(), server.URL, http.DefaultClient, 1)
		server.Close()
		if err != nil {
			t.Fatalf("lead=%d: runs failed: %v", lead, err)
		}
		if !utf8.ValidString(out) {
			t.Errorf("lead=%d: run output is not valid UTF-8: %q", lead, out)
		}
	}
}

func TestBuildErrorTaskDoesNotSplitARune(t *testing.T) {
	const budget = 500
	for _, lead := range straddlingLeads(budget) {
		repoRoot := t.TempDir()
		buildOutput := strings.Repeat("a", lead) + musicalSymbolGClef + strings.Repeat("b", 200)
		if err := AddBuildErrorTask(repoRoot, buildOutput); err != nil {
			t.Fatalf("lead=%d: %v", lead, err)
		}

		// The plan is written to disk, so a split rune survives the process
		// that made it. Read it back rather than trusting the in-memory value.
		written, err := os.ReadFile(filepath.Join(repoRoot, taskPlanFile))
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.Valid(written) {
			t.Errorf("lead=%d: plan file on disk is not valid UTF-8", lead)
		}
	}
}

// runBuild cuts at its own budget, and it is a different cut from the one
// AddBuildErrorTask makes. Sabotage scored this site UNNOTICED until the test
// below existed: the neighboring cut was pinned and this one only looked it.
func TestBuildOutputDoesNotSplitARune(t *testing.T) {
	const budget = 2000
	for _, lead := range straddlingLeads(budget) {
		repoRoot := t.TempDir()
		payload := strings.Repeat("a", lead) + musicalSymbolGClef + strings.Repeat("b", 500)
		if err := os.WriteFile(filepath.Join(repoRoot, "payload.txt"), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}

		restore := TaskPlanBuildCommand
		TaskPlanBuildCommand = "cat payload.txt"
		result := runBuild(context.Background(), repoRoot)
		TaskPlanBuildCommand = restore

		if !result.Success {
			t.Fatalf("lead=%d: build did not run: %q", lead, result.Output)
		}
		if !utf8.ValidString(result.Output) {
			t.Errorf("lead=%d: build output is not valid UTF-8", lead)
		}
		if want := budget - 3; len(result.Output) < want {
			t.Errorf("lead=%d: kept %d bytes, lost more than the straddling rune is wide",
				lead, len(result.Output))
		}
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
