package activitystore

import "strings"

// searchSeparators are the characters the search document reads as spaces:
// every ASCII punctuation mark. searchDocumentSQL's translate() holds the
// same list, so a query is split into words the way the text it searches was.
const searchSeparators = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// searchDocumentSQL is the search document of a title, a url and the text of
// details (searchableDetails), given as three placeholders. It keeps only the
// words, lower-cased, as the standalone store's contentless FTS5 index kept
// only terms.
func searchDocumentSQL(title, url, detailsText string) string {
	return `to_tsvector('simple', translate(CAST(` + title + ` AS text) || ' ' || CAST(` + url + ` AS text) || ' ' || CAST(` + detailsText + ` AS text), ` +
		`'!"#$%&''()*+,-./:;<=>?@[\]^_` + "`" + `{|}~', '                                '))`
}

func wordsOf(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || strings.ContainsRune(searchSeparators, r)
	})
}

// searchQueryOf turns what a person typed into a to_tsquery expression with
// the meaning the standalone store's FTS5 query had: each whitespace-separated
// word a quoted phrase that must appear whole, and the words ANDed. A word
// with punctuation inside it, well-being, is the phrase "well being", as FTS5
// read "well-being"; punctuation is text, never grammar, so a stray quote
// cannot make the query fail.
//
// searchable is false when q holds words but none with a letter or digit in
// it: such a search matches nothing.
func searchQueryOf(q string) (expression string, searchable bool) {
	var terms []string
	for _, word := range strings.Fields(q) {
		parts := wordsOf(word)
		if len(parts) == 0 {
			continue
		}
		quoted := make([]string, len(parts))
		for index, part := range parts {
			quoted[index] = "'" + strings.ReplaceAll(strings.ReplaceAll(part, `\`, `\\`), "'", "''") + "'"
		}
		term := strings.Join(quoted, " <-> ")
		if len(quoted) > 1 {
			term = "(" + term + ")"
		}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return "", false
	}
	return strings.Join(terms, " & "), true
}
