package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/kayushkin/tool-store/schema"
)

// WebFetch returns a tool that fetches a URL and extracts readable text content.
func WebFetch() Impl {
	type input struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	return Impl{
		Name:        "web_fetch",
		Description: "Fetch a URL and extract readable text content. Strips HTML tags, scripts, styles, and navigation to return clean text. Use for reading articles, docs, etc.",
		InputSchema: schema.Props([]string{"url"}, map[string]any{
			"url":       schema.Str("URL to fetch"),
			"max_chars": schema.Integer("Maximum characters to return (default 50000)"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			maxChars := in.MaxChars
			if maxChars <= 0 {
				maxChars = 50000
			}

			req, err := http.NewRequestWithContext(ctx, "GET", in.URL, nil)
			if err != nil {
				return fmt.Sprintf("error: %s", err), nil
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AgentKit/1.0)")
			req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,*/*")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Sprintf("error: %s", err), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				return fmt.Sprintf("error: HTTP %d", resp.StatusCode), nil
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Sprintf("error reading body: %s", err), nil
			}

			content := string(body)

			// If it looks like HTML, extract text
			if strings.Contains(resp.Header.Get("Content-Type"), "html") || strings.HasPrefix(strings.TrimSpace(content), "<") {
				content = extractText(content)
			}

			if truncated := truncateToRuneCount(content, maxChars); len(truncated) < len(content) {
				content = truncated + "\n... (truncated)"
			}

			return content, nil
		},
	}
}

// truncateToRuneCount returns the first maxRunes characters of s, or s itself
// when it is already that short or shorter.
//
// It counts characters rather than bytes, which is what the max_chars schema
// promises. Counting bytes instead under-delivers on any text that is not plain
// ASCII — a 50000-byte cut of Japanese returns about 16000 characters — and it
// can also land inside a multi-byte character and split it. Ranging over a
// string yields the byte offset of each character's first byte, so cutting at
// one of those offsets is always a whole number of characters.
func truncateToRuneCount(s string, maxRunes int) string {
	count := 0
	for byteOffset := range s {
		if count >= maxRunes {
			return s[:byteOffset]
		}
		count++
	}
	return s
}

var (
	reScript   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reNav      = regexp.MustCompile(`(?is)<nav[^>]*>.*?</nav>`)
	reHeader   = regexp.MustCompile(`(?is)<header[^>]*>.*?</header>`)
	reFooter   = regexp.MustCompile(`(?is)<footer[^>]*>.*?</footer>`)
	reTags     = regexp.MustCompile(`<[^>]+>`)
	reSpaces   = regexp.MustCompile(`[ \t]+`)
	reNewlines = regexp.MustCompile(`\n{3,}`)
	reEntities = regexp.MustCompile(`&(amp|lt|gt|quot|apos|nbsp|#\d+|#x[0-9a-fA-F]+);`)
)

func extractText(html string) string {
	// Remove non-content elements
	s := reScript.ReplaceAllString(html, "")
	s = reStyle.ReplaceAllString(s, "")
	s = reNav.ReplaceAllString(s, "")
	s = reHeader.ReplaceAllString(s, "")
	s = reFooter.ReplaceAllString(s, "")

	// Add newlines before block elements
	for _, tag := range []string{"p", "div", "br", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "td", "th", "article", "section"} {
		s = strings.ReplaceAll(s, "<"+tag, "\n<"+tag)
		s = strings.ReplaceAll(s, "</"+tag, "\n</"+tag)
	}

	// Strip tags
	s = reTags.ReplaceAllString(s, "")

	// Decode common entities
	s = reEntities.ReplaceAllStringFunc(s, decodeEntity)

	// Clean whitespace
	s = reSpaces.ReplaceAllString(s, " ")
	s = reNewlines.ReplaceAllString(s, "\n\n")

	// Trim lines
	lines := strings.Split(s, "\n")
	var clean []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			clean = append(clean, l)
		}
	}
	return strings.Join(clean, "\n")
}

func decodeEntity(e string) string {
	switch e {
	case "&amp;":
		return "&"
	case "&lt;":
		return "<"
	case "&gt;":
		return ">"
	case "&quot;":
		return "\""
	case "&apos;":
		return "'"
	case "&nbsp;":
		return " "
	default:
		return e
	}
}
