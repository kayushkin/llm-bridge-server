package memorystore

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/memory-store/internal/textutil"
)

// ToolMetadata represents metadata about an available tool
type ToolMetadata struct {
	Name        string
	Description string
	Category    string // "filesystem", "code-introspection", "search", etc.
}

// LoadToolRegistry creates a memory entry describing available tools.
// This makes the agent aware of what capabilities it has.
func (s *Store) LoadToolRegistry(tools []ToolMetadata) error {
	if len(tools) == 0 {
		return nil
	}

	// Group tools by category
	categories := make(map[string][]ToolMetadata)
	for _, tool := range tools {
		cat := tool.Category
		if cat == "" {
			cat = "general"
		}
		categories[cat] = append(categories[cat], tool)
	}

	// Build content
	var builder strings.Builder
	builder.WriteString("You have access to these tools:\n\n")

	// Sort categories for deterministic output (cache stability)
	catNames := make([]string, 0, len(categories))
	for cat := range categories {
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)

	for _, category := range catNames {
		categoryTools := categories[category]
		builder.WriteString(fmt.Sprintf("## %s\n\n", textutil.TitleFirstRuneOfEachWord(category)))
		for _, tool := range categoryTools {
			builder.WriteString(fmt.Sprintf("- **%s**: %s\n", tool.Name, tool.Description))
		}
		builder.WriteString("\n")
	}

	builder.WriteString("Important guidelines:\n")
	builder.WriteString("- Use `repo_map()` to understand codebase structure before reading files\n")
	builder.WriteString("- Use `recent_files()` to see what's been worked on recently\n")
	builder.WriteString("- Use `read_files()` to get full file contents only when needed\n")
	builder.WriteString("- Tools generate fresh data on-demand - don't rely on stale context\n")

	// Save as always-load memory
	return s.Save(Memory{
		ID:         "tool-registry",
		Content:    builder.String(),
		Tags:       []string{"tools", "capabilities", "system"},
		Importance: 0.9, // High importance so it's always loaded
		AlwaysLoad: true,
		Source:     "system",
	})
}

// UpdateToolUsageSummary saves a summary of a tool call result as ephemeral memory.
// This helps the agent remember what it learned without re-calling tools.
func (s *Store) UpdateToolUsageSummary(toolName, summary string, ttlSeconds int64) error {
	expiresAt := time.Now().Add(time.Duration(ttlSeconds) * time.Second)

	return s.Save(Memory{
		ID:         fmt.Sprintf("tool-usage:%s", toolName),
		Content:    fmt.Sprintf("[%s] %s", toolName, summary),
		Tags:       []string{"tool-usage", "cache", toolName},
		Importance: 0.5, // Medium importance
		Source:     "tool",
		ExpiresAt:  &expiresAt,
	})
}