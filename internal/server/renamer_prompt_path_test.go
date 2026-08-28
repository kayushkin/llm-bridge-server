package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// The renamer prompt ends with a curl command an agent is instructed to run,
// carrying two session ids. Both are caller-choosable, so this is the same
// defect as the hook-settings sites in a third shape -- and the shape is why
// it was missed: it is built with fmt.Fprintf, not fmt.Sprintf, so a sweep
// grepping for Sprintf does not see it.
//
// Instruments are shared with hook_settings_path_test.go: runGeneratedCommand
// hands the line to a real /bin/sh with a stub curl on PATH, assertOneSegment
// checks the endpoint the URL actually addresses.

// curlLineFromPrompt pulls the single curl invocation out of the prompt body.
func curlLineFromPrompt(t *testing.T, prompt string) string {
	t.Helper()
	var found string
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "curl ") {
			if found != "" {
				t.Fatalf("more than one curl line in the prompt")
			}
			found = strings.TrimSpace(line)
		}
	}
	if found == "" {
		t.Fatalf("no curl line in the prompt:\n%s", prompt)
	}
	return found
}

func TestRenamerPromptCurlRunsExactlyOneCurl(t *testing.T) {
	for _, probe := range pathProbes() {
		t.Run(probe.name, func(t *testing.T) {
			marker := t.TempDir() + "/INJECTED"
			id := strings.ReplaceAll(probe.id, "MARKER", marker)
			if !strings.Contains(probe.id, "MARKER") {
				marker = ""
			}

			prompt := buildRenamerPrompt(
				&store.Session{SessionID: id, DisplayName: "old"},
				"br_renamer", nil, "http://localhost:8160",
			)
			assertOneCurlRun(t, probe.name, curlLineFromPrompt(t, prompt), marker, id,
				"sessions", "", "auto-rename")
		})
	}
}

// The renamer's OWN id lands in the JSON body rather than the path, so it
// needs JSON encoding rather than path escaping -- and the whole body still
// needs to survive the shell as one word.
func TestRenamerPromptPayloadStaysValidJSONForAnyRenamerID(t *testing.T) {
	for _, probe := range pathProbes() {
		t.Run(probe.name, func(t *testing.T) {
			marker := t.TempDir() + "/INJECTED"
			renamerID := strings.ReplaceAll(probe.id, "MARKER", marker)
			if !strings.Contains(probe.id, "MARKER") {
				marker = ""
			}
			// A quote is the character that breaks a JSON body, and it is not
			// in the path probe set because it is inert in a URL.
			if probe.name == "well formed" {
				renamerID = `a"b`
			}

			prompt := buildRenamerPrompt(
				&store.Session{SessionID: "br_target", DisplayName: "old"},
				renamerID, nil, "http://localhost:8160",
			)
			cmd := curlLineFromPrompt(t, prompt)
			runs := runGeneratedCommand(t, cmd)
			if len(runs) != 1 {
				t.Fatalf("%s: the shell ran curl %d times, want exactly 1 (command: %q)",
					probe.name, len(runs), cmd)
			}
			if marker != "" {
				if _, err := os.Stat(marker); err == nil {
					t.Fatalf("%s: the renamer id EXECUTED -- %q exists", probe.name, marker)
				}
			}

			// Find curl's -d argument and require it to be the JSON we meant.
			argv := runs[0]
			var body string
			for i, a := range argv {
				if a == "-d" && i+1 < len(argv) {
					body = argv[i+1]
				}
			}
			if body == "" {
				t.Fatalf("%s: no -d argument reached curl: %v", probe.name, argv)
			}
			// <TITLE> is a literal placeholder the agent substitutes, so
			// stand a value in before parsing.
			var parsed map[string]any
			if err := json.Unmarshal([]byte(strings.Replace(body, "<TITLE>", "a title", 1)), &parsed); err != nil {
				t.Fatalf("%s: -d payload is not valid JSON: %v (%q)", probe.name, err, body)
			}
			if parsed["renamer_session_id"] != renamerID {
				t.Errorf("%s: renamer_session_id = %v, want %q", probe.name, parsed["renamer_session_id"], renamerID)
			}
		})
	}
}
