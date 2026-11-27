package principals

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Reading the q filter.
//
// The standalone store searched an FTS5 index, and its callers learned two
// ways to write a query. A query with no search syntax is a list of words,
// each a prefix — "pri" finds Priya Raman, "priya northwind" finds her by name
// and the domain of her address — because the main caller is an assignee
// picker reading keystrokes. A query that carries search syntax (a double
// quote, a star, parentheses, or an upper-case AND, OR or NOT) means exactly
// what it says: "Dana Example" in quotes is that phrase, a bare word is
// that whole word, and a word followed by * is a prefix. Both are read here
// into a Postgres tsquery over the generated search_document column.
//
// A column filter, display_name:data or email:"northwind eng", searches one
// field. What is gone: NEAR, a {column list} filter, and ^ for "first word".
// Each is a 400 naming what to write instead, never a query that silently
// means something else.

// searchSeparators are the characters that separate words, beside spaces:
// every ASCII punctuation mark. The document and the query are both split by
// searchWordsOf, so a query is split into words the way the text it searches
// was.
const searchSeparators = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// searchWordsOf splits text into words as the search document holds them:
// at every space and ASCII punctuation mark, lower-cased, with diacritics
// taken off (José is jose), as FTS5's unicode61 tokenizer read them.
func searchWordsOf(text string) []string {
	words := strings.FieldsFunc(withoutDiacritics(text), func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(searchSeparators, r)
	})
	for index, word := range words {
		words[index] = strings.ToLower(word)
	}
	return words
}

// withoutDiacritics takes the marks off letters: it decomposes the text and
// drops the combining marks, so é is e and ñ is n.
func withoutDiacritics(text string) string {
	decomposed := norm.NFD.String(text)
	var kept strings.Builder
	kept.Grow(len(decomposed))
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			kept.WriteRune(r)
		}
	}
	return kept.String()
}

// searchTextOf is a field's text as the search document indexes it: its
// words, joined by spaces.
func searchTextOf(text string) string {
	return strings.Join(searchWordsOf(text), " ")
}

// searchDocumentSQL builds the search document from two parameters, the
// searchTextOf of the display name and of the email, numbered as given.
func searchDocumentSQL(displayNameParameter, emailParameter int) string {
	return fmt.Sprintf(`setweight(to_tsvector('simple', $%d), 'A') || setweight(to_tsvector('simple', $%d), 'B')`,
		displayNameParameter, emailParameter)
}

// searchSyntaxCharacters, in a query, switch it from a list of prefixes to a
// query that means exactly what it says, as they did in front of FTS5.
const searchSyntaxCharacters = `"*:()`

// searchOperators are the upper-case words that are operators, not words.
var searchOperators = map[string]bool{"AND": true, "OR": true, "NOT": true, "NEAR": true}

// searchTSQueryOf turns what a caller typed into the text of a tsquery for
// to_tsquery('simple', …). Every word in it is quoted and holds no ASCII
// punctuation, so nothing the caller typed can be read as tsquery syntax. An
// empty answer with no error is a query that holds no word, which matches
// nothing.
func searchTSQueryOf(q string) (string, error) {
	q = strings.TrimSpace(q)
	carriesSyntax := strings.ContainsAny(q, searchSyntaxCharacters)
	if !carriesSyntax {
		for _, field := range strings.Fields(q) {
			if searchOperators[field] {
				carriesSyntax = true
				break
			}
		}
	}
	if !carriesSyntax {
		var terms []string
		for _, field := range strings.Fields(q) {
			if term := tsqueryPhrase(searchWordsOf(field), true, ""); term != "" {
				terms = append(terms, term)
			}
		}
		// Punctuation alone, such as a "-" typed into a picker, holds no word
		// and matches nothing, as FTS5 answered an empty prefix: an empty
		// answer, not the caller's mistake.
		return strings.Join(terms, " & "), nil
	}
	tokens, err := searchTokensOf(q)
	if err != nil {
		return "", fmt.Errorf("%w: bad search query %q: %v", ErrInvalidPrincipal, q, err)
	}
	parser := &searchParser{tokens: tokens}
	expression, err := parser.orExpression()
	if err == nil && parser.position < len(parser.tokens) {
		err = fmt.Errorf("unexpected %s", parser.tokens[parser.position].describe())
	}
	if err != nil {
		return "", fmt.Errorf("%w: bad search query %q: %v", ErrInvalidPrincipal, q, err)
	}
	return expression, nil
}

// searchColumnWeights are the columns a query may name in a column filter
// (display_name:data), and the weight each one's words carry in the search
// document.
var searchColumnWeights = map[string]string{"display_name": "A", "email": "B"}

// tsqueryPhrase joins words into a tsquery phrase, the last a prefix when
// prefix is set, within the one column whose weight is given, or within
// either column when weight is empty: the document holds both columns one
// after the other, and a phrase must not match the last word of the name
// beside the first of the address, which FTS5 never did. No words is the
// empty string.
func tsqueryPhrase(words []string, prefix bool, weight string) string {
	if len(words) == 0 {
		return ""
	}
	if weight == "" && len(words) > 1 {
		return "(" + tsqueryPhrase(words, prefix, searchColumnWeights["display_name"]) + " | " +
			tsqueryPhrase(words, prefix, searchColumnWeights["email"]) + ")"
	}
	quoted := make([]string, len(words))
	for index, word := range words {
		label := weight
		if prefix && index == len(words)-1 {
			label = "*" + weight
		}
		quoted[index] = "'" + word + "'"
		if label != "" {
			quoted[index] += ":" + label
		}
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return "(" + strings.Join(quoted, " <-> ") + ")"
}

type searchTokenKind int

const (
	searchTokenPhrase searchTokenKind = iota
	searchTokenOpenParenthesis
	searchTokenCloseParenthesis
	searchTokenAnd
	searchTokenOr
	searchTokenNot
)

type searchToken struct {
	kind   searchTokenKind
	words  []string
	prefix bool
	// weight is the column a column filter named, as its weight; empty for
	// both columns.
	weight string
	text   string
}

func (token searchToken) describe() string {
	if token.kind == searchTokenPhrase {
		return fmt.Sprintf("%q", token.text)
	}
	return token.text
}

// searchTokensOf reads a query that carries syntax into tokens: phrases (a
// quoted string, or a bare run of characters), each perhaps followed by * for
// a prefix, parentheses and the three operators.
func searchTokensOf(q string) ([]searchToken, error) {
	var tokens []searchToken
	runes := []rune(q)
	// pendingWeight is the column a column filter just named, waiting for the
	// phrase it applies to.
	pendingWeight := ""
	for index := 0; index < len(runes); {
		character := runes[index]
		if pendingWeight != "" && character != '"' && (unicode.IsSpace(character) || strings.ContainsRune(searchSyntaxCharacters, character)) {
			return nil, fmt.Errorf("a column filter must be followed at once by a word or a quoted phrase")
		}
		switch {
		case unicode.IsSpace(character):
			index++
		case character == '(':
			tokens = append(tokens, searchToken{kind: searchTokenOpenParenthesis, text: "("})
			index++
		case character == ')':
			tokens = append(tokens, searchToken{kind: searchTokenCloseParenthesis, text: ")"})
			index++
		case character == '*':
			return nil, fmt.Errorf("a * must follow a word or a phrase, as a prefix")
		case character == ':':
			return nil, fmt.Errorf("a : must follow a column name, display_name or email")
		case character == '"':
			var text strings.Builder
			index++
			closed := false
			for index < len(runes) {
				if runes[index] == '"' {
					// "" inside a quoted phrase is one quote, as FTS5 read it.
					if index+1 < len(runes) && runes[index+1] == '"' {
						text.WriteRune('"')
						index += 2
						continue
					}
					closed = true
					index++
					break
				}
				text.WriteRune(runes[index])
				index++
			}
			if !closed {
				return nil, fmt.Errorf("a double quote is not closed")
			}
			token := searchToken{kind: searchTokenPhrase, words: searchWordsOf(text.String()), text: `"` + text.String() + `"`, weight: pendingWeight}
			pendingWeight = ""
			if index < len(runes) && runes[index] == '*' {
				token.prefix = true
				index++
			}
			tokens = append(tokens, token)
		default:
			start := index
			for index < len(runes) && !unicode.IsSpace(runes[index]) && !strings.ContainsRune(searchSyntaxCharacters, runes[index]) {
				index++
			}
			text := string(runes[start:index])
			switch text {
			case "AND":
				tokens = append(tokens, searchToken{kind: searchTokenAnd, text: text})
				continue
			case "OR":
				tokens = append(tokens, searchToken{kind: searchTokenOr, text: text})
				continue
			case "NOT":
				tokens = append(tokens, searchToken{kind: searchTokenNot, text: text})
				continue
			case "NEAR":
				return nil, fmt.Errorf("NEAR is not supported; quote the words as a phrase, or list them to find rows holding all of them")
			}
			if strings.HasPrefix(text, "^") {
				return nil, fmt.Errorf("^ (first word only) is not supported")
			}
			if index < len(runes) && runes[index] == ':' {
				weight, known := searchColumnWeights[text]
				if !known {
					return nil, fmt.Errorf("%q is not a column; a column filter names display_name or email", text)
				}
				pendingWeight = weight
				index++
				continue
			}
			token := searchToken{kind: searchTokenPhrase, words: searchWordsOf(text), text: text, weight: pendingWeight}
			pendingWeight = ""
			if index < len(runes) && runes[index] == '*' {
				token.prefix = true
				index++
			}
			tokens = append(tokens, token)
		}
	}
	if pendingWeight != "" {
		return nil, fmt.Errorf("a column filter must be followed by a word or a quoted phrase")
	}
	return tokens, nil
}

// searchParser reads tokens with FTS5's precedence, tightest first: NOT, then
// AND (or two phrases side by side), then OR. NOT is binary, as in FTS5: "a
// NOT b" is a without b.
type searchParser struct {
	tokens   []searchToken
	position int
}

func (parser *searchParser) peek() (searchToken, bool) {
	if parser.position >= len(parser.tokens) {
		return searchToken{}, false
	}
	return parser.tokens[parser.position], true
}

func (parser *searchParser) orExpression() (string, error) {
	left, err := parser.andExpression()
	if err != nil {
		return "", err
	}
	for {
		token, ok := parser.peek()
		if !ok || token.kind != searchTokenOr {
			return left, nil
		}
		parser.position++
		right, err := parser.andExpression()
		if err != nil {
			return "", err
		}
		left = "(" + left + " | " + right + ")"
	}
}

func (parser *searchParser) andExpression() (string, error) {
	left, err := parser.notExpression()
	if err != nil {
		return "", err
	}
	for {
		token, ok := parser.peek()
		if !ok {
			return left, nil
		}
		switch token.kind {
		case searchTokenAnd:
			parser.position++
		case searchTokenPhrase, searchTokenOpenParenthesis:
		default:
			return left, nil
		}
		right, err := parser.notExpression()
		if err != nil {
			return "", err
		}
		left = "(" + left + " & " + right + ")"
	}
}

func (parser *searchParser) notExpression() (string, error) {
	left, err := parser.primary()
	if err != nil {
		return "", err
	}
	for {
		token, ok := parser.peek()
		if !ok || token.kind != searchTokenNot {
			return left, nil
		}
		parser.position++
		right, err := parser.primary()
		if err != nil {
			return "", err
		}
		left = "(" + left + " & !" + right + ")"
	}
}

func (parser *searchParser) primary() (string, error) {
	token, ok := parser.peek()
	if !ok {
		return "", fmt.Errorf("it ends where a word or a phrase was expected")
	}
	switch token.kind {
	case searchTokenOpenParenthesis:
		parser.position++
		inner, err := parser.orExpression()
		if err != nil {
			return "", err
		}
		closing, ok := parser.peek()
		if !ok || closing.kind != searchTokenCloseParenthesis {
			return "", fmt.Errorf("a parenthesis is not closed")
		}
		parser.position++
		return inner, nil
	case searchTokenPhrase:
		parser.position++
		phrase := tsqueryPhrase(token.words, token.prefix, token.weight)
		if phrase == "" {
			return "", fmt.Errorf("%s holds no word to search for", token.describe())
		}
		return phrase, nil
	default:
		return "", fmt.Errorf("unexpected %s where a word or a phrase was expected", token.describe())
	}
}
