package journalstore

import (
	"regexp"
	"strings"
	"unicode"
)

// refLinkPattern matches a markdown link whose target uses the ref: scheme:
// [label](ref:<id>). The id runs to the closing parenthesis and holds no
// whitespace. A link with any other target (https:, a relative path) is not a
// mention.
var refLinkPattern = regexp.MustCompile(`\[((?:\\.|[^\]\\])*)\]\(ref:([^)\s]+)\)`)

// ExtractMentions finds every ref: link in a markdown body, in order. Links
// inside fenced code blocks and inline code spans are text, not links, and
// are skipped. EntryID and EntityType are left for the caller.
func ExtractMentions(body string) []Mention {
	mentions := []Mention{}
	inFence := false
	fenceMarker := ""
	for lineIndex, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if marker := fenceOpening(trimmed); marker != "" {
			if !inFence {
				inFence, fenceMarker = true, marker
				continue
			}
			if strings.HasPrefix(trimmed, fenceMarker) && strings.TrimSpace(strings.TrimLeft(trimmed, fenceMarker[:1])) == "" {
				inFence = false
				continue
			}
		}
		if inFence {
			continue
		}
		visible := blankInlineCode(line)
		for _, match := range refLinkPattern.FindAllStringSubmatchIndex(visible, -1) {
			mentions = append(mentions, Mention{
				Position:  len(mentions),
				Line:      lineIndex + 1,
				Label:     unescapeLinkText(line[match[2]:match[3]]),
				EntityRef: line[match[4]:match[5]],
			})
		}
	}
	return mentions
}

// fenceOpening returns the fence marker (``` or ~~~, three or more) a line
// starts with, or "".
func fenceOpening(trimmed string) string {
	for _, character := range []string{"`", "~"} {
		run := 0
		for run < len(trimmed) && trimmed[run] == character[0] {
			run++
		}
		if run >= 3 {
			return trimmed[:run]
		}
	}
	return ""
}

// blankInlineCode replaces the inside of every `code span` with spaces, so a
// link written inside one is not matched and every byte offset still lines up
// with the original line.
func blankInlineCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	out := []byte(line)
	for start := 0; start < len(out); {
		if out[start] != '`' {
			start++
			continue
		}
		run := 0
		for start+run < len(out) && out[start+run] == '`' {
			run++
		}
		closing := strings.Index(string(out[start+run:]), strings.Repeat("`", run))
		if closing < 0 {
			break
		}
		for index := start; index < start+run+closing+run; index++ {
			out[index] = ' '
		}
		start += run + closing + run
	}
	return string(out)
}

func unescapeLinkText(text string) string {
	replacer := strings.NewReplacer(`\]`, `]`, `\[`, `[`, `\\`, `\`)
	return replacer.Replace(text)
}

func escapeLinkText(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
	return replacer.Replace(text)
}

// checklistPattern matches a markdown task-list item: indent, a list marker,
// the box, then the text.
var checklistPattern = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+\[([ xX])\]\s+(.*)$`)

// checklistItem is one parsed task-list line.
type checklistItem struct {
	indent, marker, box, text string
}

func parseChecklistLine(line string) (checklistItem, bool) {
	match := checklistPattern.FindStringSubmatch(line)
	if match == nil {
		return checklistItem{}, false
	}
	return checklistItem{indent: match[1], marker: match[2], box: match[3], text: strings.TrimSpace(match[4])}, true
}

// linkedLine renders the item with its text as a ref: link to id, keeping
// the indent, the marker and whether the box was ticked.
func (item checklistItem) linkedLine(id string) string {
	return item.indent + item.marker + " [" + item.box + "] [" + escapeLinkText(item.text) + "](ref:" + id + ")"
}

// validSlug holds a slug to lowercase letters and digits in any script,
// joined by single hyphens.
func validSlug(slug string) bool {
	if slug == "" || strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return false
	}
	for _, character := range slug {
		if character == '-' || unicode.IsDigit(character) || (unicode.IsLetter(character) && !unicode.IsUpper(character)) {
			continue
		}
		return false
	}
	return true
}

// DeriveSlug turns a title into a slug: lowercased, every run of characters
// that are not letters or digits becomes one hyphen. Letters outside ASCII
// are kept, so a Russian title gives a Russian slug rather than an empty one.
func DeriveSlug(title string) string {
	var builder strings.Builder
	pendingHyphen := false
	for _, character := range strings.ToLower(title) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			if pendingHyphen && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			pendingHyphen = false
			builder.WriteRune(character)
			continue
		}
		pendingHyphen = true
	}
	return builder.String()
}
