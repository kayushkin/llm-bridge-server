package predictionstore

import (
	"fmt"
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

// searchQueryOf rewrites q for websearch_to_tsquery so it splits words as the
// search document does. A bare word with punctuation inside it, deploy.sh,
// becomes the phrase "deploy sh", as FTS5 matched it; a leading - still means
// "not"; or still means either; a quoted phrase stays a phrase. An unclosed
// quote is the caller's mistake, as FTS5 said it was.
func searchQueryOf(q string) (string, error) {
	if strings.Count(q, `"`)%2 != 0 {
		return "", fmt.Errorf("%w: bad search query %q: a double quote is not closed", ErrInvalidPrediction, q)
	}
	var parts []string
	for index, segment := range strings.Split(q, `"`) {
		if index%2 == 1 {
			if words := wordsOf(segment); len(words) > 0 {
				parts = append(parts, `"`+strings.Join(words, " ")+`"`)
			}
			continue
		}
		for _, token := range strings.Fields(segment) {
			negated := strings.HasPrefix(token, "-")
			words := wordsOf(strings.TrimPrefix(token, "-"))
			if len(words) == 0 {
				continue
			}
			term := words[0]
			if len(words) > 1 {
				term = `"` + strings.Join(words, " ") + `"`
			}
			if negated {
				term = "-" + term
			}
			parts = append(parts, term)
		}
	}
	return strings.Join(parts, " "), nil
}
