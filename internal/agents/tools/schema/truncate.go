package schema

import (
	"fmt"
	"strings"
)

// TruncateLines smart-truncates multi-line output to keep context manageable.
// keepFirst: number of lines to keep from the start
// keepLast: number of lines to keep from the end
// Returns truncated output with a marker showing how many lines were removed.
func TruncateLines(output string, keepFirst, keepLast int) string {
	if output == "" {
		return output
	}

	lines := strings.Split(output, "\n")
	totalLines := len(lines)
	threshold := keepFirst + keepLast

	// If total lines <= threshold, return as-is
	if totalLines <= threshold {
		return output
	}

	// Build truncated output
	var result strings.Builder

	// First N lines
	for i := 0; i < keepFirst && i < totalLines; i++ {
		result.WriteString(lines[i])
		result.WriteString("\n")
	}

	// Truncation marker
	removed := totalLines - threshold
	result.WriteString(fmt.Sprintf("\n[...%d lines truncated — content NOT shown above...]\n", removed))
	result.WriteString("(⚠ Do NOT use text from this truncated view for edit_file old_text — it will fail.)\n")
	result.WriteString("(Use read_file with offset/limit to see the missing lines before editing.)\n\n")

	// Last M lines
	startLast := totalLines - keepLast
	if startLast < keepFirst {
		startLast = keepFirst
	}
	for i := startLast; i < totalLines; i++ {
		result.WriteString(lines[i])
		if i < totalLines-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}

// TruncateShellOutput truncates shell command output intelligently.
// Default: keep first 20 + last 10 lines if over 40 lines total.
func TruncateShellOutput(output string) string {
	lines := strings.Split(output, "\n")
	if len(lines) <= 40 {
		return output
	}
	return TruncateLines(output, 20, 10)
}

// Line windows used when a whole-file read is too long to return intact.
const (
	fileReadWholeFileLimit = 2000
	fileReadKeepFirst      = 500
	fileReadKeepLast       = 50
)

// FileLines splits file content into the lines a reader can actually address.
// A file that ends in a newline has that many lines, not one more:
// strings.Split leaves an empty final element that nobody can read, edit, or
// ask for by number, and counting it makes every line number reported back to
// the model one too many. Empty content is zero lines.
func FileLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

// FileReadCut records what TruncateFileRead left out. The caller needs this
// reported rather than re-derived: a line count taken over the returned text
// also counts the truncation banner, and it cannot see a cut that some other
// limit made before this one ran.
type FileReadCut struct {
	TotalLines int // lines in the content handed to TruncateFileRead
	KeptFirst  int // lines kept from the start
	KeptLast   int // lines kept from the end
}

// Truncated reports whether any line was left out.
func (c FileReadCut) Truncated() bool { return c.KeptFirst+c.KeptLast < c.TotalLines }

// TruncateFileRead truncates file read output for very large files, and
// reports what it removed.
//
// Files under 2000 lines are returned in full — truncating source files
// causes repeated re-reads that waste API turns (far more expensive than
// the extra context tokens). Truncation is for large error logs, content
// dumps, and generated files — not normal source code.
// Files over 2000 lines get truncated (first 500 + last 50).
func TruncateFileRead(content string) (string, FileReadCut) {
	total := len(FileLines(content))
	if total <= fileReadWholeFileLimit {
		return content, FileReadCut{TotalLines: total, KeptFirst: total}
	}
	// Hand TruncateLines the content without its trailing newline, so the
	// window it keeps is fileReadKeepLast real lines rather than the last
	// fileReadKeepLast-1 plus an empty one — otherwise the range this
	// function reports and the range the caller gets disagree by a line.
	kept := TruncateLines(strings.TrimSuffix(content, "\n"), fileReadKeepFirst, fileReadKeepLast)
	return kept, FileReadCut{
		TotalLines: total,
		KeptFirst:  fileReadKeepFirst,
		KeptLast:   fileReadKeepLast,
	}
}

// SummarizeFileWrite creates a brief summary for file write operations.
func SummarizeFileWrite(path string, lineCount int) string {
	return fmt.Sprintf("Successfully wrote %d lines to %s", lineCount, path)
}

// ListPopulation reports how big the collection behind a rendered list really
// is. Only the caller that did the enumerating knows this: a caller that
// stopped early — at a walk cap, a page boundary, a deadline — hands over a
// slice whose length is a floor and not a total, and nothing downstream can
// tell that slice apart from a complete one by looking at it.
//
// EnumerationComplete is what separates the two. When it is false, Count is
// the number of items the caller managed to see before it gave up, and the
// real total is unknown and larger.
type ListPopulation struct {
	Count               int
	EnumerationComplete bool
}

// CompletePopulation describes a collection the caller enumerated in full.
func CompletePopulation(count int) ListPopulation {
	return ListPopulation{Count: count, EnumerationComplete: true}
}

// PopulationStoppedEarly describes a collection the caller gave up on after
// seeing countSeen items, so the total is unknown and greater than countSeen.
func PopulationStoppedEarly(countSeen int) ListPopulation {
	return ListPopulation{Count: countSeen, EnumerationComplete: false}
}

// TruncateList renders at most maxItems of a list of items (e.g., file
// listings), followed by a footer describing everything the reader is not
// being shown.
//
// population must describe the collection items was drawn from. It is a
// parameter rather than something derived from len(items) because a caller
// that stopped enumerating early is the only place that still knows it did:
// by the time the slice arrives here, a walk that halted at its cap and a
// directory that genuinely holds exactly that many entries are identical. The
// footer used to say "Total: N items" off len(items) alone, which stated the
// cap as the size of the tree for every listing large enough to hit it.
func TruncateList(items []string, maxItems int, population ListPopulation) string {
	// Everything enumerated, everything shown: the list speaks for itself and
	// there is nothing to disclose.
	if len(items) <= maxItems && population.EnumerationComplete {
		return strings.Join(items, "\n")
	}

	shown := items
	if len(items) > maxItems {
		shown = items[:maxItems]
	}

	var result strings.Builder
	for _, item := range shown {
		result.WriteString(item)
		result.WriteString("\n")
	}
	result.WriteString("\n")

	if omitted := len(items) - len(shown); omitted > 0 {
		result.WriteString(fmt.Sprintf("[...%d more items...]\n", omitted))
	}

	// The remediation named here has to be something the tool actually
	// offers. list_files takes a path and a recursive flag and no pattern of
	// any kind, so telling the reader to filter by pattern sends it to an
	// argument that does not exist.
	if population.EnumerationComplete {
		result.WriteString(fmt.Sprintf("(Total: %d items. List a subdirectory to narrow it)\n", population.Count))
	} else {
		result.WriteString(fmt.Sprintf("(Listing stopped after %d items and the total is unknown — there are more than this. List a subdirectory to see them)\n", population.Count))
	}

	return result.String()
}

// ShouldTruncateOldToolResult determines if an old tool result should be summarized.
// turnsAgo: how many turns ago this tool result occurred.
func ShouldTruncateOldToolResult(turnsAgo int) bool {
	return turnsAgo > 10
}

// SummarizeOldToolResult creates a brief summary of an old tool result.
// This is used when pruning old tool results from conversation history.
func SummarizeOldToolResult(toolName, fullOutput string) string {
	lines := strings.Split(fullOutput, "\n")
	lineCount := len(lines)

	// For very short results, just return as-is
	if lineCount <= 3 {
		return fullOutput
	}

	// Extract key information based on tool type
	var summary strings.Builder
	summary.WriteString(fmt.Sprintf("[%s result, %d lines]", toolName, lineCount))

	// Tool-specific extraction
	switch toolName {
	case "shell", "shell_commands":
		// Include first line (command result) and any error markers
		if lineCount > 0 {
			summary.WriteString("\n")
			summary.WriteString(lines[0])
			if strings.Contains(fullOutput, "error") || strings.Contains(fullOutput, "Error") {
				summary.WriteString("\n[contains errors]")
			}
		}

	case "read_file", "read_files":
		// Just note what was read
		summary.WriteString(" - content available in full history")

	case "write_file", "edit_file", "write_files", "edit_files":
		// These are already brief, keep as-is
		return fullOutput

	case "list_files":
		// Show count only
		summary.WriteString(fmt.Sprintf(" - %d items listed", lineCount))

	default:
		// Generic: first + last line
		if lineCount > 0 {
			summary.WriteString("\n")
			summary.WriteString(lines[0])
			if lineCount > 1 {
				summary.WriteString("\n[...]")
				summary.WriteString("\n")
				summary.WriteString(lines[lineCount-1])
			}
		}
	}

	return summary.String()
}
