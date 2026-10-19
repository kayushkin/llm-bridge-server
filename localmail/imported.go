package localmail

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/mailstack/backend"
)

// ============================ imported mail ============================
//
// An import puts messages that some other system exported — an Outlook export
// on a support desk's machine, say — into a local account, keyed by the id the
// source gave each message. It differs from Append in three ways that matter to
// the caller:
//
//   - It is idempotent. The same message imported again changes nothing and
//     says so; changed content replaces the stored copy and bumps its revision.
//   - It invents nothing. A message with no Message-ID keeps none, a message
//     with no conversation id has no thread id, and a body the export did not
//     carry stays absent rather than becoming an empty string.
//   - It keeps what the export says about where the message lives at the source
//     (the Outlook store, folder and folder path), because a reply to it has to
//     be sent from there.

// ErrBodyAbsent is returned by Body when the message exists but has no body in
// the format asked for. It is not the same answer as an empty body.
var ErrBodyAbsent = errors.New("localmail: message has no body in that format")

// MaxImportBatch bounds one import call. An export of a few hundred messages
// fits in one; a larger one is several calls, each its own transaction.
const MaxImportBatch = 500

// BodyFormat names one representation of a message body.
type BodyFormat string

const (
	BodyFormatText BodyFormat = "text"
	BodyFormatHTML BodyFormat = "html"
)

// OutlookLocation is where an exported message lives in the Outlook profile it
// came from: enough to open it there again, or to reply from the right store.
type OutlookLocation struct {
	StoreID    string `json:"store_id"`
	FolderID   string `json:"folder_id"`
	FolderPath string `json:"folder_path"`
}

// ImportedMessage is one message as an export describes it.
//
// BodyText and BodyHTML are pointers so that "the export has no HTML body" and
// "the export has an empty HTML body" are different requests.
type ImportedMessage struct {
	// ProviderMessageID is the source's own id for the message, and the key that
	// makes an import idempotent. Required.
	ProviderMessageID string `json:"provider_message_id"`
	// SourceRevision is whatever the source uses to say a message changed
	// (Outlook's change key, a last-modified stamp). Stored and returned as
	// given; mailstack's own revision counts content changes it has seen.
	SourceRevision string `json:"source_revision,omitempty"`

	MessageID      string            `json:"message_id,omitempty"`
	ConversationID string            `json:"conversation_id,omitempty"`
	InReplyTo      string            `json:"in_reply_to,omitempty"`
	References     []string          `json:"references,omitempty"`
	Subject        string            `json:"subject"`
	From           backend.Address   `json:"from"`
	To             []backend.Address `json:"to,omitempty"`
	Cc             []backend.Address `json:"cc,omitempty"`
	Bcc            []backend.Address `json:"bcc,omitempty"`
	// ReceivedAt is required: a message dated "now" by the importer would sort
	// every backfilled export into the same moment.
	ReceivedAt time.Time        `json:"received_at"`
	BodyText   *string          `json:"body_text,omitempty"`
	BodyHTML   *string          `json:"body_html,omitempty"`
	Outlook    *OutlookLocation `json:"outlook,omitempty"`

	// Mailbox state. Applied when the message is first imported and never on a
	// re-import: once a message is here, whether it is read, flagged or filed
	// elsewhere belongs to this mailbox, not to the export.
	FolderID string `json:"folder_id,omitempty"`
	Unread   *bool  `json:"unread,omitempty"`
	Flagged  bool   `json:"flagged,omitempty"`

	// Snippet is derived by the caller, which has the HTML-to-text rule the
	// search index uses. Not part of the request.
	Snippet string `json:"-"`
}

// ImportSource names the unit an import came from, so the next run can skip it
// when its bytes have not changed. SHA256 is of the source's bytes as the
// importer read them.
type ImportSource struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

// ImportSourceRecord is a source as last imported.
type ImportSourceRecord struct {
	Ref          string    `json:"ref"`
	SHA256       string    `json:"sha256"`
	MessageCount int       `json:"message_count"`
	ImportedAt   time.Time `json:"imported_at"`
}

// ImportItemError is one message that failed validation, by its place in the
// batch.
type ImportItemError struct {
	Index             int    `json:"index"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	Error             string `json:"error"`
}

// ImportValidationError is returned when any message in a batch is invalid. The
// whole batch is refused: a source is imported entirely or not at all, so its
// checkpoint can never claim messages that were dropped.
type ImportValidationError struct {
	Items []ImportItemError
}

func (e *ImportValidationError) Error() string {
	return fmt.Sprintf("localmail: %d message(s) in the import are invalid; nothing was imported", len(e.Items))
}

// Import outcomes.
const (
	ImportCreated   = "created"
	ImportUpdated   = "updated"
	ImportUnchanged = "unchanged"
)

// ImportOutcome reports what one imported message did.
type ImportOutcome struct {
	ProviderMessageID string `json:"provider_message_id"`
	ID                string `json:"id"`
	Locator           string `json:"locator"`
	Outcome           string `json:"outcome"`
	Revision          int64  `json:"revision"`
	PreviousRevision  int64  `json:"previous_revision,omitempty"`
}

// BodySize describes one stored body representation.
type BodySize struct {
	Bytes int64 `json:"bytes"`
}

// BodyPresence says which representations a message has. A nil entry is
// absent; a zero-byte entry is present and empty.
type BodyPresence struct {
	Text *BodySize `json:"text"`
	HTML *BodySize `json:"html"`
}

// StoredMessage is a message's metadata as this store holds it, without its
// bodies.
type StoredMessage struct {
	backend.MessageMeta
	Bcc               []backend.Address `json:"bcc,omitempty"`
	Locator           string            `json:"locator"`
	Revision          int64             `json:"revision"`
	ContentSHA256     string            `json:"content_sha256,omitempty"`
	ProviderMessageID string            `json:"provider_message_id,omitempty"`
	SourceRevision    string            `json:"source_revision,omitempty"`
	Outlook           *OutlookLocation  `json:"outlook,omitempty"`
	Bodies            BodyPresence      `json:"bodies"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// MetadataETag changes whenever anything in the metadata response changes,
// including the mailbox state (read, flagged, folder) that a revision does not
// count.
func (m StoredMessage) MetadataETag() string {
	state := fmt.Sprintf("%s|%d|%s|%t|%t", m.ID, m.Revision, m.FolderID, m.Unread, m.Flagged)
	sum := sha256.Sum256([]byte(state))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// BodyETag changes only when the content does, which is what a revision counts.
func (m StoredMessage) BodyETag(format BodyFormat) string {
	return fmt.Sprintf(`"%s.r%d.%s"`, m.ID, m.Revision, format)
}

// ValidateImport checks a batch without touching the database.
func ValidateImport(msgs []ImportedMessage) error {
	var items []ImportItemError
	fail := func(i int, m ImportedMessage, format string, args ...any) {
		items = append(items, ImportItemError{Index: i, ProviderMessageID: m.ProviderMessageID, Error: fmt.Sprintf(format, args...)})
	}
	if len(msgs) == 0 {
		return &ImportValidationError{Items: []ImportItemError{{Index: -1, Error: "messages is empty"}}}
	}
	if len(msgs) > MaxImportBatch {
		return &ImportValidationError{Items: []ImportItemError{{Index: -1,
			Error: fmt.Sprintf("%d messages in one import; the most is %d", len(msgs), MaxImportBatch)}}}
	}
	seen := map[string]int{}
	for i, m := range msgs {
		if strings.TrimSpace(m.ProviderMessageID) == "" {
			fail(i, m, "provider_message_id is required")
		} else if first, dup := seen[m.ProviderMessageID]; dup {
			fail(i, m, "provider_message_id repeats message %d of this import", first)
		} else {
			seen[m.ProviderMessageID] = i
		}
		if m.ReceivedAt.IsZero() {
			fail(i, m, "received_at is required")
		}
		if m.BodyText == nil && m.BodyHTML == nil {
			fail(i, m, "body_text or body_html is required; send an empty string for a body that is present and empty")
		}
		if m.FolderID != "" && !knownFolder(m.FolderID) {
			fail(i, m, "unknown folder %q", m.FolderID)
		}
	}
	if len(items) > 0 {
		return &ImportValidationError{Items: items}
	}
	return nil
}

// importedID is the store's id for an imported message: derived from the
// account and the source's id, so a re-import lands on the same row, and long
// enough that two source ids colliding is not a case anyone has to handle.
func importedID(accountID, providerMessageID string) string {
	sum := sha256.Sum256([]byte(accountID + "\x00" + providerMessageID))
	return "lm" + hex.EncodeToString(sum[:16])
}

// contentHash covers everything an import sets except mailbox state, so a
// re-import of an unchanged message hashes the same whatever has happened to it
// here since.
func contentHash(m ImportedMessage) (string, error) {
	content := struct {
		ImportedMessage
		FolderID string `json:"folder_id,omitempty"`
		Unread   *bool  `json:"unread,omitempty"`
		Flagged  bool   `json:"flagged,omitempty"`
	}{ImportedMessage: m}
	content.ReceivedAt = m.ReceivedAt.UTC()
	raw, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("localmail: hash message %s: %w", m.ProviderMessageID, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Import stores a validated batch in one transaction and, when source is set,
// records it as imported in the same transaction. It returns one outcome per
// message in request order, and the metadata of every message that was created
// or changed, for the caller to index.
func (s *Store) Import(ctx context.Context, accountID string, msgs []ImportedMessage, source *ImportSource) ([]ImportOutcome, []backend.MessageMeta, error) {
	if accountID == "" {
		return nil, nil, fmt.Errorf("localmail: account id is required")
	}
	if err := ValidateImport(msgs); err != nil {
		return nil, nil, err
	}
	if source != nil && (source.Ref == "" || source.SHA256 == "") {
		return nil, nil, fmt.Errorf("localmail: an import source needs both ref and sha256")
	}

	now := time.Now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("localmail: begin import: %w", err)
	}
	defer tx.Rollback()

	outcomes := make([]ImportOutcome, len(msgs))
	var changed []backend.MessageMeta
	for i, m := range msgs {
		hash, err := contentHash(m)
		if err != nil {
			return nil, nil, err
		}
		id := importedID(accountID, m.ProviderMessageID)
		out := ImportOutcome{ProviderMessageID: m.ProviderMessageID, ID: id, Locator: accountID + ":" + id}

		var existingRevision int64
		var existingHash string
		err = tx.QueryRowContext(ctx, `
			SELECT revision, content_sha256 FROM local_messages
			WHERE account_id = ? AND provider_message_id = ? AND `+providerIDIndexed+``, accountID, m.ProviderMessageID).
			Scan(&existingRevision, &existingHash)
		switch {
		case err == sql.ErrNoRows:
			if err := insertImported(ctx, tx, accountID, id, m, hash, now); err != nil {
				return nil, nil, err
			}
			out.Outcome, out.Revision = ImportCreated, 1
		case err != nil:
			return nil, nil, fmt.Errorf("localmail: look up %s: %w", m.ProviderMessageID, err)
		case existingHash == hash:
			out.Outcome, out.Revision = ImportUnchanged, existingRevision
		default:
			if err := updateImported(ctx, tx, accountID, id, m, hash, existingRevision+1, now); err != nil {
				return nil, nil, err
			}
			out.Outcome, out.Revision, out.PreviousRevision = ImportUpdated, existingRevision+1, existingRevision
		}
		outcomes[i] = out
		if out.Outcome != ImportUnchanged {
			changed = append(changed, importedMeta(accountID, id, m))
		}
	}

	if source != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_import_sources (account_id, source_ref, source_sha256, message_count, imported_at)
			VALUES (?,?,?,?,?)
			ON CONFLICT (account_id, source_ref) DO UPDATE SET
				source_sha256 = excluded.source_sha256,
				message_count = excluded.message_count,
				imported_at   = excluded.imported_at`,
			accountID, source.Ref, source.SHA256, len(msgs), now); err != nil {
			return nil, nil, fmt.Errorf("localmail: record import source %s: %w", source.Ref, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("localmail: commit import: %w", err)
	}
	return outcomes, changed, nil
}

// importedMeta is the index's view of an imported message: what accounts'
// message index needs for Message-ID lookup and threading.
func importedMeta(accountID, id string, m ImportedMessage) backend.MessageMeta {
	folder := m.FolderID
	if folder == "" {
		folder = DefaultFolderID
	}
	return backend.MessageMeta{
		ID: id, AccountID: accountID, FolderID: folder,
		Subject: m.Subject, From: m.From, To: m.To, Cc: m.Cc, Date: m.ReceivedAt.UTC(),
		Snippet:   m.Snippet,
		MessageID: backend.StripAngleBrackets(m.MessageID), ThreadID: m.ConversationID,
		InReplyTo: backend.StripAngleBrackets(m.InReplyTo), References: stripAll(m.References),
	}
}

func stripAll(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if v := backend.StripAngleBrackets(id); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// importedColumns are the columns an import sets on create and on update, in
// the order importedValues gives them.
const importedColumns = `message_id, thread_id, in_reply_to, references_ids, subject,
	from_name, from_email, to_addrs, cc_addrs, bcc_addrs, date_ms, snippet,
	text_bytes, html_bytes, source_revision, outlook_store_id, outlook_folder_id,
	outlook_folder_path, content_sha256`

func importedValues(m ImportedMessage, hash string) ([]any, error) {
	var encodeErr error
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil && encodeErr == nil {
			encodeErr = fmt.Errorf("localmail: encode %s: %w", m.ProviderMessageID, err)
		}
		return string(raw)
	}
	var outlook OutlookLocation
	if m.Outlook != nil {
		outlook = *m.Outlook
	}
	values := []any{
		backend.StripAngleBrackets(m.MessageID), m.ConversationID, backend.StripAngleBrackets(m.InReplyTo),
		encode(stripAll(m.References)), m.Subject, m.From.Name, m.From.Email,
		encode(nonNilAddrs(m.To)), encode(nonNilAddrs(m.Cc)), encode(nonNilAddrs(m.Bcc)),
		m.ReceivedAt.UTC().UnixMilli(), m.Snippet,
		presentByteCount(m.BodyText), presentByteCount(m.BodyHTML),
		m.SourceRevision, outlook.StoreID, outlook.FolderID, outlook.FolderPath, hash,
	}
	return values, encodeErr
}

// presentByteCount is NULL for an absent body and a count, possibly zero, for a
// present one.
func presentByteCount(body *string) any {
	if body == nil {
		return nil
	}
	return len(*body)
}

func presentBody(body *string) any {
	if body == nil {
		return nil
	}
	return *body
}

func insertImported(ctx context.Context, tx *sql.Tx, accountID, id string, m ImportedMessage, hash string, now int64) error {
	values, err := importedValues(m, hash)
	if err != nil {
		return err
	}
	folder := m.FolderID
	if folder == "" {
		folder = DefaultFolderID
	}
	unread := true
	if m.Unread != nil {
		unread = *m.Unread
	}
	args := append([]any{accountID, id, m.ProviderMessageID, folder, boolToInt(unread), boolToInt(m.Flagged), now, now}, values...)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_messages (account_id, id, provider_message_id, folder_id, unread, flagged,
			created_at, updated_at, `+importedColumns+`)
		VALUES (`+placeholders+`)`, args...); err != nil {
		// The id is derived from the provider id, so the only way to reach this
		// is an appended message that happens to hold the same id, which a
		// sha256-derived id makes a non-event; report it rather than overwrite.
		return fmt.Errorf("localmail: insert imported %s: %w", m.ProviderMessageID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_message_bodies (account_id, id, body_text, body_html) VALUES (?,?,?,?)`,
		accountID, id, presentBody(m.BodyText), presentBody(m.BodyHTML)); err != nil {
		return fmt.Errorf("localmail: insert imported body %s: %w", m.ProviderMessageID, err)
	}
	return nil
}

func updateImported(ctx context.Context, tx *sql.Tx, accountID, id string, m ImportedMessage, hash string, revision, now int64) error {
	values, err := importedValues(m, hash)
	if err != nil {
		return err
	}
	columns := strings.Fields(strings.ReplaceAll(importedColumns, ",", " "))
	assignments := make([]string, len(columns))
	for i, c := range columns {
		assignments[i] = c + " = ?"
	}
	args := append(values, revision, now, accountID, id)
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_messages SET `+strings.Join(assignments, ", ")+`, revision = ?, updated_at = ?
		WHERE account_id = ? AND id = ?`, args...); err != nil {
		return fmt.Errorf("localmail: update imported %s: %w", m.ProviderMessageID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_message_bodies SET body_text = ?, body_html = ? WHERE account_id = ? AND id = ?`,
		presentBody(m.BodyText), presentBody(m.BodyHTML), accountID, id); err != nil {
		return fmt.Errorf("localmail: update imported body %s: %w", m.ProviderMessageID, err)
	}
	return nil
}

// providerIDIndexed repeats the WHERE of the partial unique index on
// provider_message_id. SQLite uses a partial index only when the query's own
// WHERE implies the index's, and a bound parameter proves nothing, so without
// this term every lookup by provider id walks the whole account and an import
// grows with the square of the mailbox.
const providerIDIndexed = `provider_message_id <> ''`

// ImportSources lists the sources imported into an account, by ref.
func (s *Store) ImportSources(ctx context.Context, accountID string) ([]ImportSourceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_ref, source_sha256, message_count, imported_at FROM local_import_sources
		WHERE account_id = ? ORDER BY source_ref`, accountID)
	if err != nil {
		return nil, fmt.Errorf("localmail: list import sources: %w", err)
	}
	defer rows.Close()
	out := []ImportSourceRecord{}
	for rows.Next() {
		var r ImportSourceRecord
		var importedAt int64
		if err := rows.Scan(&r.Ref, &r.SHA256, &r.MessageCount, &importedAt); err != nil {
			return nil, fmt.Errorf("localmail: scan import source: %w", err)
		}
		r.ImportedAt = time.UnixMilli(importedAt).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localmail: read import sources: %w", err)
	}
	return out, nil
}

// ============================ split reads ============================

const storedColumns = `id, folder_id, message_id, thread_id, in_reply_to, references_ids,
	subject, from_name, from_email, to_addrs, cc_addrs, bcc_addrs, date_ms, snippet,
	unread, flagged, text_bytes, html_bytes, provider_message_id, source_revision,
	outlook_store_id, outlook_folder_id, outlook_folder_path, revision, content_sha256,
	created_at, updated_at`

// scanStored reads storedColumns, then whatever extra columns the query
// selected after them into extra.
func scanStored(row *sql.Row, accountID string, extra ...any) (StoredMessage, error) {
	var m StoredMessage
	var refsJSON, toJSON, ccJSON, bccJSON string
	var dateMS, createdAt, updatedAt int64
	var unread, flagged int
	var textBytes, htmlBytes sql.NullInt64
	var outlook OutlookLocation
	dest := append([]any{&m.ID, &m.FolderID, &m.MessageID, &m.ThreadID, &m.InReplyTo, &refsJSON,
		&m.Subject, &m.From.Name, &m.From.Email, &toJSON, &ccJSON, &bccJSON, &dateMS, &m.Snippet,
		&unread, &flagged, &textBytes, &htmlBytes, &m.ProviderMessageID, &m.SourceRevision,
		&outlook.StoreID, &outlook.FolderID, &outlook.FolderPath, &m.Revision, &m.ContentSHA256,
		&createdAt, &updatedAt}, extra...)
	err := row.Scan(dest...)
	if err == sql.ErrNoRows {
		return StoredMessage{}, ErrNotFound
	}
	if err != nil {
		return StoredMessage{}, fmt.Errorf("localmail: read message: %w", err)
	}
	m.AccountID = accountID
	m.Locator = accountID + ":" + m.ID
	m.Date = time.UnixMilli(dateMS).UTC()
	m.CreatedAt = time.UnixMilli(createdAt).UTC()
	m.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	m.Unread = unread == 1
	m.Flagged = flagged == 1
	if textBytes.Valid {
		m.Bodies.Text = &BodySize{Bytes: textBytes.Int64}
	}
	if htmlBytes.Valid {
		m.Bodies.HTML = &BodySize{Bytes: htmlBytes.Int64}
	}
	if outlook != (OutlookLocation{}) {
		m.Outlook = &outlook
	}
	for _, field := range []struct {
		raw string
		out any
	}{{refsJSON, &m.References}, {toJSON, &m.To}, {ccJSON, &m.Cc}, {bccJSON, &m.Bcc}} {
		if err := decodeJSON(field.raw, field.out); err != nil {
			return StoredMessage{}, err
		}
	}
	return m, nil
}

// Metadata reads one message without its bodies.
func (s *Store) Metadata(ctx context.Context, accountID, id string) (StoredMessage, error) {
	return scanStored(s.db.QueryRowContext(ctx,
		`SELECT `+storedColumns+` FROM local_messages WHERE account_id = ? AND id = ?`, accountID, id), accountID)
}

// MetadataByProviderID reads one message by the id its source gave it.
func (s *Store) MetadataByProviderID(ctx context.Context, accountID, providerMessageID string) (StoredMessage, error) {
	if providerMessageID == "" {
		return StoredMessage{}, ErrNotFound
	}
	return scanStored(s.db.QueryRowContext(ctx,
		`SELECT `+storedColumns+` FROM local_messages WHERE account_id = ? AND provider_message_id = ? AND `+providerIDIndexed,
		accountID, providerMessageID), accountID)
}

// Body reads one representation of a message's body, whole, with the metadata
// a caller needs to label and cache it. One query, so the body and the
// revision that labels it cannot come from either side of a concurrent import;
// it never reads the other representation.
func (s *Store) Body(ctx context.Context, accountID, id string, format BodyFormat) (string, StoredMessage, error) {
	var column string
	switch format {
	case BodyFormatText:
		column = "body_text"
	case BodyFormatHTML:
		column = "body_html"
	default:
		return "", StoredMessage{}, fmt.Errorf("localmail: unknown body format %q; want text or html", format)
	}
	var body sql.NullString
	meta, err := scanStored(s.db.QueryRowContext(ctx, `
		SELECT `+storedColumns+`, `+column+`
		FROM local_messages JOIN local_message_bodies USING (account_id, id)
		WHERE account_id = ? AND id = ?`, accountID, id), accountID, &body)
	if err != nil {
		return "", StoredMessage{}, err
	}
	if !body.Valid {
		return "", meta, ErrBodyAbsent
	}
	return body.String, meta, nil
}
