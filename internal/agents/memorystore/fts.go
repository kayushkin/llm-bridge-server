package memorystore

import (
	"strings"
	"unicode"
)

// Search ranks on the search_document column of memories, a tsvector Postgres
// keeps from the content and summary (migrations/0001). It is a generated
// column, so it changes in the same statement as the text it describes: no
// separate index can fall behind a save, be keyed to the wrong row, or need a
// backfill when a store opens. The FTS5 index the SQLite store kept had to be
// written beside every save and could be renumbered away from its rows by a
// VACUUM.
//
// The text search configuration memory_text stems every word with the
// Snowball English stemmer and drops none, as tokenize='porter unicode61'
// did, which is what makes "scheduling" find "scheduler".

// tsqueryLiteral writes one lexeme as a tsquery operand. A quoted operand is
// read as the lexeme itself, so a word the query language would treat as an
// operator (or, a quote, a backslash, a colon) is only ever a word. Inside the
// quotes a quote is doubled and a backslash escaped.
func tsqueryLiteral(lexeme string) string {
	escaped := strings.ReplaceAll(lexeme, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `''`)
	return `'` + escaped + `'`
}

// anyOfTheLexemes is the tsquery a memory must match to be a candidate: any
// one of the lexemes, as FTS5 MATCH read the OR of the quoted words.
func anyOfTheLexemes(lexemes []string) string {
	operands := make([]string, len(lexemes))
	for index, lexeme := range lexemes {
		operands[index] = tsqueryLiteral(lexeme)
	}
	return strings.Join(operands, " | ")
}

// tokenizeForSearch splits a query into the terms to ask the index for:
// lowercase, alphanumeric, stop words removed, every length kept.
//
// It is a deliberate departure from tokenize(): that one drops every token of
// three characters or fewer, because a 256-bucket hash needs a small
// vocabulary to collide less. BM25 has no such need — it weights a term by how
// rare it is, so a short term is ranked, not excluded — and dropping them
// would lose "ssh", "api", "db" and every other short technical name from the
// query side while the index still carries them.
//
// Stop words stay dropped. Under OR semantics a stop word does not merely rank
// low, it ADMITS every document containing it into the candidate set, so the
// cost of keeping them is paid in candidates rather than in ranking.
//
// Splitting on every rune that is not a letter or a number is what keeps the
// query data rather than syntax: no operator, quote or parenthesis survives
// into a term.
func tokenizeForSearch(text string) []string {
	var tokens []string
	var current strings.Builder

	flush := func() {
		if current.Len() == 0 {
			return
		}
		token := strings.ToLower(current.String())
		current.Reset()
		if !isStopWord(token) {
			tokens = append(tokens, token)
		}
	}

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()

	return tokens
}
