package signin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// standaloneTables are the tables of the SQLite database the standalone
// sign-in-store kept, in the order the import copies them, with their columns.
// The import refuses a file whose tables have any other columns: a column it
// does not know is data it would drop.
var standaloneTables = []struct {
	name    string
	columns []string
}{
	{"sign_in_links", []string{"seq", "secret_sha256", "principal_id", "sent_by", "made_by", "created_at", "expires_at", "redeemed_at", "session_id"}},
	{"sessions", []string{"seq", "secret_sha256", "principal_id", "method", "sign_in_link_id", "created_at", "expires_at", "last_seen_at",
		"revoked_at", "revoked_by", "revoke_reason", "user_agent", "remote_address"}},
	{"discord_attempts", []string{"seq", "state_sha256", "browser_secret_sha256", "purpose", "linking_principal_id", "linking_session_id",
		"return_to", "created_at", "expires_at", "consumed_at"}},
}

// ImportCounts says how many rows of each table an import copied, and the
// highest number in each, after which the module numbers on.
type ImportCounts struct {
	SignInLinks           int   `json:"sign_in_links"`
	Sessions              int   `json:"sessions"`
	DiscordAttempts       int   `json:"discord_attempts"`
	LastSignInLinkSeq     int64 `json:"last_sign_in_link_seq"`
	LastSessionSeq        int64 `json:"last_session_seq"`
	LastDiscordAttemptSeq int64 `json:"last_discord_attempt_seq"`
}

// ImportStandaloneDatabase copies every sign-in link, session and Discord
// attempt from the SQLite file of a standalone sign-in-store into the module
// behind db, as they are: numbers (so every id), secret hashes (so every
// cookie and unspent link still works), times, and redeemed, revoked and
// consumed states. It runs in one transaction, so it copies everything or
// nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// databases or run twice, and a file whose tables have columns it does not
// know. Afterwards each table numbers on from the highest number copied, so no
// id already handed out is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	for _, table := range standaloneTables {
		if err := checkStandaloneColumns(ctx, source, table.name, table.columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range standaloneTables {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table.name, err)
			}
			if held > 0 {
				return fmt.Errorf("the sign_in module already holds %d rows in %s; the import copies into an empty module only", held, table.name)
			}
		}
		copied := []*int{&counts.SignInLinks, &counts.Sessions, &counts.DiscordAttempts}
		last := []*int64{&counts.LastSignInLinkSeq, &counts.LastSessionSeq, &counts.LastDiscordAttemptSeq}
		for index, table := range standaloneTables {
			rowCount, lastSeq, err := copyStandaloneTable(ctx, source, tx, table.name, table.columns)
			if err != nil {
				return err
			}
			*copied[index], *last[index] = rowCount, lastSeq
			// AUTOINCREMENT kept the highest number it ever handed out in
			// sqlite_sequence, which is past the last row if that row was
			// removed; numbering goes on after whichever is higher.
			handedOut, err := standaloneSequence(ctx, source, table.name)
			if err != nil {
				return err
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table.name, "seq", max(lastSeq, handedOut)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// standaloneSequence is the highest number SQLite's AUTOINCREMENT handed out
// for table, 0 when it handed out none.
func standaloneSequence(ctx context.Context, source *sql.DB, table string) (int64, error) {
	var handedOut int64
	err := source.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&handedOut)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the numbering of %s: %w", table, err)
	}
	return handedOut, nil
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("it has no table %s, so it is not a sign-in-store database", table)
	}
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedWant)
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole",
			table, strings.Join(found, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}

// copyStandaloneTable copies every row of one table, in number order, keeping
// each number. Every column of these tables is text or a whole number, and
// each value goes over as the type it was read as.
func copyStandaloneTable(ctx context.Context, source *sql.DB, tx *sql.Tx, table string, columns []string) (copied int, lastSeq int64, err error) {
	columnList := strings.Join(columns, ", ")
	rows, err := source.QueryContext(ctx, "SELECT "+columnList+" FROM "+table+" ORDER BY seq")
	if err != nil {
		return 0, 0, fmt.Errorf("reading %s: %w", table, err)
	}
	defer rows.Close()
	placeholders := make([]string, len(columns))
	for index := range columns {
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	insert := "INSERT INTO " + table + " (" + columnList + ") OVERRIDING SYSTEM VALUE VALUES (" + strings.Join(placeholders, ", ") + ")"
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return 0, 0, fmt.Errorf("reading %s: %w", table, err)
		}
		for index, value := range values {
			switch typed := value.(type) {
			case int64:
			case string:
			case []byte:
				values[index] = string(typed)
			default:
				return 0, 0, fmt.Errorf("%s row %v: column %s holds %T, where the import expects text or a whole number", table, values[0], columns[index], value)
			}
		}
		seq, ok := values[0].(int64)
		if !ok {
			return 0, 0, fmt.Errorf("%s: a row's seq is %v, not a whole number", table, values[0])
		}
		if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
			return 0, 0, fmt.Errorf("copying %s row %d: %w", table, seq, err)
		}
		copied++
		lastSeq = seq
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading %s: %w", table, err)
	}
	return copied, lastSeq, nil
}
