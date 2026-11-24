package agentstore

import (
	"strings"
)

// PromptMarkdownSection is one heading-delimited piece of a prompt file, as
// text only: no ids, no tags. Heading is the whole heading line ("## Scheduler"),
// empty for text that comes before the first heading. Body has no leading or
// trailing blank lines.
type PromptMarkdownSection struct {
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

// promptSectionDeepestHeadingLevel is the deepest heading that starts a new
// section. Deeper headings stay inside the section that contains them.
const promptSectionDeepestHeadingLevel = 2

// SplitPromptMarkdown cuts a prompt file into sections at level-1 and level-2
// headings. A line inside a fenced code block is never a heading: shell
// comments start with "# " too, and the first version of this splitter cut
// every code sample in two at them.
func SplitPromptMarkdown(content string) []PromptMarkdownSection {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")

	var sections []PromptMarkdownSection
	currentHeading := ""
	var currentLines []string
	flush := func() {
		body := strings.Trim(strings.Join(currentLines, "\n"), "\n")
		body = trimTrailingSpaceLines(body)
		if currentHeading == "" && strings.TrimSpace(body) == "" {
			return
		}
		sections = append(sections, PromptMarkdownSection{Heading: currentHeading, Body: body})
	}

	openFence := ""
	for _, line := range lines {
		if fence := codeFenceMarker(line); fence != "" {
			switch {
			case openFence == "":
				openFence = fence
			case fence[0] == openFence[0] && len(fence) >= len(openFence) && strings.TrimSpace(line) == fence:
				openFence = ""
			}
			currentLines = append(currentLines, line)
			continue
		}
		if openFence == "" && isPromptSectionHeading(line) {
			flush()
			currentHeading = strings.TrimRight(line, " \t")
			currentLines = nil
			continue
		}
		currentLines = append(currentLines, line)
	}
	flush()
	return sections
}

// codeFenceMarker returns the run of backticks or tildes that opens or closes
// a fenced block on this line, or "" when the line is not a fence.
func codeFenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || trimmed == "" {
		return ""
	}
	marker := trimmed[0]
	if marker != '`' && marker != '~' {
		return ""
	}
	run := 0
	for run < len(trimmed) && trimmed[run] == marker {
		run++
	}
	if run < 3 {
		return ""
	}
	return trimmed[:run]
}

func isPromptSectionHeading(line string) bool {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > promptSectionDeepestHeadingLevel {
		return false
	}
	return level < len(line) && line[level] == ' '
}

func trimTrailingSpaceLines(body string) string {
	for {
		trimmed := strings.TrimRight(body, " \t")
		trimmed = strings.TrimRight(trimmed, "\n")
		if trimmed == body {
			return body
		}
		body = trimmed
	}
}

// PromptSectionTitleFromHeading is the display title for a heading line.
func PromptSectionTitleFromHeading(heading string) string {
	title := strings.TrimSpace(strings.TrimLeft(heading, "#"))
	if title == "" {
		return "Preamble"
	}
	return title
}

// RenderPromptMarkdown is the one way sections become a file: heading, blank
// line, body, sections separated by a blank line, one newline at the end.
// Splitting the result gives back the same sections, which is what lets drift
// be judged by comparing section lists instead of bytes.
func RenderPromptMarkdown(sections []PromptMarkdownSection) string {
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		heading := strings.TrimSpace(section.Heading)
		body := strings.Trim(section.Body, "\n")
		switch {
		case heading != "" && body != "":
			parts = append(parts, heading+"\n\n"+body)
		case heading != "":
			parts = append(parts, heading)
		case body != "":
			parts = append(parts, body)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// collapseWhitespace reduces every run of whitespace to one space. Two texts
// that are equal under it differ only in spacing and blank lines.
func collapseWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// PromptMarkdownEditKind names what happened to one section between two
// versions of a file.
const (
	PromptMarkdownEditUpdate = "update"
	PromptMarkdownEditInsert = "insert"
	PromptMarkdownEditDelete = "delete"
)

// PromptMarkdownEdit is one step of the difference between a base section list
// and a changed one. BaseIndex points into the base list (update, delete),
// ChangedIndex into the changed list (update, insert). For an insert,
// AfterBaseIndex is the base section it follows, -1 for the top of the file.
type PromptMarkdownEdit struct {
	Kind           string
	BaseIndex      int
	ChangedIndex   int
	AfterBaseIndex int
}

// DiffPromptMarkdownSections aligns two section lists on their headings
// (longest common subsequence, so repeated headings and reordering do not
// confuse it) and reports the sections that were edited, added and removed.
// A removed section and an added one with the very same body are reported as
// one update: that is a renamed heading, and treating it as delete-plus-insert
// would throw away the section's id, tags and history.
func DiffPromptMarkdownSections(base, changed []PromptMarkdownSection) []PromptMarkdownEdit {
	n, m := len(base), len(changed)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if base[i].Heading == changed[j].Heading {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var edits []PromptMarkdownEdit
	lastBaseIndex := -1
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && base[i].Heading == changed[j].Heading:
			if base[i].Body != changed[j].Body {
				edits = append(edits, PromptMarkdownEdit{Kind: PromptMarkdownEditUpdate, BaseIndex: i, ChangedIndex: j, AfterBaseIndex: -1})
			}
			lastBaseIndex = i
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			edits = append(edits, PromptMarkdownEdit{Kind: PromptMarkdownEditInsert, BaseIndex: -1, ChangedIndex: j, AfterBaseIndex: lastBaseIndex})
			j++
		default:
			edits = append(edits, PromptMarkdownEdit{Kind: PromptMarkdownEditDelete, BaseIndex: i, ChangedIndex: -1, AfterBaseIndex: -1})
			lastBaseIndex = i
			i++
		}
	}
	return pairRenamedHeadings(base, changed, edits)
}

func pairRenamedHeadings(base, changed []PromptMarkdownSection, edits []PromptMarkdownEdit) []PromptMarkdownEdit {
	out := make([]PromptMarkdownEdit, 0, len(edits))
	// Pair each delete with an unclaimed insert carrying the same body, then
	// rebuild the list in its original order: a paired delete becomes an update
	// and the insert it was paired with is dropped.
	pairedChangedIndexByBase := map[int]int{}
	consumedInsert := map[int]bool{}
	for _, edit := range edits {
		if edit.Kind != PromptMarkdownEditDelete {
			continue
		}
		for insertPosition, candidate := range edits {
			if candidate.Kind != PromptMarkdownEditInsert || consumedInsert[insertPosition] {
				continue
			}
			if base[edit.BaseIndex].Body != "" && base[edit.BaseIndex].Body == changed[candidate.ChangedIndex].Body {
				consumedInsert[insertPosition] = true
				pairedChangedIndexByBase[edit.BaseIndex] = candidate.ChangedIndex
				break
			}
		}
	}
	for position, edit := range edits {
		switch {
		case edit.Kind == PromptMarkdownEditInsert && consumedInsert[position]:
			continue
		case edit.Kind == PromptMarkdownEditDelete:
			if changedIndex, paired := pairedChangedIndexByBase[edit.BaseIndex]; paired {
				out = append(out, PromptMarkdownEdit{Kind: PromptMarkdownEditUpdate, BaseIndex: edit.BaseIndex, ChangedIndex: changedIndex, AfterBaseIndex: -1})
				continue
			}
		}
		out = append(out, edit)
	}
	return out
}
