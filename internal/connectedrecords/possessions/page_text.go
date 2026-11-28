package possessions

import (
	"html"
	"regexp"
	"strings"
)

var (
	scriptOrStyle  = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	htmlComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag        = regexp.MustCompile(`(?s)<[^>]+>`)
	invisibleRunes = strings.NewReplacer("\u034f", "", "\u200c", "", "\u200b", "", "\u00ad", "", "\ufeff", "", "\u00a0", " ")
)

// pageText turns an HTML page or email into the lines a reader would see:
// scripts, styles and comments go, every tag ends a line, entities are
// decoded, and blank lines and the invisible padding mail templates use
// are dropped. It is for a model to read, not for display.
func pageText(page string) string {
	page = scriptOrStyle.ReplaceAllString(page, "\n")
	page = htmlComment.ReplaceAllString(page, "\n")
	page = htmlTag.ReplaceAllString(page, "\n")
	page = invisibleRunes.Replace(html.UnescapeString(page))
	lines := []string{}
	for _, line := range strings.Split(page, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// Clip keeps the first limit bytes of text, cut at a line, and says so.
func Clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := strings.LastIndex(text[:limit], "\n")
	if cut < 0 {
		cut = limit
	}
	return text[:cut] + "\n[… the rest is cut]"
}
