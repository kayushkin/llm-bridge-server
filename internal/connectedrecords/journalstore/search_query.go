package journalstore

import (
	"strings"
)

// searchSeparators are the characters the search document reads as spaces:
// every ASCII punctuation mark. The migration's translate() holds the same
// list, so a query is split into words the way the text it searches was.
const searchSeparators = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

func wordsOf(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || strings.ContainsRune(searchSeparators, r)
	})
}

// searchQueryOf turns what a person typed into a to_tsquery expression with
// the meaning the standalone store's FTS5 query had: each whitespace-separated
// word a quoted prefix, and the words ANDed. A word with punctuation inside
// it, well-being, is the phrase "well being" with its last part a prefix, as
// FTS5 read "well-being"*; punctuation is text, never grammar, so a hyphen or
// a stray quote cannot make the query fail. Each lexeme is quoted for
// to_tsquery, which still runs the simple parser over it, as it ran over the
// document.
//
// searchable is false when q holds words but none with a letter or digit in
// it: such a search matches nothing. q with no words at all is no search.
func searchQueryOf(q string) (expression string, searchable bool) {
	var terms []string
	words := strings.Fields(q)
	for _, word := range words {
		parts := wordsOf(word)
		if len(parts) == 0 {
			continue
		}
		quoted := make([]string, len(parts))
		for index, part := range parts {
			quoted[index] = "'" + strings.ReplaceAll(part, "'", "''") + "'"
		}
		quoted[len(quoted)-1] += ":*"
		term := strings.Join(quoted, " <-> ")
		if len(quoted) > 1 {
			term = "(" + term + ")"
		}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return "", len(words) == 0
	}
	return strings.Join(terms, " & "), true
}
