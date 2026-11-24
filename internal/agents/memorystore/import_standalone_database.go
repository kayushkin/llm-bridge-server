package memorystore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneColumns are the columns of each table of the SQLite database the
// standalone memory-store kept, once its start-up migrations had run. The
// import refuses a file whose tables have any other columns: a column it does
// not know is data it would drop, and a missing one is a file the store never
// finished migrating. Open such a file with the standalone store once, so it
// migrates it, and import the result.
var standaloneColumns = map[string][]string{
	"memories": {"id", "content", "summary", "original_id", "importance", "access_count", "last_accessed",
		"created_at", "source", "embedding", "always_load", "expires_at", "tokens", "ref_type", "ref_target",
		"is_lazy", "orchestrator"},
	"memory_tags":  {"memory_id", "tag"},
	"memory_usage": {"id", "memory_id", "session_id", "turn_number", "usage_type", "accessed_at"},
}

// standaloneDerivedTable says whether a table of the SQLite file holds nothing
// of its own: the FTS5 index (memories_fts and its shadow tables), which
// Postgres rebuilds from the rows as the search_document column, and SQLite's
// own record of AUTOINCREMENT numbers, which the import reads separately.
func standaloneDerivedTable(name string) bool {
	return name == "sqlite_sequence" || name == "memories_fts" || strings.HasPrefix(name, "memories_fts_")
}

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Memories          int64 `json:"memories"`
	Tags              int64 `json:"tags"`
	Usages            int64 `json:"usages"`
	LastUsageNumber   int64 `json:"last_usage_number"`
	ForgottenMemories int64 `json:"forgotten_memories"`
}

// ImportStandaloneDatabase copies every memory, tag and usage row from the
// SQLite file of a standalone memory-store into the module behind db, as they
// are: memory ids, times, importance, embeddings, flags, tags in the order
// they were written, and usage rows with their numbers. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// stores or run twice, and it refuses a file it cannot copy whole: a table it
// does not know, a table with a column it does not know or lacks one it needs,
// a flag that is not 0 or 1, or a row the schema refuses (a tag or usage of a
// memory the file does not hold). Afterwards usage rows number on from the
// highest number the standalone store handed out.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	if err := refuseUnknownStandaloneTables(ctx, source, sqlitePath); err != nil {
		return counts, err
	}
	for table, want := range standaloneColumns {
		if err := standalonesqlite.CheckColumns(ctx, source, "memory-store", table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	if err := source.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'memory_usage'), 0),
		(SELECT COALESCE(MAX(id), 0) FROM memory_usage))`).Scan(&counts.LastUsageNumber); err != nil {
		return counts, fmt.Errorf("reading the last usage number the standalone store handed out: %w", err)
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "memories", "memories", "memory_tags", "memory_usage"); err != nil {
			return err
		}
		var err error
		counts.Memories, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "memories", Columns: standaloneColumns["memories"], OrderBy: "id",
			Insert: `INSERT INTO memories (id, content, summary, original_id, importance, access_count, last_accessed,
				created_at, source, embedding, always_load, expires_at, tokens, ref_type, ref_target, is_lazy, orchestrator)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			Convert: convertStandaloneMemory,
		})
		if err != nil {
			return err
		}
		// rowid is the order the standalone store wrote each memory's tags in,
		// which is the order it read them back in.
		counts.Tags, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "memory_tags", Columns: standaloneColumns["memory_tags"], OrderBy: "rowid",
			Insert: `INSERT INTO memory_tags (memory_id, tag) VALUES ($1, $2)`,
			Convert: func(row []any) ([]any, error) {
				return textColumns(standaloneColumns["memory_tags"], row)
			},
		})
		if err != nil {
			return err
		}
		counts.Usages, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "memory_usage", Columns: standaloneColumns["memory_usage"], OrderBy: "id",
			Insert: `INSERT INTO memory_usage (id, memory_id, session_id, turn_number, usage_type, accessed_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: convertStandaloneUsage,
		})
		if err != nil {
			return err
		}
		if err := postgresstorage.ContinueNumberingAfter(ctx, tx, "memory_usage", "id", counts.LastUsageNumber); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE importance = 0`).Scan(&counts.ForgottenMemories)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// refuseUnknownStandaloneTables refuses a file holding a table the import
// does not copy and cannot rebuild: its rows would be lost without a word.
func refuseUnknownStandaloneTables(ctx context.Context, source *sql.DB, sqlitePath string) error {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
		}
		if _, copied := standaloneColumns[name]; copied || standaloneDerivedTable(name) {
			continue
		}
		return fmt.Errorf("%s has a table %s, which the import does not know; it copies only a file it can copy whole", sqlitePath, name)
	}
	return rows.Err()
}

func convertStandaloneMemory(row []any) ([]any, error) {
	columns := standaloneColumns["memories"]
	values := make([]any, len(row))
	for index, column := range columns {
		value := row[index]
		var err error
		switch column {
		case "id", "content", "source", "ref_type", "orchestrator":
			values[index], err = standalonesqlite.Text(column, value)
		case "summary", "original_id", "ref_target":
			values[index], err = nullableText(column, value)
		case "importance":
			values[index], err = realNumber(column, value)
		case "access_count", "last_accessed", "created_at", "tokens":
			values[index], err = standalonesqlite.Integer(column, value)
		case "expires_at":
			if value == nil {
				values[index] = nil
			} else {
				values[index], err = standalonesqlite.Integer(column, value)
			}
		case "always_load", "is_lazy":
			values[index], err = standalonesqlite.Boolean(column, value)
		case "embedding":
			values[index], err = embeddingBytes(column, value)
		default:
			return nil, fmt.Errorf("the import has no rule for column %s", column)
		}
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func convertStandaloneUsage(row []any) ([]any, error) {
	columns := standaloneColumns["memory_usage"]
	values := make([]any, len(row))
	for index, column := range columns {
		var err error
		switch column {
		case "id", "turn_number", "accessed_at":
			values[index], err = standalonesqlite.Integer(column, row[index])
		default:
			values[index], err = standalonesqlite.Text(column, row[index])
		}
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func textColumns(columns []string, row []any) ([]any, error) {
	values := make([]any, len(row))
	for index, column := range columns {
		text, err := standalonesqlite.Text(column, row[index])
		if err != nil {
			return nil, err
		}
		values[index] = text
	}
	return values, nil
}

// nullableText reads a TEXT column that may be NULL, keeping the NULL.
func nullableText(column string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	return standalonesqlite.Text(column, value)
}

// realNumber reads a REAL column. SQLite keeps a whole number written to it as an
// integer when it reads back exactly, so both are taken.
func realNumber(column string, value any) (float64, error) {
	switch number := value.(type) {
	case float64:
		return number, nil
	case int64:
		return float64(number), nil
	}
	return 0, fmt.Errorf("%s is %T %v, not a number", column, value, value)
}

// embeddingBytes reads the embedding column, the JSON text of the vector the
// store wrote as bytes. NULL stays NULL.
func embeddingBytes(column string, value any) (any, error) {
	switch embedding := value.(type) {
	case nil:
		return nil, nil
	case []byte:
		return embedding, nil
	case string:
		return []byte(embedding), nil
	}
	return nil, fmt.Errorf("%s is %T, not the bytes of a vector", column, value)
}
