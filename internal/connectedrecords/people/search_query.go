package people

import (
	"fmt"
	"strings"
)

// searchSeparators are the characters the search document reads as spaces:
// every ASCII punctuation mark. The migration's translate() holds the same
// list, so a query is split into words the way the text it searches was.
const searchSeparators = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

func searchWordsOf(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || strings.ContainsRune(searchSeparators, r)
	})
}

// searchQueryOf turns what a person typed into GET /persons?q= into a
// to_tsquery expression: every typed word is a prefix, and the words are
// ANDed, so "ali smi" finds Alice Smith. A typed word with punctuation inside
// it, telegram_123456789, is its parts in order with the last a prefix, as
// SQLite's FTS5 matched it. "Quoted words" must appear together as typed, the
// last a prefix too. An unclosed quote, and FTS5's OR, NOT and NEAR, which
// this search does not read, are the caller's 400.
func searchQueryOf(q string) (string, error) {
	if strings.Count(q, `"`)%2 != 0 {
		return "", fmt.Errorf("%w: bad search query %q: a double quote is not closed", ErrInvalid, q)
	}
	var terms []string
	for index, segment := range strings.Split(q, `"`) {
		if index%2 == 1 {
			if words := searchWordsOf(segment); len(words) > 0 {
				terms = append(terms, phraseOf(words))
			}
			continue
		}
		for _, token := range strings.Fields(segment) {
			switch token {
			case "AND":
				continue
			case "OR", "NOT", "NEAR":
				return "", fmt.Errorf("%w: bad search query %q: %s is not read; every word must appear, as a prefix", ErrInvalid, q, token)
			}
			if words := searchWordsOf(token); len(words) > 0 {
				terms = append(terms, phraseOf(words))
			}
		}
	}
	if len(terms) == 0 {
		return "", fmt.Errorf("%w: bad search query %q: it holds no word to search for", ErrInvalid, q)
	}
	return strings.Join(terms, " & "), nil
}

// phraseOf is the words one after another, the last one a prefix.
func phraseOf(words []string) string {
	quoted := make([]string, len(words))
	for index, word := range words {
		// The words hold no ASCII punctuation, so no quote or backslash.
		quoted[index] = "'" + word + "'"
	}
	quoted[len(quoted)-1] += ":*"
	if len(quoted) == 1 {
		return quoted[0]
	}
	return "(" + strings.Join(quoted, " <-> ") + ")"
}
