package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kayushkin/tool-store/schema"
)

const scratchpadDir = ".scratchpad"

// ScratchpadTool returns a tool for agent-private working notes.
// Notes are always loaded into context and never pruned.
// Each agent gets its own file: .scratchpad/<agent>.md
func ScratchpadTool(repoRoot, agentName string) Impl {
	return Impl{
		Name: "scratchpad",
		Description: `Your private working memory — always visible to you, never pruned from context.

Use this to save discoveries that you'd otherwise forget between turns:
- File structures and key line numbers
- API contracts and function signatures
- Patterns you noticed that you'll need later

Actions:
- "note": Set a key-value note (upserts).
- "remove": Remove a note by key.
- "clear": Clear all notes.

Keep notes concise. This is a cheat sheet, not a journal.`,
		InputSchema: schema.Props([]string{"action"}, map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"note", "remove", "clear"},
				"description": "Action to perform",
			},
			"key": map[string]any{
				"type":        "string",
				"description": "Note key (e.g. 'SiChat structure', 'API endpoints')",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "Note content for 'note' action",
			},
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			return runScratchpad(repoRoot, agentName, raw)
		},
	}
}

type scratchpadInput struct {
	Action string `json:"action"`
	Key    string `json:"key,omitempty"`
	Value  string `json:"value,omitempty"`
}

func runScratchpad(repoRoot, agentName, raw string) (string, error) {
	var in scratchpadInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	notes := loadScratchpad(repoRoot, agentName)

	switch in.Action {
	case "note":
		if in.Key == "" {
			return "error: 'key' required for note action", nil
		}
		if notes == nil {
			notes = make(map[string]string)
		}
		notes[in.Key] = in.Value
	case "remove":
		if in.Key == "" {
			return "error: 'key' required for remove action", nil
		}
		delete(notes, in.Key)
	case "clear":
		notes = make(map[string]string)
	default:
		return fmt.Sprintf("error: unknown action %q", in.Action), nil
	}

	if err := saveScratchpad(repoRoot, agentName, notes); err != nil {
		return "", err
	}
	return fmt.Sprintf("Scratchpad updated. %d note(s).", len(notes)), nil
}

func scratchpadPath(repoRoot, agentName string) string {
	return filepath.Join(repoRoot, scratchpadDir, agentName+".md")
}

// LoadScratchpadNotes reads notes from the scratchpad file.
func LoadScratchpadNotes(repoRoot, agentName string) map[string]string {
	return loadScratchpad(repoRoot, agentName)
}

// SaveScratchpadNotes writes notes to the scratchpad file.
func SaveScratchpadNotes(repoRoot, agentName string, notes map[string]string) error {
	return saveScratchpad(repoRoot, agentName, notes)
}

func loadScratchpad(repoRoot, agentName string) map[string]string {
	data, err := os.ReadFile(scratchpadPath(repoRoot, agentName))
	if err != nil {
		return make(map[string]string)
	}
	return parseScratchpadMD(string(data))
}

func saveScratchpad(repoRoot, agentName string, notes map[string]string) error {
	dir := filepath.Join(repoRoot, scratchpadDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// Auto-create .gitignore in scratchpad dir
	gitignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignore); os.IsNotExist(err) {
		_ = os.WriteFile(gitignore, []byte("*\n"), 0644)
	}
	return os.WriteFile(scratchpadPath(repoRoot, agentName), []byte(renderScratchpadMD(agentName, notes)), 0644)
}

func parseScratchpadMD(content string) map[string]string {
	notes := make(map[string]string)
	lines := strings.Split(content, "\n")
	var key string
	var value []string

	flush := func() {
		if key != "" {
			notes[key] = strings.TrimSpace(strings.Join(value, "\n"))
		}
		key = ""
		value = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") {
			flush()
			key = strings.TrimPrefix(trimmed, "### ")
		} else if key != "" && trimmed != "" {
			value = append(value, trimmed)
		}
	}
	flush()
	return notes
}

func renderScratchpadMD(agentName string, notes map[string]string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Scratchpad (%s)\n\n", agentName))
	for k, v := range notes {
		b.WriteString(fmt.Sprintf("### %s\n%s\n\n", k, v))
	}
	return b.String()
}

// LoadScratchpadContext reads the scratchpad for context injection.
func LoadScratchpadContext(repoRoot, agentName string) string {
	data, err := os.ReadFile(scratchpadPath(repoRoot, agentName))
	if err != nil {
		return ""
	}
	return string(data)
}
