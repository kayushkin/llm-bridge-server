package agentstore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitPromptMarkdownIgnoresHeadingsInsideCodeFences(t *testing.T) {
	content := "intro\n\n# One\n\ntext\n\n```bash\n# List jobs\ncurl x\n```\n\n### Deep stays inside\n\nmore\n\n## Two\n\n~~~\n## not a heading\n~~~\n"
	sections := SplitPromptMarkdown(content)
	var headings []string
	for _, section := range sections {
		headings = append(headings, section.Heading)
	}
	want := []string{"", "# One", "## Two"}
	if !reflect.DeepEqual(headings, want) {
		t.Fatalf("headings = %q, want %q", headings, want)
	}
}

func TestRenderThenSplitIsStable(t *testing.T) {
	content := "pre\n\n# A\n\nbody a\n\n## B\n\n```\n# c\n```\n"
	sections := SplitPromptMarkdown(content)
	rendered := RenderPromptMarkdown(sections)
	if rendered != content {
		t.Fatalf("render = %q, want %q", rendered, content)
	}
	if !reflect.DeepEqual(SplitPromptMarkdown(rendered), sections) {
		t.Fatal("split(render(sections)) differs from sections")
	}
}

func TestDiffPromptMarkdownSections(t *testing.T) {
	base := []PromptMarkdownSection{{"# A", "a"}, {"## B", "b"}, {"## C", "c"}, {"## D", "d"}}
	changed := []PromptMarkdownSection{{"# A", "a"}, {"## B", "b edited"}, {"## New", "n"}, {"## D renamed", "d"}}
	edits := DiffPromptMarkdownSections(base, changed)
	want := []PromptMarkdownEdit{
		{Kind: PromptMarkdownEditUpdate, BaseIndex: 1, ChangedIndex: 1, AfterBaseIndex: -1},
		{Kind: PromptMarkdownEditInsert, BaseIndex: -1, ChangedIndex: 2, AfterBaseIndex: 1},
		{Kind: PromptMarkdownEditDelete, BaseIndex: 2, ChangedIndex: -1, AfterBaseIndex: -1},
		{Kind: PromptMarkdownEditUpdate, BaseIndex: 3, ChangedIndex: 3, AfterBaseIndex: -1},
	}
	if !reflect.DeepEqual(edits, want) {
		t.Fatalf("edits = %+v\nwant   %+v", edits, want)
	}
}

// TestSplitLosesNoTextFromTheLivePromptFiles runs only where the real files
// exist. It is the check the import depends on.
func TestSplitLosesNoTextFromTheLivePromptFiles(t *testing.T) {
	home := os.Getenv("AGENT_STORE_LIVE_PROMPT_HOME")
	if home == "" {
		t.Skip("set AGENT_STORE_LIVE_PROMPT_HOME to check the real prompt files")
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		data, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		sections := SplitPromptMarkdown(string(data))
		rendered := RenderPromptMarkdown(sections)
		t.Logf("%s: %d sections, byte-identical=%v", name, len(sections), rendered == string(data))
		if collapseWhitespace(rendered) != collapseWhitespace(string(data)) {
			t.Fatalf("%s: text lost or changed by split+render", name)
		}
	}
}
