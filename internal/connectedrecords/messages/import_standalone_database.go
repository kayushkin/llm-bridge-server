package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the import's errors.
const standaloneStoreName = "message-store"

// StandaloneImportCounts says how many rows of each table
// ImportStandaloneDatabase copied. (ImportCounts is already the name of what
// one batch of POST /imports/{id}/batches wrote.)
type StandaloneImportCounts struct {
	Imports            int64 `json:"imports"`
	Accounts           int64 `json:"accounts"`
	Conversations      int64 `json:"conversations"`
	Participants       int64 `json:"participants"`
	Messages           int64 `json:"messages"`
	ConversationDays   int64 `json:"conversation_days"`
	SupersededMessages int64 `json:"superseded_messages"`
	MessageRevisions   int64 `json:"message_revisions"`
	ConversationMerges int64 `json:"conversation_merges"`
}

// Summary is the counts in one line, and nothing of the records.
func (counts StandaloneImportCounts) Summary() string {
	return fmt.Sprintf("imports=%d accounts=%d conversations=%d participants=%d messages=%d conversation_days=%d superseded_messages=%d message_revisions=%d conversation_merges=%d",
		counts.Imports, counts.Accounts, counts.Conversations, counts.Participants, counts.Messages, counts.ConversationDays,
		counts.SupersededMessages, counts.MessageRevisions, counts.ConversationMerges)
}

// standaloneRecordNumber reads a standalone row's number and checks its id is
// the one that number makes: the import keeps ids by their number and will
// not change one.
func standaloneRecordNumber(prefix string, seqValue, idValue any) (int64, error) {
	seq, err := standalonesqlite.Integer("seq", seqValue)
	if err != nil {
		return 0, err
	}
	id, err := standalonesqlite.Text("id", idValue)
	if err != nil {
		return 0, err
	}
	if want := postgresstorage.FormatRecordID(prefix, seq); id != want {
		return 0, fmt.Errorf("id %q has number %d, whose id is %s", id, seq, want)
	}
	return seq, nil
}

// standaloneReference reads a column that names another row by its public id.
func standaloneReference(column, prefix string, value any) (int64, error) {
	id, err := standalonesqlite.Text(column, value)
	if err != nil {
		return 0, err
	}
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", column, err)
	}
	return number, nil
}

// standaloneOptionalImport reads imported_by: an import id, or ” for none.
func standaloneOptionalImport(value any) (any, error) {
	id, err := standalonesqlite.Text("imported_by", value)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, nil
	}
	number, err := postgresstorage.ParseRecordID(ImportIDPrefix, id)
	if err != nil {
		return nil, fmt.Errorf("imported_by: %w", err)
	}
	return number, nil
}

// standaloneJSON reads a column that holds JSON text and refuses one that does
// not, keeping the text byte for byte.
func standaloneJSON(column string, value any) (string, error) {
	text, err := standalonesqlite.Text(column, value)
	if err != nil {
		return "", err
	}
	if !json.Valid([]byte(text)) {
		return "", fmt.Errorf("%s is not JSON", column)
	}
	return text, nil
}

// booleans converts the named positions of row from 0 or 1 to a boolean.
func booleans(row []any, columns []string, positions ...int) error {
	for _, position := range positions {
		value, err := standalonesqlite.Boolean(columns[position], row[position])
		if err != nil {
			return err
		}
		row[position] = value
	}
	return nil
}

// standaloneTables are the tables of the standalone file in the order they
// are copied (each after what it points at), with the live schema's columns:
// schema.sql's and the ones the standalone Open added.
func standaloneTables(owner string) []standalonesqlite.Table {
	conversationColumns := []string{"seq", "id", "platform", "source", "source_ref", "title", "is_group", "parent_source_ref", "created_at", "updated_at"}
	participantColumns := []string{"conversation_id", "handle", "display_name", "person_id", "is_self"}
	messageColumns := []string{"seq", "id", "conversation_id", "sent_at", "sender_handle", "kind", "body", "body_source", "attachments", "embeds", "buttons",
		"reply_to_source_ref", "call_seconds", "edited", "deleted_on_platform", "source", "source_ref", "source_hash", "imported_by", "created_at", "updated_at"}
	return []standalonesqlite.Table{
		{
			Name:    "imports",
			Columns: []string{"seq", "id", "importer", "description", "started_at", "finished_at", "counts"},
			OrderBy: "seq",
			Insert: `INSERT INTO imports (seq, owner_principal_id, importer, description, started_at, finished_at, counts)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(ImportIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				counts, err := standaloneJSON("counts", row[6])
				if err != nil {
					return nil, err
				}
				return []any{seq, owner, row[2], row[3], row[4], row[5], counts}, nil
			},
		},
		{
			Name:    "accounts",
			Columns: []string{"seq", "id", "platform", "handle", "display_name", "kind", "credential_id", "created_at", "updated_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO accounts (seq, owner_principal_id, platform, handle, display_name, kind, credential_id, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(AccountIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner}, row[2:]...), nil
			},
		},
		{
			Name:    "conversations",
			Columns: conversationColumns,
			OrderBy: "seq",
			Insert: `INSERT INTO conversations (seq, owner_principal_id, platform, source, source_ref, title, is_group, parent_source_ref, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(ConversationIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				if err := booleans(row, conversationColumns, 6); err != nil {
					return nil, err
				}
				return append([]any{seq, owner}, row[2:]...), nil
			},
		},
		{
			Name:    "participants",
			Columns: participantColumns,
			OrderBy: "conversation_id, handle",
			Insert: `INSERT INTO participants (owner_principal_id, conversation_seq, handle, display_name, person_id, is_self)
				VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				conversationSeq, err := standaloneReference("conversation_id", ConversationIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				if err := booleans(row, participantColumns, 4); err != nil {
					return nil, err
				}
				return append([]any{owner, conversationSeq}, row[1:]...), nil
			},
		},
		{
			Name:    "messages",
			Columns: messageColumns,
			OrderBy: "seq",
			Insert: `INSERT INTO messages (seq, owner_principal_id, conversation_seq, sent_at, sender_handle, kind, body, body_source, attachments, embeds, buttons,
				reply_to_source_ref, call_seconds, edited, deleted_on_platform, source, source_ref, source_hash, import_seq, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(MessageIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				conversationSeq, err := standaloneReference("conversation_id", ConversationIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				for _, position := range []int{8, 9, 10} {
					if row[position], err = standaloneJSON(messageColumns[position], row[position]); err != nil {
						return nil, err
					}
				}
				if err := booleans(row, messageColumns, 13, 14); err != nil {
					return nil, err
				}
				importSeq, err := standaloneOptionalImport(row[18])
				if err != nil {
					return nil, err
				}
				values := append([]any{seq, owner, conversationSeq}, row[3:18]...)
				return append(values, importSeq, row[19], row[20]), nil
			},
		},
		{
			Name: "conversation_days",
			Columns: []string{"seq", "id", "conversation_id", "day", "time_zone", "starts_at", "ends_at", "journal_source", "journal_source_ref",
				"journal_entry_id", "created_at", "updated_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO conversation_days (seq, owner_principal_id, conversation_seq, day, time_zone, starts_at, ends_at, journal_source, journal_source_ref,
				journal_entry_id, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(ConversationDayIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				conversationSeq, err := standaloneReference("conversation_id", ConversationIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner, conversationSeq}, row[3:]...), nil
			},
		},
		{
			Name:    "superseded_messages",
			Columns: []string{"id", "source", "source_ref", "superseded_by", "import_id", "superseded_at", "row_json"},
			OrderBy: "id",
			Insert: `INSERT INTO superseded_messages (message_seq, owner_principal_id, source, source_ref, superseded_by_seq, import_seq, superseded_at, row_json)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneReference("id", MessageIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				bySeq, err := standaloneReference("superseded_by", MessageIDPrefix, row[3])
				if err != nil {
					return nil, err
				}
				importSeq, err := standaloneReference("import_id", ImportIDPrefix, row[4])
				if err != nil {
					return nil, err
				}
				rowJSON, err := standaloneJSON("row_json", row[6])
				if err != nil {
					return nil, err
				}
				return []any{seq, owner, row[1], row[2], bySeq, importSeq, row[5], rowJSON}, nil
			},
		},
		{
			Name:    "message_revisions",
			Columns: []string{"seq", "message_id", "import_id", "revised_at", "row_json"},
			OrderBy: "seq",
			Insert: `INSERT INTO message_revisions (seq, owner_principal_id, message_seq, import_seq, revised_at, row_json)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standalonesqlite.Integer("seq", row[0])
				if err != nil {
					return nil, err
				}
				messageSeq, err := standaloneReference("message_id", MessageIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				importSeq, err := standaloneReference("import_id", ImportIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				rowJSON, err := standaloneJSON("row_json", row[4])
				if err != nil {
					return nil, err
				}
				return []any{seq, owner, messageSeq, importSeq, row[3], rowJSON}, nil
			},
		},
		{
			Name:    "conversation_merges",
			Columns: []string{"conversation_id", "source", "source_ref", "merged_into", "rule", "import_id", "merged_at", "messages_moved", "row_json"},
			OrderBy: "conversation_id",
			Insert: `INSERT INTO conversation_merges (conversation_seq, owner_principal_id, source, source_ref, merged_into_seq, rule, import_seq, merged_at, messages_moved, row_json)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneReference("conversation_id", ConversationIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				intoSeq, err := standaloneReference("merged_into", ConversationIDPrefix, row[3])
				if err != nil {
					return nil, err
				}
				importSeq, err := standaloneReference("import_id", ImportIDPrefix, row[5])
				if err != nil {
					return nil, err
				}
				rowJSON, err := standaloneJSON("row_json", row[8])
				if err != nil {
					return nil, err
				}
				return []any{seq, owner, row[1], row[2], intoSeq, row[4], importSeq, row[6], row[7], rowJSON}, nil
			},
		},
	}
}

// numbering is one identity column to number on after the import, from the
// highest number its table and the table of its retired numbers hold.
type numbering struct {
	table, highest string
}

var numberings = []numbering{
	{"imports", `SELECT COALESCE(MAX(seq), 0) FROM imports`},
	{"accounts", `SELECT COALESCE(MAX(seq), 0) FROM accounts`},
	{"conversations", `SELECT GREATEST((SELECT COALESCE(MAX(seq), 0) FROM conversations), (SELECT COALESCE(MAX(conversation_seq), 0) FROM conversation_merges))`},
	{"messages", `SELECT GREATEST((SELECT COALESCE(MAX(seq), 0) FROM messages), (SELECT COALESCE(MAX(message_seq), 0) FROM superseded_messages))`},
	{"conversation_days", `SELECT COALESCE(MAX(seq), 0) FROM conversation_days`},
	{"message_revisions", `SELECT COALESCE(MAX(seq), 0) FROM message_revisions`},
}

// standaloneRevisionCounter is the highest message_revisions number the
// standalone file ever handed out, which its AUTOINCREMENT keeps in
// sqlite_sequence; 0 when it holds none.
func standaloneRevisionCounter(ctx context.Context, source *sql.DB) (int64, error) {
	var tables int
	if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 0 {
		return 0, nil
	}
	var counter int64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'message_revisions'`).Scan(&counter)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return counter, err
}

// ImportStandaloneDatabase copies every row of a standalone message-store's
// SQLite file into the module behind db, as it is — numbers, ids, times, text
// and the JSON of retired and revised rows — with every row owned by
// ownerPrincipalID. It runs in one transaction, so it copies everything or
// nothing.
//
// It refuses an owner that is not a principal-store id, a module that already
// holds any row (so it can never mix two stores or run twice), and a file it
// cannot copy whole: a table with a column it does not know, an id that is not
// the form its number makes, a row pointing at one the file does not hold, or
// a live row holding a number the file also lists as retired. A partial copy
// imports when it holds, for every row it keeps, the rows that row points at:
// its conversation, the imports it names, a superseded message's live
// message and a merged conversation's survivor. Afterwards each numbered
// table numbers on from the highest number copied, retired numbers included,
// so no id that chats, notes and links already carry is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (StandaloneImportCounts, error) {
	var counts StandaloneImportCounts
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return counts, err
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	tables := standaloneTables(ownerPrincipalID)
	tableNames := make([]string, 0, len(tables))
	for _, table := range tables {
		if err := standalonesqlite.CheckColumns(ctx, source, standaloneStoreName, table.Name, table.Columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		tableNames = append(tableNames, table.Name)
	}
	revisionCounter, err := standaloneRevisionCounter(ctx, source)
	if err != nil {
		return counts, fmt.Errorf("%s: reading the revision counter: %w", sqlitePath, err)
	}
	copied := map[string]int64{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "messages", tableNames...); err != nil {
			return err
		}
		for _, table := range tables {
			count, err := standalonesqlite.Copy(ctx, source, tx, table)
			if err != nil {
				return err
			}
			copied[table.Name] = count
		}
		for _, reuse := range []struct{ query, what string }{
			{`SELECT COUNT(*) FROM messages m JOIN superseded_messages s ON s.message_seq = m.seq`, "live messages hold the number of a superseded message"},
			{`SELECT COUNT(*) FROM conversations c JOIN conversation_merges g ON g.conversation_seq = c.seq`, "live conversations hold the number of a merged one"},
		} {
			var reused int64
			if err := tx.QueryRowContext(ctx, reuse.query).Scan(&reused); err != nil {
				return err
			}
			if reused > 0 {
				return fmt.Errorf("%d %s; the standalone store repairs that at its start, so start it once before copying", reused, reuse.what)
			}
		}
		for _, numbered := range numberings {
			var last int64
			if err := tx.QueryRowContext(ctx, numbered.highest).Scan(&last); err != nil {
				return fmt.Errorf("reading the highest number in %s: %w", numbered.table, err)
			}
			if numbered.table == "message_revisions" && revisionCounter > last {
				last = revisionCounter
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbered.table, "seq", last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return StandaloneImportCounts{}, fmt.Errorf("importing %s: %w", sqlitePath, err)
	}
	return StandaloneImportCounts{
		Imports:            copied["imports"],
		Accounts:           copied["accounts"],
		Conversations:      copied["conversations"],
		Participants:       copied["participants"],
		Messages:           copied["messages"],
		ConversationDays:   copied["conversation_days"],
		SupersededMessages: copied["superseded_messages"],
		MessageRevisions:   copied["message_revisions"],
		ConversationMerges: copied["conversation_merges"],
	}, nil
}
