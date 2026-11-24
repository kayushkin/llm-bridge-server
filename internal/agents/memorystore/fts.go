package memorystore

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// ftsTableName is the full-text index Search ranks on.
const ftsTableName = "memories_fts"

// ftsSchema creates the index.
//
// The row is keyed on `memories.rowid`: an indexed memory's FTS rowid IS the
// rowid of the row it describes. That is what makes both writes into it keyed
// lookups instead of scans — an FTS5 virtual table cannot carry a secondary
// index, so a WHERE on any ordinary column of it is a full scan of the whole
// index. Keyed on an unindexed column, re-saving one memory in a store of n
// costs a scan of n, and seeding 32,767 memories cost 271 seconds of that.
//
// `memory_id` is kept, UNINDEXED and never searched, purely so Search can check
// the rowid mapping still holds; see ftsRowidDriftError.
//
// The porter tokenizer is what makes "scheduling" find "scheduler". unicode61
// alone does no stemming at all, which is SQLite's default and the setting that
// would quietly cost recall on ordinary English.
const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS ` + ftsTableName + ` USING fts5(
	memory_id UNINDEXED,
	content,
	summary,
	tokenize='porter unicode61'
);`

// ⚠️ VACUUM is the one operation that breaks the rowid key.
//
// `memories` is declared with `id TEXT PRIMARY KEY`, so its rowid is not an
// INTEGER PRIMARY KEY alias, and SQLite is explicitly free to renumber such
// rowids during a VACUUM. An FTS5 rowid is backed by an INTEGER PRIMARY KEY and
// is not renumbered, so a VACUUM moves one side of the mapping and not the
// other, and every search then answers with the wrong memory.
//
// Nothing on this box vacuums this database and auto_vacuum is off (the default
// NONE; the DSN sets only busy_timeout). Rather than rely on that staying true,
// Search compares the memory_id stored beside each hit against the id of the
// row it joined to, and refuses rather than answering wrongly. The repair is to
// empty the index and reopen the store, which rebuilds it:
//
//	DELETE FROM memories_fts;
func ftsRowidDriftError(indexedMemoryID, joinedMemoryID string) error {
	return fmt.Errorf(
		"full-text index is keyed to the wrong row: index says %q, the joined memory is %q. "+
			"This is what a VACUUM of the memories table does to the rowid mapping. "+
			"Repair with `DELETE FROM %s;` and reopen the store, which rebuilds the index",
		indexedMemoryID, joinedMemoryID, ftsTableName)
}

// execer is the half of *sql.DB and *sql.Tx that the index writer needs, so a
// Save can index inside its own transaction instead of racing it.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// queryer is the half of *sql.DB and *sql.Tx the index writer reads through.
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// indexMemoryText replaces a memory's row in the full-text index.
//
// Delete-then-insert rather than UPDATE: `memories` is written with an upsert
// whose insert and update halves arrive here as the same call, and deleting a
// row that is not there is not an error, so one path covers both. Both
// statements key on the rowid, so neither scans.
// ftsDeleteStatement and ftsInsertStatement are constants so that
// TestTheIndexWriterDeletesByRowidAndNotByAScan can EXPLAIN the statement this
// function actually runs. Inlined, the test would be asserting a property of
// SQLite and nothing about this code, and changing the WHERE back to memory_id
// would leave it green.
const (
	ftsDeleteStatement = `DELETE FROM ` + ftsTableName + ` WHERE rowid = ?`
	ftsInsertStatement = `INSERT INTO ` + ftsTableName + ` (rowid, memory_id, content, summary) VALUES (?, ?, ?, ?)`
)

func indexMemoryText(db interface {
	execer
	queryer
}, memoryID, content, summary string) error {
	var rowID int64
	if err := db.QueryRow(`SELECT rowid FROM memories WHERE id = ?`, memoryID).Scan(&rowID); err != nil {
		return fmt.Errorf("look up rowid for %s: %w", memoryID, err)
	}
	if _, err := db.Exec(ftsDeleteStatement, rowID); err != nil {
		return fmt.Errorf("clear fts row for %s: %w", memoryID, err)
	}
	if _, err := db.Exec(ftsInsertStatement, rowID, memoryID, content, summary); err != nil {
		return fmt.Errorf("index fts row for %s: %w", memoryID, err)
	}
	return nil
}

// backfillFullTextIndex indexes every memory the index does not already carry.
//
// It runs on every open, not once behind a version flag. A store written by a
// binary that predates the index has every memory to index, and a store written
// by one that has it has none — both are the same statement, and running it
// unconditionally means the answer never depends on a migration marker being
// right. It is also the documented repair for a VACUUMed database.
//
// The NOT EXISTS is a rowid seek per candidate row rather than a scan of the
// index, which is the whole reason the index is keyed on the rowid.
func backfillFullTextIndex(db *sql.DB) error {
	_, err := db.Exec(`
	INSERT INTO ` + ftsTableName + ` (rowid, memory_id, content, summary)
	SELECT m.rowid, m.id, m.content, COALESCE(m.summary, '')
	FROM memories m
	WHERE NOT EXISTS (SELECT 1 FROM ` + ftsTableName + ` f WHERE f.rowid = m.rowid)`)
	if err != nil {
		return fmt.Errorf("backfill fts index: %w", err)
	}
	return nil
}

// ftsMatchExpression turns a free-text query into an FTS5 MATCH expression, or
// returns ok=false when the query carries nothing worth searching for.
//
// Two things are going on and only one of them is escaping.
//
// The escaping half: FTS5 MATCH is a query language, so a raw user string can
// carry `OR`, `NEAR`, `*`, `^`, `-` and parentheses that change what is asked,
// or unbalanced quotes that make it a syntax error rather than a search. Every
// token is emitted as a quoted string, which FTS5 reads as a literal phrase.
//
// The tokenizing half is a deliberate departure from tokenize(): that one drops
// every token of three characters or fewer, because a 256-bucket hash needs a
// small vocabulary to collide less. BM25 has no such need — it weights a term
// by how rare it is, so a short term is ranked, not excluded — and dropping
// them would lose "ssh", "api", "db" and every other short technical name from
// the query side while the index still carries them.
//
// Stop words stay dropped. Under OR semantics a stop word does not merely rank
// low, it ADMITS every document containing it into the candidate set, so the
// cost of keeping them is paid in candidates rather than in ranking.
func ftsMatchExpression(query string) (string, bool) {
	tokens := tokenizeForSearch(query)
	if len(tokens) == 0 {
		return "", false
	}

	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		quoted = append(quoted, `"`+strings.ReplaceAll(token, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR "), true
}

// tokenizeForSearch splits a query into the terms to ask the index for:
// lowercase, alphanumeric, stop words removed, every length kept.
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
