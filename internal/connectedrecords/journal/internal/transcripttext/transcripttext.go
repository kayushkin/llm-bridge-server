// Package transcripttext writes one chat message as one line of a
// transcript's markdown body, for every importer that writes transcripts.
package transcripttext

import "strings"

// MarkdownSafe keeps a message from being read as markdown structure: a line
// that would start a heading, list, quote or rule is escaped, and every ref:
// link is broken so a pasted one is not a mention.
func MarkdownSafe(line string) string {
	line = strings.ReplaceAll(line, "](ref:", "](ref\\:")
	trimmed := strings.TrimLeft(line, " ")
	if trimmed != "" && strings.ContainsRune("#>-*+=|`", rune(trimmed[0])) {
		return "\\" + trimmed
	}
	return trimmed
}
