package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

const importColumns = `seq, owner_principal_id, importer, description, started_at, finished_at, counts`

func scanImport(row interface{ Scan(...any) error }) (Import, error) {
	var run Import
	var seq int64
	var counts string
	if err := row.Scan(&seq, &run.OwnerPrincipalID, &run.Importer, &run.Description, &run.StartedAt, &run.FinishedAt, &counts); err != nil {
		return Import{}, err
	}
	run.ID = postgresstorage.FormatRecordID(ImportIDPrefix, seq)
	if err := json.Unmarshal([]byte(counts), &run.Counts); err != nil {
		return Import{}, fmt.Errorf("import %s counts: %w", run.ID, err)
	}
	return run, nil
}

// StartImport records a new import run owned by ownerPrincipalID. Every row
// its batches write is that owner's.
func (s *Store) StartImport(ctx context.Context, ownerPrincipalID string, start ImportStart) (Import, error) {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return Import{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if strings.TrimSpace(start.Importer) == "" {
		return Import{}, fmt.Errorf("%w: importer is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	return scanImport(s.db.QueryRowContext(ctx, `INSERT INTO imports (owner_principal_id, importer, description, started_at)
		VALUES ($1, $2, $3, $4) RETURNING `+importColumns, ownerPrincipalID, start.Importer, start.Description, nowMillis()))
}

// importInScope reads one import run in scope; a malformed id, a missing run
// and one outside the scope are all not found.
func importInScope(ctx context.Context, db queryer, scope ownership.Scope, id string, forUpdate bool) (Import, int64, error) {
	seq, err := recordNumber(ImportIDPrefix, "import", id)
	if err != nil {
		return Import{}, 0, err
	}
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return Import{}, 0, err
	}
	query := `SELECT ` + importColumns + ` FROM imports WHERE seq = ` + args.add(seq) + ` AND ` + condition
	if forUpdate {
		query += ` FOR UPDATE`
	}
	run, err := scanImport(db.QueryRowContext(ctx, query, args.values...))
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, 0, fmt.Errorf("%w: import %s", ErrNotFound, id)
	}
	return run, seq, err
}

// GetImport reads one import run in scope.
func (s *Store) GetImport(ctx context.Context, scope ownership.Scope, id string) (Import, error) {
	run, _, err := importInScope(ctx, s.db, scope, id, false)
	return run, err
}

// ListImports lists the import runs in scope, newest first.
func (s *Store) ListImports(ctx context.Context, scope ownership.Scope) ([]Import, error) {
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+importColumns+` FROM imports WHERE `+condition+` ORDER BY seq DESC`, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []Import{}
	for rows.Next() {
		run, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// FinishImport marks an import run in scope done.
func (s *Store) FinishImport(ctx context.Context, scope ownership.Scope, id string) (Import, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	_, seq, err := importInScope(ctx, s.db, scope, id, false)
	if err != nil {
		return Import{}, err
	}
	return scanImport(s.db.QueryRowContext(ctx, `UPDATE imports SET finished_at = $1 WHERE seq = $2 RETURNING `+importColumns, nowMillis(), seq))
}

type conversationKey struct{ source, sourceRef string }

// batchWriter holds one batch's transaction. Every row it reads or writes is
// the import owner's.
type batchWriter struct {
	ctx           context.Context
	tx            *sql.Tx
	owner         string
	importSeq     int64
	importID      string
	now           int64
	counts        ImportCounts
	conversations map[conversationKey]int64
}

func (w *batchWriter) queryRow(query string, args ...any) *sql.Row {
	return w.tx.QueryRowContext(w.ctx, query, args...)
}

func (w *batchWriter) exec(query string, args ...any) (sql.Result, error) {
	return w.tx.ExecContext(w.ctx, query, args...)
}

// ApplyBatch writes one batch into an import run in scope, inside one
// transaction: one bad record writes nothing. The rows it writes are the
// import owner's, and every key it names is looked up in that owner's
// records only.
func (s *Store) ApplyBatch(ctx context.Context, scope ownership.Scope, importID string, batch ImportBatch) (ImportCounts, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	var batchCounts ImportCounts
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		run, importSeq, err := importInScope(ctx, tx, scope, importID, true)
		if err != nil {
			return err
		}
		if run.FinishedAt != 0 {
			return fmt.Errorf("%w: import %s is finished", ErrInvalid, importID)
		}
		w := &batchWriter{ctx: ctx, tx: tx, owner: run.OwnerPrincipalID, importSeq: importSeq, importID: run.ID, now: nowMillis(),
			conversations: map[conversationKey]int64{}}
		for i, conversation := range batch.Conversations {
			if err := w.upsertConversation(conversation); err != nil {
				return fmt.Errorf("conversation %d (%s %s): %w", i, conversation.Source, conversation.SourceRef, err)
			}
		}
		for i, message := range batch.Messages {
			if err := w.upsertMessage(message); err != nil {
				return fmt.Errorf("message %d (%s %s): %w", i, message.Source, message.SourceRef, err)
			}
		}
		for i, supersession := range batch.Supersessions {
			if err := w.applySupersession(supersession); err != nil {
				return fmt.Errorf("supersession %d (%s %s by %s %s): %w", i, supersession.Superseded.Source, supersession.Superseded.SourceRef,
					supersession.By.Source, supersession.By.SourceRef, err)
			}
		}
		for i, merge := range batch.ConversationMerges {
			if err := w.applyMerge(merge); err != nil {
				return fmt.Errorf("conversation merge %d (%s %s into %s %s): %w", i, merge.Merged.Source, merge.Merged.SourceRef,
					merge.Into.Source, merge.Into.SourceRef, err)
			}
		}
		for i, day := range batch.ConversationDays {
			if err := w.upsertDay(day); err != nil {
				return fmt.Errorf("conversation day %d (%s %s %s): %w", i, day.ConversationSource, day.ConversationSourceRef, day.Day, err)
			}
		}
		total := run.Counts
		total.add(w.counts)
		encoded, err := json.Marshal(total)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE imports SET counts = $1 WHERE seq = $2`, string(encoded), importSeq); err != nil {
			return err
		}
		batchCounts = w.counts
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return batchCounts, nil
}

func (c *ImportCounts) add(other ImportCounts) {
	c.ConversationsCreated += other.ConversationsCreated
	c.ConversationsUpdated += other.ConversationsUpdated
	c.MessagesCreated += other.MessagesCreated
	c.MessagesUpdated += other.MessagesUpdated
	c.MessagesUnchanged += other.MessagesUnchanged
	c.DaysCreated += other.DaysCreated
	c.DaysUpdated += other.DaysUpdated
	c.MessagesSuperseded += other.MessagesSuperseded
	c.MessagesSupersededSkipped += other.MessagesSupersededSkipped
	c.ConversationsMerged += other.ConversationsMerged
	c.ConversationMergesSkipped += other.ConversationMergesSkipped
}

// applySupersession retires a row in favor of a stored message.
func (w *batchWriter) applySupersession(supersession ImportSupersession) error {
	if supersession.By.Source == "" || supersession.By.SourceRef == "" {
		return fmt.Errorf("%w: by needs source and source_ref", ErrInvalid)
	}
	var bySeq int64
	err := w.queryRow(`SELECT seq FROM messages WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		w.owner, supersession.By.Source, supersession.By.SourceRef).Scan(&bySeq)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no message %s %s to supersede with", ErrInvalid, supersession.By.Source, supersession.By.SourceRef)
	}
	if err != nil {
		return err
	}
	return w.supersede(supersession.Superseded, bySeq)
}

func (w *batchWriter) conversationSeq(source, sourceRef string) (int64, error) {
	key := conversationKey{source, sourceRef}
	if seq, known := w.conversations[key]; known {
		return seq, nil
	}
	seq, _, err := liveConversationSeq(w.ctx, w.tx, w.owner, ConversationKey{source, sourceRef})
	if err != nil {
		return 0, err
	}
	if seq == 0 {
		return 0, fmt.Errorf("%w: no conversation %s %s; send it first", ErrInvalid, source, sourceRef)
	}
	w.conversations[key] = seq
	return seq, nil
}

func (w *batchWriter) upsertConversation(conversation ImportConversation) error {
	if conversation.Source == "" || conversation.SourceRef == "" {
		return fmt.Errorf("%w: source and source_ref are required", ErrInvalid)
	}
	if !contains(Platforms, conversation.Platform) {
		return fmt.Errorf("%w: platform %q is not one of %v", ErrInvalid, conversation.Platform, Platforms)
	}
	var seq int64
	var platform, title, parentSourceRef string
	var isGroup bool
	err := w.queryRow(`SELECT seq, platform, title, is_group, parent_source_ref FROM conversations
		WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		w.owner, conversation.Source, conversation.SourceRef).Scan(&seq, &platform, &title, &isGroup, &parentSourceRef)
	merged := false
	if errors.Is(err, sql.ErrNoRows) {
		// A merged conversation's key stands for the one it merged into, which
		// keeps its own title and kind and gains the participants.
		lookup := w.queryRow(`SELECT merged_into_seq FROM conversation_merges WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
			w.owner, conversation.Source, conversation.SourceRef).Scan(&seq)
		switch {
		case lookup == nil:
			merged, err = true, nil
		case !errors.Is(lookup, sql.ErrNoRows):
			return lookup
		}
	}
	switch {
	case merged:
	case errors.Is(err, sql.ErrNoRows):
		if err := w.queryRow(`INSERT INTO conversations (owner_principal_id, platform, source, source_ref, title, is_group, parent_source_ref, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING seq`,
			w.owner, conversation.Platform, conversation.Source, conversation.SourceRef, conversation.Title, conversation.IsGroup, conversation.ParentSourceRef, w.now).Scan(&seq); err != nil {
			return err
		}
		w.counts.ConversationsCreated++
	case err != nil:
		return err
	default:
		// An empty title in a batch means the importer does not know one, not
		// that the chat lost its name.
		newTitle := title
		if conversation.Title != "" {
			newTitle = conversation.Title
		}
		newParent := parentSourceRef
		if conversation.ParentSourceRef != "" {
			newParent = conversation.ParentSourceRef
		}
		if platform != conversation.Platform || newTitle != title || isGroup != conversation.IsGroup || newParent != parentSourceRef {
			if _, err := w.exec(`UPDATE conversations SET platform = $1, title = $2, is_group = $3, parent_source_ref = $4, updated_at = $5 WHERE seq = $6`,
				conversation.Platform, newTitle, conversation.IsGroup, newParent, w.now, seq); err != nil {
				return err
			}
			w.counts.ConversationsUpdated++
		}
	}
	w.conversations[conversationKey{conversation.Source, conversation.SourceRef}] = seq
	for _, participant := range conversation.Participants {
		if participant.Handle == "" {
			return fmt.Errorf("%w: a participant has no handle", ErrInvalid)
		}
		if _, err := w.exec(`INSERT INTO participants (owner_principal_id, conversation_seq, handle, display_name, person_id, is_self) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (conversation_seq, handle) DO UPDATE SET
				display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE participants.display_name END,
				person_id    = CASE WHEN excluded.person_id    <> '' THEN excluded.person_id    ELSE participants.person_id END,
				is_self      = excluded.is_self OR participants.is_self`,
			w.owner, seq, participant.Handle, participant.DisplayName, participant.PersonID, participant.IsSelf); err != nil {
			return err
		}
	}
	return nil
}

// storedMessageRow is every column of a messages row, as superseded and
// revised rows are kept.
type storedMessageRow struct {
	Seq               int64
	ConversationSeq   int64
	SentAt            int64
	SenderHandle      string
	Kind              string
	Body              string
	BodySource        string
	Attachments       string
	Embeds            string
	Buttons           string
	ReplyToSourceRef  string
	CallSeconds       int
	Edited            bool
	DeletedOnPlatform bool
	Source            string
	SourceRef         string
	SourceHash        string
	ImportSeq         sql.NullInt64
	CreatedAt         int64
	UpdatedAt         int64
}

const storedMessageColumns = `seq, conversation_seq, sent_at, sender_handle, kind, body, body_source, attachments, embeds, buttons,
	reply_to_source_ref, call_seconds, edited, deleted_on_platform, source, source_ref, source_hash, import_seq, created_at, updated_at`

func scanStoredMessage(row interface{ Scan(...any) error }) (storedMessageRow, error) {
	var stored storedMessageRow
	err := row.Scan(&stored.Seq, &stored.ConversationSeq, &stored.SentAt, &stored.SenderHandle, &stored.Kind, &stored.Body, &stored.BodySource,
		&stored.Attachments, &stored.Embeds, &stored.Buttons, &stored.ReplyToSourceRef, &stored.CallSeconds, &stored.Edited, &stored.DeletedOnPlatform,
		&stored.Source, &stored.SourceRef, &stored.SourceHash, &stored.ImportSeq, &stored.CreatedAt, &stored.UpdatedAt)
	return stored, err
}

// rowJSON is the row as one JSON object, with the keys, order and values the
// standalone store wrote (SQLite's json_object): ids as their public form,
// flags as 0 or 1, and the JSON columns as JSON, so rows retired before and
// after the move read the same.
func (stored storedMessageRow) rowJSON() (string, error) {
	var builder orderedJSONObject
	builder.add("seq", stored.Seq)
	builder.add("id", postgresstorage.FormatRecordID(MessageIDPrefix, stored.Seq))
	builder.add("conversation_id", postgresstorage.FormatRecordID(ConversationIDPrefix, stored.ConversationSeq))
	builder.add("sent_at", stored.SentAt)
	builder.add("sender_handle", stored.SenderHandle)
	builder.add("kind", stored.Kind)
	builder.add("body", stored.Body)
	builder.add("body_source", stored.BodySource)
	builder.addRaw("attachments", stored.Attachments)
	builder.addRaw("embeds", stored.Embeds)
	builder.addRaw("buttons", stored.Buttons)
	builder.add("reply_to_source_ref", stored.ReplyToSourceRef)
	builder.add("call_seconds", stored.CallSeconds)
	builder.add("edited", boolInt(stored.Edited))
	builder.add("deleted_on_platform", boolInt(stored.DeletedOnPlatform))
	builder.add("source", stored.Source)
	builder.add("source_ref", stored.SourceRef)
	builder.add("source_hash", stored.SourceHash)
	builder.add("imported_by", formatOptionalImport(stored.ImportSeq))
	builder.add("created_at", stored.CreatedAt)
	builder.add("updated_at", stored.UpdatedAt)
	return builder.String()
}

// orderedJSONObject writes a JSON object with its keys in the order added,
// compact, as SQLite's json_object does.
type orderedJSONObject struct {
	parts []string
	err   error
}

func (object *orderedJSONObject) add(key string, value any) {
	encoded, err := encodeJSONLikeSQLite(value)
	if err != nil {
		object.err = errors.Join(object.err, fmt.Errorf("%s: %w", key, err))
		return
	}
	object.addRaw(key, encoded)
}

func (object *orderedJSONObject) addRaw(key, encoded string) {
	if !json.Valid([]byte(encoded)) {
		object.err = errors.Join(object.err, fmt.Errorf("%s is not JSON", key))
		return
	}
	name, err := encodeJSONLikeSQLite(key)
	if err != nil {
		object.err = errors.Join(object.err, err)
		return
	}
	object.parts = append(object.parts, name+":"+encoded)
}

// encodeJSONLikeSQLite writes value as JSON without escaping <, > and &,
// which SQLite's json functions leave as they are.
func encodeJSONLikeSQLite(value any) (string, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func (object *orderedJSONObject) String() (string, error) {
	if object.err != nil {
		return "", object.err
	}
	return "{" + strings.Join(object.parts, ",") + "}", nil
}

// storedMessage is what an import may merge into: the fields of a stored
// message one source can know and another not.
type storedMessage struct {
	SenderHandle      string
	Body              string
	Attachments       []Attachment
	Embeds            []Embed
	Buttons           []Button
	BodySource        string
	Kind              string
	ReplyToSourceRef  string
	CallSeconds       int
	Edited            bool
	DeletedOnPlatform bool
}

// mergeMessage is a stored message after an import writes it again. The
// store is the record, so a source that knows less never takes away what
// another said: text an import sends empty keeps the stored text (a live log
// that dropped a custom emoji, a bridge that moved a GIF link out of the
// body), every stored attachment stays, and a stored photo link stays on its
// file; a delete or an edit once seen stays seen. Text an import sends
// replaces the stored text, which is how an edit arrives; the old text is in
// message_revisions. A bridge's text never replaces the platform's own: a
// bridge rewrites what it carries, and an edit reaches the store from the
// platform's reading too. Embeds and buttons are a bot's card as one whole: an
// import that sends any replaces them, one that sends none keeps them.
func mergeMessage(kept storedMessage, incoming ImportMessage) storedMessage {
	merged := storedMessage{
		SenderHandle:      firstNonEmpty(incoming.SenderHandle, kept.SenderHandle),
		Body:              kept.Body,
		BodySource:        kept.BodySource,
		ReplyToSourceRef:  firstNonEmpty(incoming.ReplyToSourceRef, kept.ReplyToSourceRef),
		CallSeconds:       incoming.CallSeconds,
		Edited:            incoming.Edited || kept.Edited,
		DeletedOnPlatform: incoming.DeletedOnPlatform || kept.DeletedOnPlatform,
		Embeds:            kept.Embeds,
		Buttons:           kept.Buttons,
	}
	// The incoming text replaces the stored text unless it is empty, or a
	// bridge's rendering of text the platform itself gave. What kind of
	// message it is follows the same rule: a bridge calls a platform's
	// system line plain text.
	bridgeOverPlatform := incoming.BodySource == "bridge" && kept.BodySource == "platform"
	if incoming.Body != "" && !bridgeOverPlatform {
		merged.Body, merged.BodySource = incoming.Body, incoming.BodySource
	}
	merged.Kind = incoming.Kind
	if bridgeOverPlatform {
		merged.Kind = kept.Kind
	}
	if len(incoming.Embeds) > 0 {
		merged.Embeds = incoming.Embeds
	}
	if len(incoming.Buttons) > 0 {
		merged.Buttons = incoming.Buttons
	}
	if merged.Embeds == nil {
		merged.Embeds = []Embed{}
	}
	if merged.Buttons == nil {
		merged.Buttons = []Button{}
	}
	if merged.CallSeconds == 0 {
		merged.CallSeconds = kept.CallSeconds
	}
	photoOf := map[string]string{}
	for _, attachment := range kept.Attachments {
		if attachment.PhotoID != "" && attachment.Name != "" {
			photoOf[attachment.Name] = attachment.PhotoID
		}
	}
	named := map[string]bool{}
	for _, attachment := range incoming.Attachments {
		if attachment.PhotoID == "" {
			attachment.PhotoID = photoOf[attachment.Name]
		}
		named[attachment.Name] = true
		merged.Attachments = append(merged.Attachments, attachment)
	}
	for _, attachment := range kept.Attachments {
		if !named[attachment.Name] {
			merged.Attachments = append(merged.Attachments, attachment)
		}
	}
	if merged.Attachments == nil {
		merged.Attachments = []Attachment{}
	}
	return merged
}

func firstNonEmpty(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

func marshalJSONText(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func (w *batchWriter) upsertMessage(message ImportMessage) error {
	if message.Source == "" || message.SourceRef == "" {
		return fmt.Errorf("%w: source and source_ref are required", ErrInvalid)
	}
	if !contains(MessageKinds, message.Kind) {
		return fmt.Errorf("%w: kind %q is not one of %v", ErrInvalid, message.Kind, MessageKinds)
	}
	if !contains(BodySources, message.BodySource) {
		return fmt.Errorf("%w: body_source %q is not one of %v", ErrInvalid, message.BodySource, BodySources)
	}
	if message.SentAt <= 0 {
		return fmt.Errorf("%w: sent_at is required", ErrInvalid)
	}
	// Another row holds a message whose key was superseded: skip it.
	var retiredAs int64
	err := w.queryRow(`SELECT superseded_by_seq FROM superseded_messages WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		w.owner, message.Source, message.SourceRef).Scan(&retiredAs)
	if err == nil {
		w.counts.MessagesSupersededSkipped++
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	conversationSeq, err := w.conversationSeq(message.ConversationSource, message.ConversationSourceRef)
	if err != nil {
		return err
	}
	if message.SenderHandle != "" {
		// A sender the conversation never listed still gets a row, so every
		// message's sender can be named later in one place.
		if _, err := w.exec(`INSERT INTO participants (owner_principal_id, conversation_seq, handle) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			w.owner, conversationSeq, message.SenderHandle); err != nil {
			return err
		}
	}
	attachments := message.Attachments
	if attachments == nil {
		attachments = []Attachment{}
	}
	embeds, buttons := message.Embeds, message.Buttons
	if embeds == nil {
		embeds = []Embed{}
	}
	if buttons == nil {
		buttons = []Button{}
	}
	attachmentsJSON, err := marshalJSONText(attachments)
	if err != nil {
		return err
	}
	embedsJSON, err := marshalJSONText(embeds)
	if err != nil {
		return err
	}
	buttonsJSON, err := marshalJSONText(buttons)
	if err != nil {
		return err
	}
	kept, err := scanStoredMessage(w.queryRow(`SELECT `+storedMessageColumns+` FROM messages WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		w.owner, message.Source, message.SourceRef))
	var seq int64
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := w.queryRow(`INSERT INTO messages (owner_principal_id, conversation_seq, sent_at, sender_handle, kind, body, body_source, attachments, embeds, buttons,
				reply_to_source_ref, call_seconds, edited, deleted_on_platform, source, source_ref, source_hash, import_seq, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19) RETURNING seq`,
			w.owner, conversationSeq, message.SentAt, message.SenderHandle, message.Kind, message.Body, message.BodySource, attachmentsJSON, embedsJSON, buttonsJSON,
			message.ReplyToSourceRef, message.CallSeconds, message.Edited, message.DeletedOnPlatform, message.Source, message.SourceRef, message.SourceHash,
			w.importSeq, w.now).Scan(&seq); err != nil {
			return err
		}
		w.counts.MessagesCreated++
	case err != nil:
		return err
	case message.SourceHash != "" && message.SourceHash == kept.SourceHash:
		seq = kept.Seq
		w.counts.MessagesUnchanged++
	default:
		seq = kept.Seq
		keptMessage := storedMessage{SenderHandle: kept.SenderHandle, Body: kept.Body, BodySource: kept.BodySource, Kind: kept.Kind,
			ReplyToSourceRef: kept.ReplyToSourceRef, CallSeconds: kept.CallSeconds, Edited: kept.Edited, DeletedOnPlatform: kept.DeletedOnPlatform}
		messageID := postgresstorage.FormatRecordID(MessageIDPrefix, kept.Seq)
		if err := json.Unmarshal([]byte(kept.Attachments), &keptMessage.Attachments); err != nil {
			return fmt.Errorf("message %s: stored attachments: %w", messageID, err)
		}
		if err := json.Unmarshal([]byte(kept.Embeds), &keptMessage.Embeds); err != nil {
			return fmt.Errorf("message %s: stored embeds: %w", messageID, err)
		}
		if err := json.Unmarshal([]byte(kept.Buttons), &keptMessage.Buttons); err != nil {
			return fmt.Errorf("message %s: stored buttons: %w", messageID, err)
		}
		merged := mergeMessage(keptMessage, message)
		mergedAttachments, err := marshalJSONText(merged.Attachments)
		if err != nil {
			return err
		}
		mergedEmbeds, err := marshalJSONText(merged.Embeds)
		if err != nil {
			return err
		}
		mergedButtons, err := marshalJSONText(merged.Buttons)
		if err != nil {
			return err
		}
		// Another source read the same message: nothing to write, and no
		// revision of a row that did not change.
		if conversationSeq == kept.ConversationSeq && message.SentAt == kept.SentAt && merged.Kind == kept.Kind &&
			merged.SenderHandle == kept.SenderHandle && merged.Body == kept.Body && merged.BodySource == kept.BodySource && mergedAttachments == kept.Attachments &&
			mergedEmbeds == kept.Embeds && mergedButtons == kept.Buttons && merged.ReplyToSourceRef == kept.ReplyToSourceRef &&
			merged.CallSeconds == kept.CallSeconds && merged.Edited == kept.Edited && merged.DeletedOnPlatform == kept.DeletedOnPlatform {
			w.counts.MessagesUnchanged++
			break
		}
		rowJSON, err := kept.rowJSON()
		if err != nil {
			return fmt.Errorf("message %s: %w", messageID, err)
		}
		if _, err := w.exec(`INSERT INTO message_revisions (owner_principal_id, message_seq, import_seq, revised_at, row_json) VALUES ($1, $2, $3, $4, $5)`,
			w.owner, kept.Seq, w.importSeq, w.now, rowJSON); err != nil {
			return err
		}
		if _, err := w.exec(`UPDATE messages SET conversation_seq = $1, sent_at = $2, sender_handle = $3, kind = $4, body = $5, body_source = $6, attachments = $7,
				embeds = $8, buttons = $9, reply_to_source_ref = $10, call_seconds = $11, edited = $12, deleted_on_platform = $13, source_hash = $14,
				import_seq = $15, updated_at = $16 WHERE seq = $17`,
			conversationSeq, message.SentAt, merged.SenderHandle, merged.Kind, merged.Body, merged.BodySource, mergedAttachments, mergedEmbeds, mergedButtons,
			merged.ReplyToSourceRef, merged.CallSeconds, merged.Edited, merged.DeletedOnPlatform, message.SourceHash, w.importSeq, w.now, kept.Seq); err != nil {
			return err
		}
		w.counts.MessagesUpdated++
	}
	for _, old := range message.Supersedes {
		if err := w.supersede(old, seq); err != nil {
			return err
		}
	}
	return nil
}

// supersede retires the row filed under old in the owner's records, if there
// is one, in favor of the message supersededBySeq: it copies every column to
// superseded_messages and removes the row. A message cannot supersede itself.
func (w *batchWriter) supersede(old MessageKey, supersededBySeq int64) error {
	if old.Source == "" || old.SourceRef == "" {
		return fmt.Errorf("%w: a superseded message needs source and source_ref", ErrInvalid)
	}
	retired, err := scanStoredMessage(w.queryRow(`SELECT `+storedMessageColumns+` FROM messages WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		w.owner, old.Source, old.SourceRef))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if retired.Seq == supersededBySeq {
		return fmt.Errorf("%w: %s %s is the message itself", ErrInvalid, old.Source, old.SourceRef)
	}
	rowJSON, err := retired.rowJSON()
	if err != nil {
		return err
	}
	if _, err := w.exec(`INSERT INTO superseded_messages (message_seq, owner_principal_id, source, source_ref, superseded_by_seq, import_seq, superseded_at, row_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, retired.Seq, w.owner, old.Source, old.SourceRef, supersededBySeq, w.importSeq, w.now, rowJSON); err != nil {
		return err
	}
	// A row retired earlier in favor of the one now retired follows it.
	if _, err := w.exec(`UPDATE superseded_messages SET superseded_by_seq = $1 WHERE superseded_by_seq = $2`, supersededBySeq, retired.Seq); err != nil {
		return err
	}
	if _, err := w.exec(`DELETE FROM messages WHERE seq = $1`, retired.Seq); err != nil {
		return err
	}
	w.counts.MessagesSuperseded++
	return nil
}

// DaySpan is the start (inclusive) and end (exclusive) of a local day, in
// unix milliseconds. A day is midnight to midnight where the user lived, so a
// day across a clock change is 23 or 25 hours.
func DaySpan(day, timeZone string) (int64, int64, error) {
	location, err := time.LoadLocation(timeZone)
	if err != nil || timeZone == "" {
		return 0, 0, fmt.Errorf("%w: time zone %q: %v", ErrInvalid, timeZone, err)
	}
	start, err := time.ParseInLocation("2006-01-02", day, location)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: day %q is not YYYY-MM-DD", ErrInvalid, day)
	}
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli(), nil
}

func (w *batchWriter) upsertDay(day ImportConversationDay) error {
	conversationSeq, err := w.conversationSeq(day.ConversationSource, day.ConversationSourceRef)
	if err != nil {
		return err
	}
	startsAt, endsAt, err := DaySpan(day.Day, day.TimeZone)
	if err != nil {
		return err
	}
	var seq int64
	err = w.queryRow(`SELECT seq FROM conversation_days WHERE conversation_seq = $1 AND day = $2`, conversationSeq, day.Day).Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := w.exec(`INSERT INTO conversation_days (owner_principal_id, conversation_seq, day, time_zone, starts_at, ends_at, journal_source, journal_source_ref, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`, w.owner, conversationSeq, day.Day, day.TimeZone, startsAt, endsAt, day.JournalSource, day.JournalSourceRef, w.now); err != nil {
			return err
		}
		w.counts.DaysCreated++
	case err != nil:
		return err
	default:
		// A day keeps the journal transcript it was first given unless the
		// batch names another; an importer that knows no transcript sends ''.
		result, err := w.exec(`UPDATE conversation_days SET time_zone = $1, starts_at = $2, ends_at = $3,
				journal_source = CASE WHEN $4 <> '' THEN $4 ELSE journal_source END,
				journal_source_ref = CASE WHEN $5 <> '' THEN $5 ELSE journal_source_ref END,
				updated_at = $6
			WHERE seq = $7 AND (time_zone <> $1 OR starts_at <> $2 OR ends_at <> $3 OR ($4 <> '' AND (journal_source <> $4 OR journal_source_ref <> $5)))`,
			day.TimeZone, startsAt, endsAt, day.JournalSource, day.JournalSourceRef, w.now, seq)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected > 0 {
			w.counts.DaysUpdated++
		}
	}
	return nil
}

// LinkJournalEntries records which journal-store entry holds each transcript,
// by the transcript's source and source_ref, among the days in scope.
func (s *Store) LinkJournalEntries(ctx context.Context, scope ownership.Scope, links []JournalEntryLink) (JournalEntryLinkResult, error) {
	if err := scope.Check(); err != nil {
		return JournalEntryLinkResult{}, err
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	result := JournalEntryLinkResult{Unmatched: []JournalEntryLink{}}
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		now := nowMillis()
		for _, link := range links {
			if link.JournalSource == "" || link.JournalSourceRef == "" || link.JournalEntryID == "" {
				return fmt.Errorf("%w: journal_source, journal_source_ref and journal_entry_id are required", ErrInvalid)
			}
			args := &arguments{}
			query := `UPDATE conversation_days SET journal_entry_id = ` + args.add(link.JournalEntryID) + `, updated_at = ` + args.add(now) +
				` WHERE journal_source = ` + args.add(link.JournalSource) + ` AND journal_source_ref = ` + args.add(link.JournalSourceRef)
			condition, err := args.scope(scope, "owner_principal_id")
			if err != nil {
				return err
			}
			updated, err := tx.ExecContext(ctx, query+` AND `+condition, args.values...)
			if err != nil {
				return err
			}
			affected, err := updated.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				result.Unmatched = append(result.Unmatched, link)
			} else {
				result.Linked++
			}
		}
		return nil
	})
	if err != nil {
		return JournalEntryLinkResult{}, err
	}
	return result, nil
}

// SetParticipantPerson names the person behind a handle in one conversation
// in scope; an empty person id clears it.
func (s *Store) SetParticipantPerson(ctx context.Context, scope ownership.Scope, conversationID, handle, personID string) (Participant, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	conversationSeq, err := recordNumber(ConversationIDPrefix, "conversation", conversationID)
	if err != nil {
		return Participant{}, fmt.Errorf("%w: no participant %s in %s", ErrNotFound, handle, conversationID)
	}
	args := &arguments{}
	query := `UPDATE participants SET person_id = ` + args.add(personID) + ` WHERE conversation_seq = ` + args.add(conversationSeq) + ` AND handle = ` + args.add(handle)
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return Participant{}, err
	}
	var participant Participant
	err = s.db.QueryRowContext(ctx, query+` AND `+condition+` RETURNING handle, display_name, person_id, is_self`, args.values...).
		Scan(&participant.Handle, &participant.DisplayName, &participant.PersonID, &participant.IsSelf)
	if errors.Is(err, sql.ErrNoRows) {
		return Participant{}, fmt.Errorf("%w: no participant %s in %s", ErrNotFound, handle, conversationID)
	}
	return participant, err
}
