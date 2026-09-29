package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

func toolCallEvent(t *testing.T, toolID string, input map[string]any) harness.ToolEventInOrder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return harness.ToolEventInOrder{Call: &msg.ToolCallEvent{ToolID: toolID, Name: "Bash", Input: raw}}
}

func toolResultEvent(toolID, output string) harness.ToolEventInOrder {
	return harness.ToolEventInOrder{Result: &msg.ToolResultEvent{ToolID: toolID, Output: output}}
}

// The shape of br_1790637091562322466: the agent read files only through Bash,
// after a `cd`, and named them in its reply relative to that directory.
func TestPathsNamedByToolEventsFollowsCdAcrossCallsAndReadsGrepOutput(t *testing.T) {
	events := []harness.ToolEventInOrder{
		toolCallEvent(t, "a", map[string]any{"command": `cd ~/repos/llm-bridge-server && grep -rln "rename" --include=*.go .`}),
		toolResultEvent("a", "internal/server/renamer.go\n./internal/server/sessions.go:707:  s.maybeAutoRename(id)\n"),
		// No cd: the directory persists from the call before.
		toolCallEvent(t, "b", map[string]any{"command": `sed -n 600,712p internal/server/sessions.go; cat "README.md" > /tmp/out.txt`}),
		toolCallEvent(t, "c", map[string]any{"file_path": "/home/someone/repos/dash/web/src/App.tsx"}),
		// codex: argv with a script, and its own workdir that does not move the shared one.
		toolCallEvent(t, "d", map[string]any{"command": []any{"bash", "-lc", "cat main.go"}, "workdir": "/srv/other"}),
		toolCallEvent(t, "e", map[string]any{"command": `cat $HOME/secret.txt *.go -n config.Load`}),
	}
	got := pathsNamedByToolEvents(events, "/home/someone/repos", "/home/someone")
	want := []string{
		"/home/someone/repos/llm-bridge-server/internal/server/renamer.go",
		"/home/someone/repos/llm-bridge-server/internal/server/sessions.go",
		"/home/someone/repos/llm-bridge-server/README.md",
		"/tmp/out.txt",
		"/home/someone/repos/dash/web/src/App.tsx",
		"/srv/other/main.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths\n got %q\nwant %q", got, want)
	}
}

func TestLooksLikeFilePath(t *testing.T) {
	for token, want := range map[string]bool{
		"sessions.go":                true,
		"Chat.module.css":            true,
		"internal/server/renamer.go": true,
		"~/repos":                    true,
		"1.2":                        false,
		"-n":                         false,
		"https://example.com/a.go":   false,
		"*.go":                       false,
		"KEY=value.txt":              false,
		"":                           false,
	} {
		if got := looksLikeFilePath(token); got != want {
			t.Errorf("looksLikeFilePath(%q) = %v, want %v", token, got, want)
		}
	}
}

func TestPathIsDenied(t *testing.T) {
	denied := []string{"~/.config/secrets", "/etc/**", "/var/*.key"}
	for path, want := range map[string]bool{
		"/home/someone/.config/secrets/token": true,
		"/home/someone/.config/secretsfile":   false,
		"/etc/passwd":                         true,
		"/var/server.key":                     true,
		"/var/server.pem":                     false,
	} {
		if got := pathIsDenied(path, denied, "/home/someone"); got != want {
			t.Errorf("pathIsDenied(%q) = %v, want %v", path, got, want)
		}
	}
}
