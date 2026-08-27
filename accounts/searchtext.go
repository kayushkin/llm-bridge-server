package accounts

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ============================ search text derivation ============================
//
// The FTS index needs one plain-text rendering per message. Senders that build
// HTML-first mail often ship a text part that is a single "view this email in
// your browser" line, so the text part is preferred but not trusted blindly:
// when it is a stub and the HTML part yields substantially more prose, the
// de-tagged HTML wins. This mirrors how the message is actually read, so the
// index matches what a reader would see.

// searchTextCap bounds one message's contribution to the index. Email carries
// its meaning early; past this point it is footer, legal boilerplate and
// tracking markup, which add index size and no discrimination.
const searchTextCap = 12000

// stubBodyTextThreshold is the length below which a text part is suspected of
// being a placeholder rather than the message.
const stubBodyTextThreshold = 200

// SearchableText renders a message's parts into the plain text the FTS index
// stores. Empty when the message has no usable text at all.
func SearchableText(bodyText, bodyHTML string) string {
	text := collapseSpace(bodyText)
	if len(text) >= stubBodyTextThreshold {
		return capText(text)
	}
	fromHTML := collapseSpace(stripHTML(bodyHTML))
	if len(fromHTML) > len(text)*2 && len(fromHTML) > stubBodyTextThreshold {
		return capText(fromHTML)
	}
	return capText(text)
}

var (
	htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlStylePattern   = regexp.MustCompile(`(?is)<style\b.*?</style>`)
	htmlScriptPattern  = regexp.MustCompile(`(?is)<script\b.*?</script>`)
	htmlHeadPattern    = regexp.MustCompile(`(?is)<head\b.*?</head>`)
	htmlTagPattern     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// htmlEntityReplacer covers the entities that actually occur in mail bodies.
// Anything rarer survives as its literal form, which for a search index is a
// harmless unknown token, not a defect.
var htmlEntityReplacer = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
	"&quot;", `"`, "&#39;", "'", "&apos;", "'", "&mdash;", "—",
	"&ndash;", "–", "&hellip;", "…", "&copy;", "©", "&reg;", "®",
	"&trade;", "™", "&zwnj;", "", "&zwj;", "", "&#8203;", "",
)

// stripHTML reduces an HTML part to its text: comments, style, script and head
// blocks removed whole, remaining tags dropped, common entities decoded.
func stripHTML(html string) string {
	if html == "" {
		return ""
	}
	s := htmlCommentPattern.ReplaceAllString(html, " ")
	s = htmlStylePattern.ReplaceAllString(s, " ")
	s = htmlScriptPattern.ReplaceAllString(s, " ")
	s = htmlHeadPattern.ReplaceAllString(s, " ")
	s = htmlTagPattern.ReplaceAllString(s, " ")
	return htmlEntityReplacer.Replace(s)
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// capText truncates on a rune boundary at searchTextCap.
func capText(s string) string {
	if len(s) <= searchTextCap {
		return s
	}
	cut := s[:searchTextCap]
	// Do not split a multi-byte rune: drop trailing bytes until the string
	// ends on a complete one.
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	return cut
}
