package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunScratchpadNoteWritesTheNoteToDisk(t *testing.T) {
	root := t.TempDir()

	out, err := runScratchpad(root, "brigid", `{"action":"note","key":"API endpoints","value":"GET /tools"}`)
	if err != nil {
		t.Fatalf("runScratchpad: %v", err)
	}
	if !strings.Contains(out, "1 note(s)") {
		t.Errorf("summary = %q, want it to report 1 note", out)
	}

	notes := loadScratchpad(root, "brigid")
	if notes["API endpoints"] != "GET /tools" {
		t.Errorf("notes = %v, want the note round-tripped through the file", notes)
	}
}

// Each agent gets its own file, so one agent's note must not appear in another's.
func TestRunScratchpadKeepsEachAgentsNotesSeparate(t *testing.T) {
	root := t.TempDir()

	if _, err := runScratchpad(root, "brigid", `{"action":"note","key":"k","value":"brigid's"}`); err != nil {
		t.Fatalf("brigid note: %v", err)
	}
	if _, err := runScratchpad(root, "lugh", `{"action":"note","key":"k","value":"lugh's"}`); err != nil {
		t.Fatalf("lugh note: %v", err)
	}

	if got := loadScratchpad(root, "brigid")["k"]; got != "brigid's" {
		t.Errorf("brigid's note = %q, want %q — another agent overwrote it", got, "brigid's")
	}
	if got := loadScratchpad(root, "lugh")["k"]; got != "lugh's" {
		t.Errorf("lugh's note = %q, want %q", got, "lugh's")
	}
}

func TestRunScratchpadNoteUpsertsAnExistingKey(t *testing.T) {
	root := t.TempDir()

	if _, err := runScratchpad(root, "brigid", `{"action":"note","key":"k","value":"first"}`); err != nil {
		t.Fatalf("first note: %v", err)
	}
	out, err := runScratchpad(root, "brigid", `{"action":"note","key":"k","value":"second"}`)
	if err != nil {
		t.Fatalf("second note: %v", err)
	}

	if !strings.Contains(out, "1 note(s)") {
		t.Errorf("summary = %q, want 1 note — an upsert must not add a row", out)
	}
	if got := loadScratchpad(root, "brigid")["k"]; got != "second" {
		t.Errorf("note = %q, want %q", got, "second")
	}
}

func TestRunScratchpadRemoveDropsOnlyTheNamedKey(t *testing.T) {
	root := t.TempDir()
	for _, k := range []string{"keep", "drop"} {
		if _, err := runScratchpad(root, "brigid", `{"action":"note","key":"`+k+`","value":"v"}`); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	out, err := runScratchpad(root, "brigid", `{"action":"remove","key":"drop"}`)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(out, "1 note(s)") {
		t.Errorf("summary = %q, want 1 note left", out)
	}

	notes := loadScratchpad(root, "brigid")
	if _, still := notes["drop"]; still {
		t.Error("removed key is still present")
	}
	if _, gone := notes["keep"]; !gone {
		t.Error("remove took the wrong key with it")
	}
}

func TestRunScratchpadClearEmptiesTheNotes(t *testing.T) {
	root := t.TempDir()
	if _, err := runScratchpad(root, "brigid", `{"action":"note","key":"k","value":"v"}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := runScratchpad(root, "brigid", `{"action":"clear"}`)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !strings.Contains(out, "0 note(s)") {
		t.Errorf("summary = %q, want 0 notes", out)
	}
	if got := loadScratchpad(root, "brigid"); len(got) != 0 {
		t.Errorf("notes = %v, want empty", got)
	}
}

// A missing key is reported to the model as text rather than as a Go error, so
// the distinction is worth pinning: an error would end the turn, this does not.
func TestRunScratchpadReportsAMissingKeyWithoutFailingTheCall(t *testing.T) {
	for _, action := range []string{"note", "remove"} {
		root := t.TempDir()
		out, err := runScratchpad(root, "brigid", `{"action":"`+action+`"}`)
		if err != nil {
			t.Errorf("%s with no key: err = %v, want the refusal in the output instead", action, err)
		}
		if !strings.Contains(out, "'key' required") {
			t.Errorf("%s with no key: out = %q, want it to name the missing key", action, out)
		}
		if _, statErr := os.Stat(scratchpadPath(root, "brigid")); statErr == nil {
			t.Errorf("%s with no key: wrote a scratchpad file anyway", action)
		}
	}
}

func TestRunScratchpadReportsAnUnknownAction(t *testing.T) {
	root := t.TempDir()

	out, err := runScratchpad(root, "brigid", `{"action":"append"}`)
	if err != nil {
		t.Fatalf("unknown action: err = %v, want the refusal in the output instead", err)
	}
	if !strings.Contains(out, `unknown action "append"`) {
		t.Errorf("out = %q, want it to name the action it refused", out)
	}
}

func TestRunScratchpadRejectsMalformedInput(t *testing.T) {
	root := t.TempDir()

	if _, err := runScratchpad(root, "brigid", `{"action":`); err == nil {
		t.Fatal("malformed JSON was accepted, want an error")
	}
}

func TestSaveScratchpadCreatesTheDirectoryAndItsGitignore(t *testing.T) {
	root := t.TempDir()

	if err := saveScratchpad(root, "brigid", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("saveScratchpad: %v", err)
	}

	// The scratchpad holds an agent's private working notes inside a user's
	// repo, so the ignore file is the reason those notes never get committed.
	gitignore := filepath.Join(root, scratchpadDir, ".gitignore")
	data, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if strings.TrimSpace(string(data)) != "*" {
		t.Errorf(".gitignore = %q, want it to ignore everything", data)
	}
}

// The ignore file is only written when absent, so a user who narrowed it must
// keep their version across the next save.
func TestSaveScratchpadKeepsAnExistingGitignore(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, scratchpadDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	custom := "*.md\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(custom), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := saveScratchpad(root, "brigid", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("saveScratchpad: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(data) != custom {
		t.Errorf(".gitignore = %q, want the user's %q left alone", data, custom)
	}
}

func TestSaveScratchpadWritesMarkdownThatLoadsBack(t *testing.T) {
	root := t.TempDir()
	want := map[string]string{
		"API endpoints": "GET /tools\nPOST /provision",
		"gotcha":        "the seed runs on every boot",
	}

	if err := saveScratchpad(root, "brigid", want); err != nil {
		t.Fatalf("saveScratchpad: %v", err)
	}

	raw, err := os.ReadFile(scratchpadPath(root, "brigid"))
	if err != nil {
		t.Fatalf("read scratchpad: %v", err)
	}
	if !strings.Contains(string(raw), "# Scratchpad (brigid)") {
		t.Errorf("file does not carry the agent's own heading:\n%s", raw)
	}

	got := loadScratchpad(root, "brigid")
	if len(got) != len(want) {
		t.Fatalf("loaded %d notes, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("note %q = %q, want %q — multi-line values must survive the round trip", k, got[k], v)
		}
	}
}
