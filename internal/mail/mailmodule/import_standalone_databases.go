package mailmodule

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// The schema versions of the standalone store's two files that this import
// reads, as their user_version says. An older file has columns this import
// does not know where to put; the standalone store brings a file up to date
// when it opens it, so a copy of a running store's file is at these versions.
const (
	standaloneAccountsSchemaVersion  = 2
	standaloneLocalMailSchemaVersion = 4
)

// ImportCounts is how many rows an import copied, per table, and where the
// numbering of the two numbered tables goes on from.
type ImportCounts struct {
	Accounts                  int64 `json:"accounts"`
	IMAPLogins                int64 `json:"imap_creds"`
	IMAPFolderMaps            int64 `json:"imap_folder_map"`
	IndexedMessages           int64 `json:"messages"`
	RecordedBodies            int64 `json:"message_bodies"`
	LocalMessages             int64 `json:"local_messages"`
	LocalMessageBodies        int64 `json:"local_message_bodies"`
	LocalImportSources        int64 `json:"local_import_sources"`
	LocalMessageRevisions     int64 `json:"local_message_revisions"`
	LocalMessageAttachments   int64 `json:"local_message_attachments"`
	IngressCredentials        int64 `json:"ingress_credentials"`
	IngressCredentialAccounts int64 `json:"ingress_credential_accounts"`
	DeliveryReceipts          int64 `json:"delivery_receipts"`
	MessagePurges             int64 `json:"message_purges"`
	MessageEvents             int64 `json:"message_events"`
	// The next receipt id and outbox sequence number come after these, so
	// none the standalone store handed out is handed out again.
	LastReceiptID     int64 `json:"last_receipt_id"`
	LastEventSequence int64 `json:"last_event_seq"`
	LastRecordedBody  int64 `json:"last_message_bodies_id"`
	// RepairedText counts, by table.column, the values that were not text
	// Postgres can hold and were copied with U+FFFD in place of the bytes
	// that were not UTF-8 (TextPostgresCanHold).
	RepairedText map[string]int64 `json:"repaired_text"`
}

// copiedTable is one table copied whole, with its columns in the same order on
// both sides.
type copiedTable struct {
	name    string
	columns []string
	// orderBy orders the read, so a failure names the same row each time.
	orderBy string
	// overridingSystemValue inserts the source's numbers into a GENERATED
	// ALWAYS identity column.
	overridingSystemValue bool
	count                 *int64
}

func (table copiedTable) insert() string {
	placeholders := make([]string, len(table.columns))
	for index := range placeholders {
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	overriding := ""
	if table.overridingSystemValue {
		overriding = " OVERRIDING SYSTEM VALUE"
	}
	return "INSERT INTO " + table.name + " (" + strings.Join(table.columns, ", ") + ")" + overriding +
		" VALUES (" + strings.Join(placeholders, ", ") + ")"
}

func accountsTables(counts *ImportCounts) []copiedTable {
	return []copiedTable{
		{name: "accounts", orderBy: "id", count: &counts.Accounts, columns: []string{
			"id", "label", "provider", "address", "apiauth_provider", "apiauth_account", "imap_cred_name",
			"color", "signature", "enabled", "sort_order", "created_at", "updated_at"}},
		{name: "imap_creds", orderBy: "name", count: &counts.IMAPLogins, columns: []string{
			"name", "address", "imap_host", "imap_port", "smtp_host", "smtp_port", "username", "password",
			"tls_mode", "created_at", "updated_at"}},
		{name: "imap_folder_map", orderBy: "account_id, canonical", count: &counts.IMAPFolderMaps, columns: []string{
			"account_id", "canonical", "folder_id"}},
		{name: "messages", orderBy: "account_id, folder_scope, provider_id", count: &counts.IndexedMessages, columns: []string{
			"account_id", "folder_scope", "provider_id", "message_id", "thread_id", "root_message_id", "in_reply_to",
			"references_ids", "subject", "from_name", "from_email", "date_ms", "folder_id", "uid_validity",
			"first_seen_ms", "last_seen_ms"}},
		// The search columns are left to the trigger, which builds them from
		// the copied text.
		{name: "message_bodies", orderBy: "id", count: &counts.RecordedBodies, columns: []string{
			"id", "account_id", "folder_scope", "provider_id", "message_id", "subject", "from_name", "from_email",
			"date_ms", "body_text", "body_html", "search_text", "fetched_ms"}},
	}
}

func localMailTables(counts *ImportCounts) []copiedTable {
	return []copiedTable{
		{name: "local_messages", orderBy: "account_id, id", count: &counts.LocalMessages, columns: []string{
			"account_id", "id", "folder_id", "message_id", "thread_id", "in_reply_to", "references_ids", "subject",
			"from_name", "from_email", "to_addrs", "cc_addrs", "date_ms", "snippet", "unread", "flagged", "created_at",
			"text_bytes", "html_bytes", "bcc_addrs", "provider_message_id", "source_revision", "outlook_store_id",
			"outlook_folder_id", "outlook_folder_path", "revision", "content_sha256", "updated_at"}},
		{name: "local_message_bodies", orderBy: "account_id, id", count: &counts.LocalMessageBodies, columns: []string{
			"account_id", "id", "body_text", "body_html"}},
		{name: "local_import_sources", orderBy: "account_id, source_ref", count: &counts.LocalImportSources, columns: []string{
			"account_id", "source_ref", "source_sha256", "message_count", "imported_at"}},
		{name: "local_message_revisions", orderBy: "account_id, id, revision", count: &counts.LocalMessageRevisions, columns: []string{
			"account_id", "id", "revision", "metadata_json", "body_text", "body_html", "replaced_at"}},
		{name: "local_message_attachments", orderBy: "account_id, id, revision, attachment_id", count: &counts.LocalMessageAttachments, columns: []string{
			"account_id", "id", "revision", "attachment_id", "position", "source_attachment_id", "filename",
			"content_type", "size_bytes", "sha256", "content_id", "is_inline", "file_id"}},
		{name: "ingress_credentials", orderBy: "credential_id", count: &counts.IngressCredentials, columns: []string{
			"credential_id", "token_sha256", "source", "note", "created_at", "revoked_at"}},
		{name: "ingress_credential_accounts", orderBy: "credential_id, account_id", count: &counts.IngressCredentialAccounts, columns: []string{
			"credential_id", "account_id"}},
		{name: "delivery_receipts", orderBy: "receipt_id", count: &counts.DeliveryReceipts, overridingSystemValue: true, columns: []string{
			"receipt_id", "credential_id", "source", "delivery_id", "account_id", "provider_message_id", "source_revision",
			"content_sha256", "status", "message_id", "revision", "detail", "received_at"}},
		{name: "message_purges", orderBy: "account_id, provider_message_id", count: &counts.MessagePurges, columns: []string{
			"account_id", "provider_message_id", "message_id", "reason", "purged_at"}},
		{name: "message_events", orderBy: "seq", count: &counts.MessageEvents, overridingSystemValue: true, columns: []string{
			"seq", "kind", "origin", "account_id", "id", "provider_message_id", "revision", "created_at"}},
	}
}

// ImportStandaloneDatabases copies a standalone mailstack's two SQLite files —
// accounts.db, and local-mail.db when the store had one — into an empty mail
// module, in one transaction, keeping every row, id and time. localMailPath
// empty means the store never used a local account and so never made the
// file. It refuses a module that already holds a row, a file at another
// schema version, and a table whose columns it does not know.
//
// Attachment bytes are not in either file: the standalone store kept them in
// file-store, and local_message_attachments names each by its file-store id,
// which file-store's own import keeps.
func ImportStandaloneDatabases(ctx context.Context, pool *sql.DB, accountsPath, localMailPath string) (ImportCounts, error) {
	counts := ImportCounts{RepairedText: map[string]int64{}}
	accountsSource, err := openStandaloneFile(ctx, accountsPath, "accounts", standaloneAccountsSchemaVersion, accountsTables(&counts))
	if err != nil {
		return ImportCounts{}, err
	}
	defer accountsSource.Close()
	var localSource *sql.DB
	if localMailPath != "" {
		localSource, err = openStandaloneFile(ctx, localMailPath, "local mail", standaloneLocalMailSchemaVersion, localMailTables(&counts))
		if err != nil {
			return ImportCounts{}, err
		}
		defer localSource.Close()
	}

	err = postgresstorage.InTransaction(ctx, pool, func(tx *sql.Tx) error {
		var every []string
		for _, table := range append(accountsTables(&counts), localMailTables(&counts)...) {
			every = append(every, table.name)
		}
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "mail", every...); err != nil {
			return err
		}
		if err := copyTables(ctx, accountsSource, tx, accountsTables(&counts), counts.RepairedText); err != nil {
			return err
		}
		if counts.LastRecordedBody, err = lastNumber(ctx, accountsSource, "message_bodies", "id"); err != nil {
			return err
		}
		if err := postgresstorage.ContinueNumberingAfter(ctx, tx, "message_bodies", "id", counts.LastRecordedBody); err != nil {
			return err
		}
		if localSource == nil {
			return nil
		}
		if err := copyTables(ctx, localSource, tx, localMailTables(&counts), counts.RepairedText); err != nil {
			return err
		}
		if counts.LastReceiptID, err = lastNumber(ctx, localSource, "delivery_receipts", "receipt_id"); err != nil {
			return err
		}
		if err := postgresstorage.ContinueNumberingAfter(ctx, tx, "delivery_receipts", "receipt_id", counts.LastReceiptID); err != nil {
			return err
		}
		if counts.LastEventSequence, err = lastNumber(ctx, localSource, "message_events", "seq"); err != nil {
			return err
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "message_events", "seq", counts.LastEventSequence)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// openStandaloneFile opens one of the two files read only and refuses it
// unless it is at version and every table has exactly the columns the import
// copies.
func openStandaloneFile(ctx context.Context, path, what string, version int, tables []copiedTable) (*sql.DB, error) {
	source, err := standalonesqlite.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the %s database: %w", what, err)
	}
	var found int
	if err := source.QueryRowContext(ctx, "PRAGMA user_version").Scan(&found); err != nil {
		source.Close()
		return nil, fmt.Errorf("the %s database: reading its schema version: %w", what, err)
	}
	if found != version {
		source.Close()
		return nil, fmt.Errorf("the %s database is at schema version %d, and the import reads version %d; open it once with the standalone mailstack, which brings it up to date, and copy it again", what, found, version)
	}
	for _, table := range tables {
		if err := standalonesqlite.CheckColumns(ctx, source, "mailstack "+what, table.name, table.columns); err != nil {
			source.Close()
			return nil, fmt.Errorf("the %s database: %w", what, err)
		}
	}
	return source, nil
}

func copyTables(ctx context.Context, source *sql.DB, tx *sql.Tx, tables []copiedTable, repaired map[string]int64) error {
	for _, table := range tables {
		copied, err := standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name:    table.name,
			Columns: table.columns,
			OrderBy: table.orderBy,
			Insert:  table.insert(),
			Convert: func(row []any) ([]any, error) { return textPostgresCanHold(table, row, repaired), nil },
		})
		if err != nil {
			return err
		}
		*table.count = copied
	}
	return nil
}

// textPostgresCanHold passes a row on as SQLite gave it, except that every
// text value — including one the driver read as bytes, since every column
// the store wrote is TEXT or INTEGER — goes through TextPostgresCanHold, and
// each one it changed is counted.
func textPostgresCanHold(table copiedTable, row []any, repaired map[string]int64) []any {
	converted := make([]any, len(row))
	for index, value := range row {
		var text string
		switch typed := value.(type) {
		case []byte:
			text = string(typed)
		case string:
			text = typed
		default:
			converted[index] = value
			continue
		}
		held, changed := TextPostgresCanHold(text)
		if changed {
			repaired[table.name+"."+table.columns[index]]++
		}
		converted[index] = held
	}
	return converted
}

// lastNumber is the highest number the standalone store handed out in table:
// its AUTOINCREMENT counter when it has one, which remembers rows that were
// deleted, and otherwise the highest in the table.
func lastNumber(ctx context.Context, source *sql.DB, table, column string) (int64, error) {
	var highest sql.NullInt64
	if err := source.QueryRowContext(ctx, "SELECT MAX("+column+") FROM "+table).Scan(&highest); err != nil {
		return 0, fmt.Errorf("the highest %s.%s: %w", table, column, err)
	}
	var hasSequences int
	if err := source.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'").Scan(&hasSequences); err != nil {
		return 0, fmt.Errorf("looking for sqlite_sequence: %w", err)
	}
	if hasSequences > 0 {
		var counter sql.NullInt64
		err := source.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&counter)
		if err != nil && err != sql.ErrNoRows {
			return 0, fmt.Errorf("the AUTOINCREMENT counter of %s: %w", table, err)
		}
		if counter.Valid && counter.Int64 > highest.Int64 {
			return counter.Int64, nil
		}
	}
	return highest.Int64, nil
}
