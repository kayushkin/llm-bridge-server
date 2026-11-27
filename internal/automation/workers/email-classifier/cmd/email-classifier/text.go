package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ============================ body cleaning ============================
//
// Bulk mail is mostly not words. Measured on this host's inbox, a Meetup
// invitation's body_text is ~95% click-tracking URL, and Google's account
// notices carry Outlook conditional comments plus a wall of invisible padding
// characters that senders use to pad the preview line. All of it reaches the
// prompt as tokens and none of it helps decide whether a message is a task.
//
// These functions run before the digest is built, so the model reads sentences
// rather than tracking parameters.

var (
	htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)
	urlPattern         = regexp.MustCompile(`https?://[^\s<>"']+`)
	htmlTagPattern     = regexp.MustCompile(`(?s)<[a-zA-Z/][^>]*>`)
	// style and script hold text that is not prose. Stripping only their tags
	// would leave the CSS and JavaScript behind as body content — measured on
	// eBay and Meetup mail, whose style blocks alone filled the entire prompt
	// budget with @media rules and font-face declarations.
	stylePattern   = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	scriptPattern  = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	doctypePattern = regexp.MustCompile(`(?i)<!DOCTYPE[^>]*>`)
)

// invisibleRunes are the characters senders use as preview-pane padding. They
// carry no meaning, and a single Meetup message can contain hundreds of them.
//
// U+034F is the combining grapheme joiner and U+200C the zero-width non-joiner;
// both appear in this host's mail purely as spacers. The rest are the standard
// zero-width and byte-order-mark family.
// Written as escapes rather than literals: a literal U+FEFF is an illegal byte
// order mark in Go source, and the rest are invisible in an editor, so a
// reviewer could not tell them apart or spot a duplicate.
var invisibleRunes = map[rune]bool{
	'\u00AD': true, // soft hyphen
	'\u034F': true, // combining grapheme joiner
	'\u200B': true, // zero-width space
	'\u200C': true, // zero-width non-joiner
	'\u200D': true, // zero-width joiner
	'\u200E': true, // left-to-right mark
	'\u200F': true, // right-to-left mark
	'\u2060': true, // word joiner
	'\u2061': true, // function application
	'\u2062': true, // invisible times
	'\u2063': true, // invisible separator
	'\u2064': true, // invisible plus
	'\uFEFF': true, // zero-width no-break space / BOM
}

// shortenURL reduces a URL to its host, so the model still sees that a link to
// paypal.com or github.com was present without paying for the tracking payload.
// Everything after the host is discarded: a click-tracking path encodes the
// campaign, not the meaning.
func shortenURL(raw string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	host := trimmed
	if i := strings.IndexAny(trimmed, "/?#"); i >= 0 {
		host = trimmed[:i]
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return ""
	}
	return "[link:" + host + "]"
}

// stripInvisibleRunes removes preview-padding characters.
func stripInvisibleRunes(s string) string {
	return strings.Map(func(r rune) rune {
		if invisibleRunes[r] {
			return -1
		}
		return r
	}, s)
}

// cleanBodyText turns a raw message body into the prose the classifier should
// read. Order matters: comments go before tags, because an Outlook conditional
// comment contains tags that would otherwise be unwrapped into their contents.
func cleanBodyText(raw string) string {
	s := htmlCommentPattern.ReplaceAllString(raw, " ")
	s = stylePattern.ReplaceAllString(s, " ")
	s = scriptPattern.ReplaceAllString(s, " ")
	s = doctypePattern.ReplaceAllString(s, " ")
	s = htmlTagPattern.ReplaceAllString(s, " ")
	s = urlPattern.ReplaceAllStringFunc(s, shortenURL)
	// Entities are decoded BEFORE invisible runes are stripped, not after.
	// eBay pads its preview line with the encoded forms (`&#847;&zwnj;`), which
	// the rune stripper cannot see while they are still text — decoding second
	// left hundreds of them in the prompt.
	s = unescapeCommonEntities(s)
	s = decodeNumericEntities(s)
	s = stripInvisibleRunes(s)
	return collapseWhitespace(s)
}

// numericEntityPattern matches both decimal (&#847;) and hexadecimal (&#x34F;)
// character references.
var numericEntityPattern = regexp.MustCompile(`&#([xX]?)([0-9a-fA-F]+);`)

// decodeNumericEntities resolves numeric character references so that padding
// characters encoded as entities become the runes stripInvisibleRunes removes.
// An out-of-range or unparseable reference is left exactly as written rather
// than replaced with a substitution character, so nothing is silently corrupted.
func decodeNumericEntities(s string) string {
	return numericEntityPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := numericEntityPattern.FindStringSubmatch(match)
		base := 10
		if groups[1] != "" {
			base = 16
		}
		code, err := strconv.ParseInt(groups[2], base, 32)
		if err != nil || code <= 0 || code > unicode.MaxRune {
			return match
		}
		return string(rune(code))
	})
}

// commonEntities covers what actually shows up in mail subjects and bodies on
// this host. A full HTML entity table would be a dependency for no gain: an
// unrecognized entity survives as literal text, which the model reads fine.
var commonEntities = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&apos;", "'",
	"&nbsp;", " ",
	"&mdash;", "—",
	"&ndash;", "–",
	"&hellip;", "…",
	// The named spellings of the preview-padding characters. Decoded here so
	// stripInvisibleRunes can then remove them.
	"&zwnj;", "\u200C",
	"&zwj;", "\u200D",
	"&shy;", "\u00AD",
	"&lrm;", "\u200E",
	"&rlm;", "\u200F",
)

func unescapeCommonEntities(s string) string {
	return commonEntities.Replace(s)
}
