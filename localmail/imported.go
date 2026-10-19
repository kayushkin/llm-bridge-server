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
	// Attachments in the order the message lists them. Their bytes go to
	// file-store; see ImportedAttachment.
	Attachments []ImportedAttachment `json:"attachments,omitempty"`

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

// MaxAttachmentsPerMessage bounds one message's attachment list.
const MaxAttachmentsPerMessage = 100

// ImportedAttachment is one attachment as an export carries it, bytes and all.
// Content is base64 on the wire, which encoding/json decodes into the slice.
type ImportedAttachment struct {
	// SourceAttachmentID is the export's own id for the part, kept as given.
	SourceAttachmentID string `json:"source_attachment_id,omitempty"`
	Filename           string `json:"filename"`
	ContentType        string `json:"content_type"`
	// ContentID is the part's Content-ID, by which an HTML body shows it
	// inline as cid:<ContentID>.
	ContentID string `json:"content_id,omitempty"`
	Inline    bool   `json:"inline,omitempty"`
	Content   []byte `json:"content_base64"`

	// FileID is file-store's id once the server has uploaded the bytes. Import
	// refuses a changed message whose attachments have none.
	FileID string `json:"-"`
}

// StoredAttachment describes one attachment of a stored revision. It carries
// no file-store id: the bytes are read through this service, which checks the
// caller first.
type StoredAttachment struct {
	ID                 string `json:"id"`
	Filename           string `json:"filename"`
	MimeType           string `json:"mime_type"`
	Size               int64  `json:"size"`
	SHA256             string `json:"sha256"`
	InlineCID          string `json:"inline_cid,omitempty"`
	Inline             bool   `json:"inline"`
	SourceAttachmentID string `json:"source_attachment_id,omitempty"`
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
	// Attachments of this revision, in the order the message lists them.
	// Always present, empty when there are none.
	Attachments []StoredAttachment `json:"attachments"`
	CreatedAt   time.Time          `json:"created_at"`
	// UpdatedAt is when this revision became current.
	UpdatedAt time.Time `json:"updated_at"`
	// ReplacedAt is when a later import replaced this revision. Absent on the
	// current one.
	ReplacedAt *time.Time `json:"replaced_at,omitempty"`
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
		if len(m.Attachments) > MaxAttachmentsPerMessage {
			fail(i, m, "%d attachments; the most is %d", len(m.Attachments), MaxAttachmentsPerMessage)
		}
		for j, a := range m.Attachments {
			switch {
			case strings.TrimSpace(a.Filename) == "":
				fail(i, m, "attachment %d has no filename", j)
			case strings.TrimSpace(a.ContentType) == "":
				fail(i, m, "attachment %d (%s) has no content_type", j, a.Filename)
			case len(a.Content) == 0:
				// file-store keeps no empty file, so an empty part cannot be
				// stored; refusing here says so before anything is uploaded.
				fail(i, m, "attachment %d (%s) has no bytes; an empty attachment cannot be stored", j, a.Filename)
			}
		}
	}
	if len(items) > 0 {
		return &ImportValidationError{Items: items}
	}
	return nil
}

// ImportedID is the store's id for an imported message: derived from the
// account and the source's id, so a re-import lands on the same row, and long
// enough that two source ids colliding is not a case anyone has to handle.
func ImportedID(accountID, providerMessageID string) string {
	sum := sha256.Sum256([]byte(accountID + "\x00" + providerMessageID))
	return "lm" + hex.EncodeToString(sum[:16])
}

// contentHash covers everything an import sets except mailbox state, so a
// re-import of an unchanged message hashes the same whatever has happened to it
// here since.
//
// An attachment counts by its descriptor and the sha256 of its bytes, so a
// changed file changes the hash without the bytes being hashed into JSON.
func contentHash(m ImportedMessage) (string, error) {
	type hashedAttachment struct {
		SourceAttachmentID string `json:"source_attachment_id,omitempty"`
		Filename           string `json:"filename"`
		ContentType        string `json:"content_type"`
		ContentID          string `json:"content_id,omitempty"`
		Inline             bool   `json:"inline,omitempty"`
		SHA256             string `json:"sha256"`
	}
	content := struct {
		ImportedMessage
		FolderID    string             `json:"folder_id,omitempty"`
		Unread      *bool              `json:"unread,omitempty"`
		Flagged     bool               `json:"flagged,omitempty"`
		Attachments []hashedAttachment `json:"attachments,omitempty"`
	}{ImportedMessage: m}
	content.ReceivedAt = m.ReceivedAt.UTC()
	for _, a := range m.Attachments {
		content.Attachments = append(content.Attachments, hashedAttachment{
			SourceAttachmentID: a.SourceAttachmentID, Filename: a.Filename, ContentType: a.ContentType,
			ContentID: a.ContentID, Inline: a.Inline, SHA256: sha256Hex(a.Content),
		})
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("localmail: hash message %s: %w", m.ProviderMessageID, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PlanImport says what Import would do with each message right now, without
// writing anything: the server uploads attachments only for the messages that
// will change. An import that runs between the plan and the import can change
// the answer, which Import handles by reporting the uploads it did not use.
func (s *Store) PlanImport(ctx context.Context, accountID string, msgs []ImportedMessage) ([]string, error) {
	plan := make([]string, len(msgs))
	for i, m := range msgs {
		hash, err := contentHash(m)
		if err != nil {
			return nil, err
		}
		var existingHash string
		err = s.db.QueryRowContext(ctx, `
			SELECT content_sha256 FROM local_messages
			WHERE account_id = ? AND provider_message_id = ? AND `+providerIDIndexed,
			accountID, m.ProviderMessageID).Scan(&existingHash)
		switch {
		case err == sql.ErrNoRows:
			plan[i] = ImportCreated
		case err != nil:
			return nil, fmt.Errorf("localmail: look up %s: %w", m.ProviderMessageID, err)
		case existingHash == hash:
			plan[i] = ImportUnchanged
		default:
			plan[i] = ImportUpdated
		}
	}
	return plan, nil
}

// Import stores a validated batch in one transaction and, when source is set,
// records it as imported in the same transaction. It returns one outcome per
// message in request order; the metadata of every message that was created or
// changed, for the caller to index; and the file-store ids of uploaded
// attachments that ended up unused, because their message turned out
// unchanged, for the caller to purge.
//
// An update copies the revision it replaces into local_message_revisions
// first, so nothing an import overwrites is lost.
func (s *Store) Import(ctx context.Context, accountID string, msgs []ImportedMessage, source *ImportSource) ([]ImportOutcome, []backend.MessageMeta, []string, error) {
	if accountID == "" {
		return nil, nil, nil, fmt.Errorf("localmail: account id is required")
	}
	if err := ValidateImport(msgs); err != nil {
		return nil, nil, nil, err
	}
	if source != nil && (source.Ref == "" || source.SHA256 == "") {
		return nil, nil, nil, fmt.Errorf("localmail: an import source needs both ref and sha256")
	}

	now := time.Now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("localmail: begin import: %w", err)
	}
	defer tx.Rollback()

	outcomes := make([]ImportOutcome, len(msgs))
	var changed []backend.MessageMeta
	var unused []string
	for i, m := range msgs {
		hash, err := contentHash(m)
		if err != nil {
			return nil, nil, nil, err
		}
		id := ImportedID(accountID, m.ProviderMessageID)
		out := ImportOutcome{ProviderMessageID: m.ProviderMessageID, ID: id, Locator: accountID + ":" + id}

		var existingRevision int64
		var existingHash string
		err = tx.QueryRowContext(ctx, `
			SELECT revision, content_sha256 FROM local_messages
			WHERE account_id = ? AND provider_message_id = ? AND `+providerIDIndexed, accountID, m.ProviderMessageID).
			Scan(&existingRevision, &existingHash)
		switch {
		case err == sql.ErrNoRows:
			if err := insertImported(ctx, tx, accountID, id, m, hash, now); err != nil {
				return nil, nil, nil, err
			}
			out.Outcome, out.Revision = ImportCreated, 1
		case err != nil:
			return nil, nil, nil, fmt.Errorf("localmail: look up %s: %w", m.ProviderMessageID, err)
		case existingHash == hash:
			out.Outcome, out.Revision = ImportUnchanged, existingRevision
		default:
			if err := keepRevision(ctx, tx, accountID, id, now); err != nil {
				return nil, nil, nil, err
			}
			if err := updateImported(ctx, tx, accountID, id, m, hash, existingRevision+1, now); err != nil {
				return nil, nil, nil, err
			}
			out.Outcome, out.Revision, out.PreviousRevision = ImportUpdated, existingRevision+1, existingRevision
		}
		if out.Outcome == ImportUnchanged {
			for _, a := range m.Attachments {
				if a.FileID != "" {
					unused = append(unused, a.FileID)
				}
			}
		} else {
			if err := insertAttachments(ctx, tx, accountID, id, out.Revision, m); err != nil {
				return nil, nil, nil, err
			}
			changed = append(changed, importedMeta(accountID, id, m))
		}
		outcomes[i] = out
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
			return nil, nil, nil, fmt.Errorf("localmail: record import source %s: %w", source.Ref, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("localmail: commit import: %w", err)
	}
	return outcomes, changed, unused, nil
}

// keepRevision copies the current revision of a message into
// local_message_revisions before an update overwrites it.
func keepRevision(ctx context.Context, tx *sql.Tx, accountID, id string, now int64) error {
	var bodyText, bodyHTML sql.NullString
	current, err := scanStored(tx.QueryRowContext(ctx, `
		SELECT `+storedColumns+`, body_text, body_html
		FROM local_messages JOIN local_message_bodies USING (account_id, id)
		WHERE account_id = ? AND id = ?`, accountID, id), accountID, &bodyText, &bodyHTML)
	if err != nil {
		return fmt.Errorf("localmail: read revision to keep of %s: %w", id, err)
	}
	// The attachments stay in their own table under this revision's number,
	// so the snapshot leaves them out rather than holding a second copy.
	current.Attachments = nil
	snapshot, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("localmail: encode revision %d of %s: %w", current.Revision, id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_message_revisions (account_id, id, revision, metadata_json, body_text, body_html, replaced_at)
		VALUES (?,?,?,?,?,?,?)`,
		accountID, id, current.Revision, string(snapshot), bodyText, bodyHTML, now); err != nil {
		return fmt.Errorf("localmail: keep revision %d of %s: %w", current.Revision, id, err)
	}
	return nil
}

// insertAttachments records a revision's attachments, whose bytes the caller
// has already put in file-store.
func insertAttachments(ctx context.Context, tx *sql.Tx, accountID, id string, revision int64, m ImportedMessage) error {
	for position, a := range m.Attachments {
		if a.FileID == "" {
			return fmt.Errorf("%w: attachment %d (%s) of %s", ErrAttachmentsNotUploaded, position, a.Filename, m.ProviderMessageID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_message_attachments (account_id, id, revision, attachment_id, position,
				source_attachment_id, filename, content_type, size_bytes, sha256, content_id, is_inline, file_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			accountID, id, revision, attachmentIDAt(position), position, a.SourceAttachmentID, a.Filename,
			a.ContentType, len(a.Content), sha256Hex(a.Content), backend.StripAngleBrackets(a.ContentID),
			boolToInt(a.Inline), a.FileID); err != nil {
			return fmt.Errorf("localmail: record attachment %d of %s: %w", position, m.ProviderMessageID, err)
		}
	}
	return nil
}

// attachmentIDAt is an attachment's id within its revision: its place in the
// message's list, counted from one.
func attachmentIDAt(position int) string { return fmt.Sprintf("a%d", position+1) }

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
	return s.withAttachments(ctx)(scanStored(s.db.QueryRowContext(ctx,
		`SELECT `+storedColumns+` FROM local_messages WHERE account_id = ? AND id = ?`, accountID, id), accountID))
}

// MetadataByProviderID reads one message by the id its source gave it.
func (s *Store) MetadataByProviderID(ctx context.Context, accountID, providerMessageID string) (StoredMessage, error) {
	if providerMessageID == "" {
		return StoredMessage{}, ErrNotFound
	}
	return s.withAttachments(ctx)(scanStored(s.db.QueryRowContext(ctx,
		`SELECT `+storedColumns+` FROM local_messages WHERE account_id = ? AND provider_message_id = ? AND `+providerIDIndexed,
		accountID, providerMessageID), accountID))
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

// ============================ attachments ============================

// ErrAttachmentsNotUploaded is returned by Import for a changed message whose
// attachments carry no file-store id. The server uploads only for messages
// PlanImport said would change, so this means one changed in between.
var ErrAttachmentsNotUploaded = errors.New("localmail: a changed message's attachments were not uploaded")

// ErrAttachmentNotFound is returned for an attachment id the revision does not
// have.
var ErrAttachmentNotFound = errors.New("localmail: attachment not found")

// ErrRevisionNotFound is returned for a revision the message never had.
var ErrRevisionNotFound = errors.New("localmail: revision not found")

func (s *Store) attachmentsOf(ctx context.Context, accountID, id string, revision int64) ([]StoredAttachment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT attachment_id, filename, content_type, size_bytes, sha256, content_id, is_inline, source_attachment_id
		FROM local_message_attachments WHERE account_id = ? AND id = ? AND revision = ?
		ORDER BY position`, accountID, id, revision)
	if err != nil {
		return nil, fmt.Errorf("localmail: list attachments of %s: %w", id, err)
	}
	defer rows.Close()
	out := []StoredAttachment{}
	for rows.Next() {
		var a StoredAttachment
		var inline int
		if err := rows.Scan(&a.ID, &a.Filename, &a.MimeType, &a.Size, &a.SHA256, &a.InlineCID, &inline, &a.SourceAttachmentID); err != nil {
			return nil, fmt.Errorf("localmail: scan attachment of %s: %w", id, err)
		}
		a.Inline = inline == 1
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localmail: read attachments of %s: %w", id, err)
	}
	return out, nil
}

// withAttachments fills in the attachments of the revision a read returned.
// It takes the read's two results whole, so a call reads
// s.withAttachments(ctx)(scanStored(...)).
func (s *Store) withAttachments(ctx context.Context) func(StoredMessage, error) (StoredMessage, error) {
	return func(m StoredMessage, err error) (StoredMessage, error) {
		if err != nil {
			return StoredMessage{}, err
		}
		m.Attachments, err = s.attachmentsOf(ctx, m.AccountID, m.ID, m.Revision)
		if err != nil {
			return StoredMessage{}, err
		}
		m.HasAttach = len(m.Attachments) > 0
		return m, nil
	}
}

// AttachmentFile finds one attachment of a revision — revision 0 means the
// current one — and the file-store id that holds its bytes. The id is for this
// service to fetch with, never to hand out.
func (s *Store) AttachmentFile(ctx context.Context, accountID, id string, revision int64, attachmentID string) (StoredAttachment, string, error) {
	if revision == 0 {
		current, err := s.Metadata(ctx, accountID, id)
		if err != nil {
			return StoredAttachment{}, "", err
		}
		revision = current.Revision
	} else if _, err := s.Revision(ctx, accountID, id, revision); err != nil {
		return StoredAttachment{}, "", err
	}
	var a StoredAttachment
	var inline int
	var fileID string
	err := s.db.QueryRowContext(ctx, `
		SELECT attachment_id, filename, content_type, size_bytes, sha256, content_id, is_inline, source_attachment_id, file_id
		FROM local_message_attachments WHERE account_id = ? AND id = ? AND revision = ? AND attachment_id = ?`,
		accountID, id, revision, attachmentID).
		Scan(&a.ID, &a.Filename, &a.MimeType, &a.Size, &a.SHA256, &a.InlineCID, &inline, &a.SourceAttachmentID, &fileID)
	if err == sql.ErrNoRows {
		return StoredAttachment{}, "", ErrAttachmentNotFound
	}
	if err != nil {
		return StoredAttachment{}, "", fmt.Errorf("localmail: read attachment %s of %s: %w", attachmentID, id, err)
	}
	a.Inline = inline == 1
	return a, fileID, nil
}

// AttachmentFileIDs lists the file-store ids of every attachment of every
// revision of a message: what has to be purged before the message is.
func (s *Store) AttachmentFileIDs(ctx context.Context, accountID, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT file_id FROM local_message_attachments WHERE account_id = ? AND id = ?`, accountID, id)
	if err != nil {
		return nil, fmt.Errorf("localmail: list attachment files of %s: %w", id, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fileID string
		if err := rows.Scan(&fileID); err != nil {
			return nil, fmt.Errorf("localmail: scan attachment file of %s: %w", id, err)
		}
		out = append(out, fileID)
	}
	return out, rows.Err()
}

// ============================ revisions ============================

// RevisionSummary is one entry in a message's history.
type RevisionSummary struct {
	Revision       int64        `json:"revision"`
	Current        bool         `json:"current"`
	ContentSHA256  string       `json:"content_sha256,omitempty"`
	SourceRevision string       `json:"source_revision,omitempty"`
	Bodies         BodyPresence `json:"bodies"`
	BecameCurrent  time.Time    `json:"became_current_at"`
	ReplacedAt     *time.Time   `json:"replaced_at,omitempty"`
}

// Revisions lists a message's history, newest first: the current revision,
// then each one an import replaced.
func (s *Store) Revisions(ctx context.Context, accountID, id string) ([]RevisionSummary, error) {
	current, err := s.Metadata(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	out := []RevisionSummary{summaryOf(current, true)}
	rows, err := s.db.QueryContext(ctx, `
		SELECT metadata_json, replaced_at FROM local_message_revisions
		WHERE account_id = ? AND id = ? ORDER BY revision DESC`, accountID, id)
	if err != nil {
		return nil, fmt.Errorf("localmail: list revisions of %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		old, err := scanRevision(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, summaryOf(old, false))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localmail: read revisions of %s: %w", id, err)
	}
	return out, nil
}

func summaryOf(m StoredMessage, current bool) RevisionSummary {
	return RevisionSummary{
		Revision: m.Revision, Current: current, ContentSHA256: m.ContentSHA256, SourceRevision: m.SourceRevision,
		Bodies: m.Bodies, BecameCurrent: m.UpdatedAt, ReplacedAt: m.ReplacedAt,
	}
}

func scanRevision(scan func(...any) error, extra ...any) (StoredMessage, error) {
	var metadataJSON string
	var replacedAt int64
	if err := scan(append([]any{&metadataJSON, &replacedAt}, extra...)...); err != nil {
		if err == sql.ErrNoRows {
			return StoredMessage{}, ErrRevisionNotFound
		}
		return StoredMessage{}, fmt.Errorf("localmail: read revision: %w", err)
	}
	var m StoredMessage
	if err := json.Unmarshal([]byte(metadataJSON), &m); err != nil {
		return StoredMessage{}, fmt.Errorf("localmail: decode stored revision: %w", err)
	}
	replaced := time.UnixMilli(replacedAt).UTC()
	m.ReplacedAt = &replaced
	return m, nil
}

// Revision reads one revision's metadata, the current one included.
func (s *Store) Revision(ctx context.Context, accountID, id string, revision int64) (StoredMessage, error) {
	current, err := s.Metadata(ctx, accountID, id)
	if err != nil {
		return StoredMessage{}, err
	}
	if revision == current.Revision {
		return current, nil
	}
	old, err := scanRevision(s.db.QueryRowContext(ctx, `
		SELECT metadata_json, replaced_at FROM local_message_revisions
		WHERE account_id = ? AND id = ? AND revision = ?`, accountID, id, revision).Scan)
	return s.withAttachments(ctx)(old, err)
}

// RevisionBody reads one body representation of one revision.
func (s *Store) RevisionBody(ctx context.Context, accountID, id string, revision int64, format BodyFormat) (string, StoredMessage, error) {
	current, err := s.Metadata(ctx, accountID, id)
	if err != nil {
		return "", StoredMessage{}, err
	}
	if revision == current.Revision {
		return s.Body(ctx, accountID, id, format)
	}
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
	old, err := scanRevision(s.db.QueryRowContext(ctx, `
		SELECT metadata_json, replaced_at, `+column+` FROM local_message_revisions
		WHERE account_id = ? AND id = ? AND revision = ?`, accountID, id, revision).Scan, &body)
	if err != nil {
		return "", StoredMessage{}, err
	}
	if !body.Valid {
		return "", old, ErrBodyAbsent
	}
	return body.String, old, nil
}
