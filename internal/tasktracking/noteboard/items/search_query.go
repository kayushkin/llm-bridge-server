package items

import "strings"

// searchSeparators are the characters the search document reads as spaces:
// every ASCII punctuation mark. The migration's translate() holds the same
// list, so a query is split into words the way the text it searches was.
const searchSeparators = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

func wordsOf(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || strings.ContainsRune(searchSeparators, r)
	})
}

// searchTermsOf splits a search into the terms an item must all match. Each
// whitespace-separated token is one term, read as literal text and never as
// query syntax: hello-world is the phrase "hello world", a leading - is not
// "not", OR is a word, and a quote is punctuation. That is what the FTS5 search
// did, which quoted each token as a phrase. A token with no word in it, such
// as "--", is no term.
func searchTermsOf(query string) []string {
	terms := []string{}
	for _, token := range strings.Fields(query) {
		if words := wordsOf(token); len(words) > 0 {
			terms = append(terms, strings.Join(words, " "))
		}
	}
	return terms
}
