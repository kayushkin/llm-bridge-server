package memorystore

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := "/tmp/test_tool_registry_" + uuid.New().String() + ".db"
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})
	return store
}

func TestLoadToolRegistryEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.LoadToolRegistry(nil); err != nil {
		t.Fatalf("LoadToolRegistry(nil) returned error: %v", err)
	}
	// Nothing should have been saved.
	if _, err := s.Get("tool-registry"); err == nil {
		t.Error("expected no tool-registry memory to be saved for empty input")
	}
}

func TestLoadToolRegistryFormatting(t *testing.T) {
	s := newTestStore(t)
	tools := []ToolMetadata{
		{Name: "grep", Description: "search files", Category: "search"},
		{Name: "read_file", Description: "read a file", Category: "filesystem"},
		{Name: "repo_map", Description: "map the repo", Category: "code-introspection"},
		{Name: "uncategorized", Description: "no category"}, // should fall into "general"
	}
	if err := s.LoadToolRegistry(tools); err != nil {
		t.Fatalf("LoadToolRegistry failed: %v", err)
	}

	m, err := s.Get("tool-registry")
	if err != nil {
		t.Fatalf("Get(tool-registry) failed: %v", err)
	}

	// Memory metadata expectations.
	if !m.AlwaysLoad {
		t.Error("expected AlwaysLoad to be true")
	}
	if m.Importance < 0.9 {
		t.Errorf("expected high importance (>=0.9), got %f", m.Importance)
	}
	if m.Source != "system" {
		t.Errorf("expected source 'system', got %q", m.Source)
	}

	content := m.Content

	// Categories are title-cased headings.
	for _, heading := range []string{"## Search", "## Filesystem", "## Code-Introspection", "## General"} {
		if !strings.Contains(content, heading) {
			t.Errorf("expected heading %q in content:\n%s", heading, content)
		}
	}

	// Categories must appear in sorted order for cache stability:
	// code-introspection < filesystem < general < search.
	idxCode := strings.Index(content, "## Code-Introspection")
	idxFs := strings.Index(content, "## Filesystem")
	idxGen := strings.Index(content, "## General")
	idxSearch := strings.Index(content, "## Search")
	if !(idxCode < idxFs && idxFs < idxGen && idxGen < idxSearch) {
		t.Errorf("categories not in sorted order: code=%d fs=%d gen=%d search=%d", idxCode, idxFs, idxGen, idxSearch)
	}

	// Tool entries are rendered as bold-name bullets.
	for _, entry := range []string{
		"- **grep**: search files",
		"- **read_file**: read a file",
		"- **repo_map**: map the repo",
		"- **uncategorized**: no category",
	} {
		if !strings.Contains(content, entry) {
			t.Errorf("expected tool entry %q in content:\n%s", entry, content)
		}
	}

	// Standard tags applied.
	wantTags := map[string]bool{"tools": true, "capabilities": true, "system": true}
	for _, tag := range m.Tags {
		delete(wantTags, tag)
	}
	if len(wantTags) != 0 {
		t.Errorf("missing expected tags: %v (got %v)", wantTags, m.Tags)
	}
}

func TestLoadToolRegistryDeterministic(t *testing.T) {
	tools := []ToolMetadata{
		{Name: "b_tool", Description: "b", Category: "zeta"},
		{Name: "a_tool", Description: "a", Category: "alpha"},
		{Name: "c_tool", Description: "c", Category: "alpha"},
	}

	s1 := newTestStore(t)
	if err := s1.LoadToolRegistry(tools); err != nil {
		t.Fatalf("LoadToolRegistry failed: %v", err)
	}
	m1, err := s1.Get("tool-registry")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	s2 := newTestStore(t)
	if err := s2.LoadToolRegistry(tools); err != nil {
		t.Fatalf("LoadToolRegistry failed: %v", err)
	}
	m2, err := s2.Get("tool-registry")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if m1.Content != m2.Content {
		t.Errorf("LoadToolRegistry content not deterministic:\n--- first ---\n%s\n--- second ---\n%s", m1.Content, m2.Content)
	}

	// alpha must precede zeta.
	if strings.Index(m1.Content, "## Alpha") >= strings.Index(m1.Content, "## Zeta") {
		t.Error("expected 'alpha' category before 'zeta'")
	}
}

func TestUpdateToolUsageSummary(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateToolUsageSummary("grep", "found 3 matches in server.go", 60); err != nil {
		t.Fatalf("UpdateToolUsageSummary failed: %v", err)
	}

	m, err := s.Get("tool-usage:grep")
	if err != nil {
		t.Fatalf("Get(tool-usage:grep) failed: %v", err)
	}

	if m.Content != "[grep] found 3 matches in server.go" {
		t.Errorf("unexpected content: %q", m.Content)
	}
	if m.Source != "tool" {
		t.Errorf("expected source 'tool', got %q", m.Source)
	}
	if m.ExpiresAt == nil {
		t.Error("expected ExpiresAt to be set for ephemeral tool usage")
	}

	// Tags include the tool name plus the standard markers.
	wantTags := map[string]bool{"tool-usage": true, "cache": true, "grep": true}
	for _, tag := range m.Tags {
		delete(wantTags, tag)
	}
	if len(wantTags) != 0 {
		t.Errorf("missing expected tags: %v (got %v)", wantTags, m.Tags)
	}
}

// The guidance lines name tools by string literal, so a rename in the tool
// set does not reach them. inber's file-reading tool is read_files; the
// guidance said read_file, which names no tool, in an always-load block.
func TestLoadToolRegistryGuidanceNamesReadFiles(t *testing.T) {
	s := newTestStore(t)
	if err := s.LoadToolRegistry([]ToolMetadata{{Name: "read_files", Description: "read files", Category: "filesystem"}}); err != nil {
		t.Fatalf("LoadToolRegistry returned error: %v", err)
	}
	m, err := s.Get("tool-registry")
	if err != nil {
		t.Fatalf("Get(tool-registry) failed: %v", err)
	}
	if !strings.Contains(m.Content, "- Use `read_files()` to get full file contents only when needed\n") {
		t.Errorf("guidance does not name read_files:\n%s", m.Content)
	}
	if strings.Contains(m.Content, "`read_file()`") {
		t.Errorf("guidance still names read_file, which is not a tool:\n%s", m.Content)
	}
}
