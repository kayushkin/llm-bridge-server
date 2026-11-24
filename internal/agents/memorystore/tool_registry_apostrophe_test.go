package memorystore

import (
	"strings"
	"testing"
)

// TestToolRegistryHeadingKeepsAnApostropheInsideAWord covers tool_registry.go:47.
//
// ToolMetadata.Category is a free-form string supplied by whoever registers the
// tool — its own doc comment ends "etc." — so unlike the tier and priority-word
// call sites in inber-party there is no vocabulary here that rules an
// apostrophe out.
func TestToolRegistryHeadingKeepsAnApostropheInsideAWord(t *testing.T) {
	s := newTestStore(t)
	tools := []ToolMetadata{
		{Name: "recall", Description: "recall a fact", Category: "agent's memory"},
	}
	if err := s.LoadToolRegistry(tools); err != nil {
		t.Fatalf("LoadToolRegistry failed: %v", err)
	}
	m, err := s.Get("tool-registry")
	if err != nil {
		t.Fatalf("Get(tool-registry) failed: %v", err)
	}

	if strings.Contains(m.Content, "Agent'S") {
		t.Errorf("heading rendered as \"Agent'S\" — the apostrophe was treated as a word break:\n%s", m.Content)
	}
	if !strings.Contains(m.Content, "## Agent's Memory") {
		t.Errorf("expected heading %q in content:\n%s", "## Agent's Memory", m.Content)
	}
}

// TestToolRegistryHeadingStillBreaksOnAHyphen is the paired negative. The
// replacement was chosen over a plain first-rune capitaliser precisely because
// it preserves this rendering, which TestLoadToolRegistryFormatting already
// pins for "code-introspection". Asserting it here as well means a future
// swap to a simpler helper fails a test that says why rather than one that
// looks like an incidental expectation.
func TestToolRegistryHeadingStillBreaksOnAHyphen(t *testing.T) {
	s := newTestStore(t)
	tools := []ToolMetadata{
		{Name: "repo_map", Description: "map the repo", Category: "code-introspection"},
	}
	if err := s.LoadToolRegistry(tools); err != nil {
		t.Fatalf("LoadToolRegistry failed: %v", err)
	}
	m, err := s.Get("tool-registry")
	if err != nil {
		t.Fatalf("Get(tool-registry) failed: %v", err)
	}

	if !strings.Contains(m.Content, "## Code-Introspection") {
		t.Errorf("expected heading %q in content:\n%s", "## Code-Introspection", m.Content)
	}
}
