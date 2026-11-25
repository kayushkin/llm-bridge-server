package messagestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StartImport records a new import run.
func (s *Store) StartImport(start ImportStart) (Import, error) {
	if strings.TrimSpace(start.Importer) == "" {
		return Import{}, fmt.Errorf("%w: importer is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Import{}, err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "imports")
	if err != nil {
		return Import{}, err
	}
	run := Import{ID: formatID(prefixImport, seq), Importer: start.Importer, Description: start.Description, StartedAt: nowMillis()}
	if _, err := tx.Exec(`INSERT INTO imports (seq, id, importer, description, started_at) VALUES (?,?,?,?,?)`,
		seq, run.ID, run.Importer, run.Description, run.StartedAt); err != nil {
		return Import{}, err
	}
	return run, tx.Commit()
}

// GetImport reads one import run.
func (s *Store) GetImport(id string) (Import, error) {
	var run Import
	var counts string
	err := s.db.QueryRow(`SELECT id, importer, description, started_at, finished_at, counts FROM imports WHERE id = ?`, id).
		Scan(&run.ID, &run.Importer, &run.Description, &run.StartedAt, &run.FinishedAt, &counts)
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, fmt.Errorf("%w: import %s", ErrNotFound, id)
	}
	if err != nil {
		return Import{}, err
	}
	return run, json.Unmarshal([]byte(counts), &run.Counts)
}

// ListImports lists import runs, newest first.
func (s *Store) ListImports() ([]Import, error) {
	rows, err := s.db.Query(`SELECT id FROM imports ORDER BY seq DESC`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	runs := []Import{}
	for _, id := range ids {
		run, err := s.GetImport(id)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// FinishImport marks an import run done.
func (s *Store) FinishImport(id string) (Import, error) {
	s.writeMutex.Lock()
	result, err := s.db.Exec(`UPDATE imports SET finished_at = ? WHERE id = ?`, nowMillis(), id)
	s.writeMutex.Unlock()
	if err != nil {
		return Import{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Import{}, fmt.Errorf("%w: import %s", ErrNotFound, id)
	}
	return s.GetImport(id)
}

type conversationKey struct{ source, sourceRef string }

// errMergedKey marks, inside upsertConversation, a key that was merged.
var errMergedKey = errors.New("merged conversation key")

// batchWriter holds one batch's transaction and its id counters.
type batchWriter struct {
	tx            *sql.Tx
	importID      string
	now           int64
	counts        ImportCounts
	conversations map[conversationKey]string
	nextSeqs      map[string]int64
}

func (w *batchWriter) mint(table, prefix string) (int64, string, error) {
	seq, known := w.nextSeqs[table]
	if !known {
		var err error
		if seq, err = nextSeq(w.tx, table); err != nil {
			return 0, "", err
		}
	}
	w.nextSeqs[table] = seq + 1
	return seq, formatID(prefix, seq), nil
}

// ApplyBatch writes one batch inside one transaction: one bad record writes
// nothing.
func (s *Store) ApplyBatch(importID string, batch ImportBatch) (ImportCounts, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ImportCounts{}, err
	}
	defer tx.Rollback()
	var finishedAt int64
	if err := tx.QueryRow(`SELECT finished_at FROM imports WHERE id = ?`, importID).Scan(&finishedAt); errors.Is(err, sql.ErrNoRows) {
		return ImportCounts{}, fmt.Errorf("%w: import %s", ErrNotFound, importID)
	} else if err != nil {
		return ImportCounts{}, err
	}
	if finishedAt != 0 {
		return ImportCounts{}, fmt.Errorf("%w: import %s is finished", ErrInvalid, importID)
	}
	w := &batchWriter{tx: tx, importID: importID, now: nowMillis(), conversations: map[conversationKey]string{}, nextSeqs: map[string]int64{}}
	for i, conversation := range batch.Conversations {
		if err := w.upsertConversation(conversation); err != nil {
			return ImportCounts{}, fmt.Errorf("conversation %d (%s %s): %w", i, conversation.Source, conversation.SourceRef, err)
		}
	}
	for i, message := range batch.Messages {
		if err := w.upsertMessage(message); err != nil {
			return ImportCounts{}, fmt.Errorf("message %d (%s %s): %w", i, message.Source, message.SourceRef, err)
		}
	}
	for i, supersession := range batch.Supersessions {
		if err := w.applySupersession(supersession); err != nil {
			return ImportCounts{}, fmt.Errorf("supersession %d (%s %s by %s %s): %w", i, supersession.Superseded.Source, supersession.Superseded.SourceRef,
				supersession.By.Source, supersession.By.SourceRef, err)
		}
	}
	for i, merge := range batch.ConversationMerges {
		if err := w.applyMerge(merge); err != nil {
			return ImportCounts{}, fmt.Errorf("conversation merge %d (%s %s into %s %s): %w", i, merge.Merged.Source, merge.Merged.SourceRef,
				merge.Into.Source, merge.Into.SourceRef, err)
		}
	}
	for i, day := range batch.ConversationDays {
		if err := w.upsertDay(day); err != nil {
			return ImportCounts{}, fmt.Errorf("conversation day %d (%s %s %s): %w", i, day.ConversationSource, day.ConversationSourceRef, day.Day, err)
		}
	}
	var countsJSON string
	if err := tx.QueryRow(`SELECT counts FROM imports WHERE id = ?`, importID).Scan(&countsJSON); err != nil {
		return ImportCounts{}, err
	}
	var total ImportCounts
	if err := json.Unmarshal([]byte(countsJSON), &total); err != nil {
		return ImportCounts{}, err
	}
	total.add(w.counts)
	encoded, _ := json.Marshal(total)
	if _, err := tx.Exec(`UPDATE imports SET counts = ? WHERE id = ?`, string(encoded), importID); err != nil {
		return ImportCounts{}, err
	}
	return w.counts, tx.Commit()
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
	var byID string
	err := w.tx.QueryRow(`SELECT id FROM messages WHERE source = ? AND source_ref = ?`, supersession.By.Source, supersession.By.SourceRef).Scan(&byID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no message %s %s to supersede with", ErrInvalid, supersession.By.Source, supersession.By.SourceRef)
	}
	if err != nil {
		return err
	}
	return w.supersede(supersession.Superseded, byID)
}

func (w *batchWriter) conversationID(source, sourceRef string) (string, error) {
	key := conversationKey{source, sourceRef}
	if id, known := w.conversations[key]; known {
		return id, nil
	}
	id, _, err := liveConversationID(w.tx, ConversationKey{source, sourceRef})
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: no conversation %s %s; send it first", ErrInvalid, source, sourceRef)
	}
	w.conversations[key] = id
	return id, nil
}

func (w *batchWriter) upsertConversation(conversation ImportConversation) error {
	if conversation.Source == "" || conversation.SourceRef == "" {
		return fmt.Errorf("%w: source and source_ref are required", ErrInvalid)
	}
	if !contains(Platforms, conversation.Platform) {
		return fmt.Errorf("%w: platform %q is not one of %v", ErrInvalid, conversation.Platform, Platforms)
	}
	var id, platform, title, parentSourceRef string
	var isGroup int
	err := w.tx.QueryRow(`SELECT id, platform, title, is_group, parent_source_ref FROM conversations WHERE source = ? AND source_ref = ?`,
		conversation.Source, conversation.SourceRef).Scan(&id, &platform, &title, &isGroup, &parentSourceRef)
	if errors.Is(err, sql.ErrNoRows) {
		// A merged conversation's key stands for the one it merged into, which
		// keeps its own title and kind and gains the participants.
		var mergedInto string
		lookup := w.tx.QueryRow(`SELECT merged_into FROM conversation_merges WHERE source = ? AND source_ref = ?`, conversation.Source, conversation.SourceRef).Scan(&mergedInto)
		switch {
		case lookup == nil:
			id, err = mergedInto, errMergedKey
		case !errors.Is(lookup, sql.ErrNoRows):
			return lookup
		}
	}
	switch {
	case errors.Is(err, errMergedKey):
	case errors.Is(err, sql.ErrNoRows):
		seq, newID, err := w.mint("conversations", prefixConversation)
		if err != nil {
			return err
		}
		id = newID
		if _, err := w.tx.Exec(`INSERT INTO conversations (seq, id, platform, source, source_ref, title, is_group, parent_source_ref, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			seq, id, conversation.Platform, conversation.Source, conversation.SourceRef, conversation.Title, boolInt(conversation.IsGroup), conversation.ParentSourceRef, w.now, w.now); err != nil {
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
		if platform != conversation.Platform || newTitle != title || isGroup != boolInt(conversation.IsGroup) || newParent != parentSourceRef {
			if _, err := w.tx.Exec(`UPDATE conversations SET platform = ?, title = ?, is_group = ?, parent_source_ref = ?, updated_at = ? WHERE id = ?`,
				conversation.Platform, newTitle, boolInt(conversation.IsGroup), newParent, w.now, id); err != nil {
				return err
			}
			w.counts.ConversationsUpdated++
		}
	}
	w.conversations[conversationKey{conversation.Source, conversation.SourceRef}] = id
	for _, participant := range conversation.Participants {
		if participant.Handle == "" {
			return fmt.Errorf("%w: a participant has no handle", ErrInvalid)
		}
		if _, err := w.tx.Exec(`INSERT INTO participants (conversation_id, handle, display_name, person_id, is_self) VALUES (?,?,?,?,?)
			ON CONFLICT (conversation_id, handle) DO UPDATE SET
				display_name = CASE WHEN excluded.display_name != '' THEN excluded.display_name ELSE display_name END,
				person_id    = CASE WHEN excluded.person_id    != '' THEN excluded.person_id    ELSE person_id END,
				is_self      = CASE WHEN excluded.is_self = 1 THEN 1 ELSE is_self END`,
			id, participant.Handle, participant.DisplayName, participant.PersonID, boolInt(participant.IsSelf)); err != nil {
			return err
		}
	}
	return nil
}

// messageRowJSON selects every column of a messages row as one JSON object,
// the form superseded and revised rows are kept in.
const messageRowJSON = `json_object(
			'seq', seq, 'id', id, 'conversation_id', conversation_id, 'sent_at', sent_at, 'sender_handle', sender_handle,
			'kind', kind, 'body', body, 'body_source', body_source, 'attachments', json(attachments), 'embeds', json(embeds), 'buttons', json(buttons), 'reply_to_source_ref', reply_to_source_ref,
			'call_seconds', call_seconds, 'edited', edited, 'deleted_on_platform', deleted_on_platform, 'source', source,
			'source_ref', source_ref, 'source_hash', source_hash, 'imported_by', imported_by, 'created_at', created_at,
			'updated_at', updated_at)`

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
	var retiredAs string
	err := w.tx.QueryRow(`SELECT superseded_by FROM superseded_messages WHERE source = ? AND source_ref = ?`, message.Source, message.SourceRef).Scan(&retiredAs)
	if err == nil {
		w.counts.MessagesSupersededSkipped++
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	conversationID, err := w.conversationID(message.ConversationSource, message.ConversationSourceRef)
	if err != nil {
		return err
	}
	if message.SenderHandle != "" {
		// A sender the conversation never listed still gets a row, so every
		// message's sender can be named later in one place.
		if _, err := w.tx.Exec(`INSERT OR IGNORE INTO participants (conversation_id, handle) VALUES (?, ?)`, conversationID, message.SenderHandle); err != nil {
			return err
		}
	}
	attachments := message.Attachments
	if attachments == nil {
		attachments = []Attachment{}
	}
	attachmentsJSON, _ := json.Marshal(attachments)
	embeds, buttons := message.Embeds, message.Buttons
	if embeds == nil {
		embeds = []Embed{}
	}
	if buttons == nil {
		buttons = []Button{}
	}
	embedsJSON, _ := json.Marshal(embeds)
	buttonsJSON, _ := json.Marshal(buttons)
	var id, hash string
	err = w.tx.QueryRow(`SELECT id, source_hash FROM messages WHERE source = ? AND source_ref = ?`, message.Source, message.SourceRef).Scan(&id, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		seq, newID, err := w.mint("messages", prefixMessage)
		if err != nil {
			return err
		}
		_, err = w.tx.Exec(`INSERT INTO messages (seq, id, conversation_id, sent_at, sender_handle, kind, body, body_source, attachments, embeds, buttons, reply_to_source_ref, call_seconds,
				edited, deleted_on_platform, source, source_ref, source_hash, imported_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			seq, newID, conversationID, message.SentAt, message.SenderHandle, message.Kind, message.Body, message.BodySource, string(attachmentsJSON), string(embedsJSON), string(buttonsJSON), message.ReplyToSourceRef,
			message.CallSeconds, boolInt(message.Edited), boolInt(message.DeletedOnPlatform), message.Source, message.SourceRef, message.SourceHash,
			w.importID, w.now, w.now)
		if err != nil {
			return err
		}
		id = newID
		w.counts.MessagesCreated++
	case err != nil:
		return err
	case message.SourceHash != "" && message.SourceHash == hash:
		w.counts.MessagesUnchanged++
	default:
		var kept storedMessage
		var keptAttachments, keptEmbeds, keptButtons, rowJSON, keptConversationID string
		var keptSentAt int64
		var edited, deleted int
		if err := w.tx.QueryRow(`SELECT conversation_id, sent_at, kind, sender_handle, body, body_source, attachments, embeds, buttons, reply_to_source_ref, call_seconds, edited, deleted_on_platform, `+messageRowJSON+`
				FROM messages WHERE id = ?`, id).Scan(&keptConversationID, &keptSentAt, &kept.Kind, &kept.SenderHandle, &kept.Body, &kept.BodySource, &keptAttachments, &keptEmbeds, &keptButtons, &kept.ReplyToSourceRef, &kept.CallSeconds,
			&edited, &deleted, &rowJSON); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(keptAttachments), &kept.Attachments); err != nil {
			return fmt.Errorf("message %s: stored attachments: %w", id, err)
		}
		if err := json.Unmarshal([]byte(keptEmbeds), &kept.Embeds); err != nil {
			return fmt.Errorf("message %s: stored embeds: %w", id, err)
		}
		if err := json.Unmarshal([]byte(keptButtons), &kept.Buttons); err != nil {
			return fmt.Errorf("message %s: stored buttons: %w", id, err)
		}
		kept.Edited, kept.DeletedOnPlatform = edited == 1, deleted == 1
		merged := mergeMessage(kept, message)
		mergedAttachments, _ := json.Marshal(merged.Attachments)
		mergedEmbeds, _ := json.Marshal(merged.Embeds)
		mergedButtons, _ := json.Marshal(merged.Buttons)
		// Another source read the same message: nothing to write, and no
		// revision of a row that did not change.
		if conversationID == keptConversationID && message.SentAt == keptSentAt && merged.Kind == kept.Kind &&
			merged.SenderHandle == kept.SenderHandle && merged.Body == kept.Body && merged.BodySource == kept.BodySource && string(mergedAttachments) == keptAttachments &&
			string(mergedEmbeds) == keptEmbeds && string(mergedButtons) == keptButtons && merged.ReplyToSourceRef == kept.ReplyToSourceRef &&
			merged.CallSeconds == kept.CallSeconds && merged.Edited == kept.Edited && merged.DeletedOnPlatform == kept.DeletedOnPlatform {
			w.counts.MessagesUnchanged++
			break
		}
		if _, err := w.tx.Exec(`INSERT INTO message_revisions (message_id, import_id, revised_at, row_json) VALUES (?,?,?,?)`,
			id, w.importID, w.now, rowJSON); err != nil {
			return err
		}
		_, err = w.tx.Exec(`UPDATE messages SET conversation_id = ?, sent_at = ?, sender_handle = ?, kind = ?, body = ?, body_source = ?, attachments = ?, embeds = ?, buttons = ?, reply_to_source_ref = ?,
				call_seconds = ?, edited = ?, deleted_on_platform = ?, source_hash = ?, imported_by = ?, updated_at = ? WHERE id = ?`,
			conversationID, message.SentAt, merged.SenderHandle, merged.Kind, merged.Body, merged.BodySource, string(mergedAttachments), string(mergedEmbeds), string(mergedButtons), merged.ReplyToSourceRef,
			merged.CallSeconds, boolInt(merged.Edited), boolInt(merged.DeletedOnPlatform), message.SourceHash, w.importID, w.now, id)
		if err != nil {
			return err
		}
		w.counts.MessagesUpdated++
	}
	for _, old := range message.Supersedes {
		if err := w.supersede(old, id); err != nil {
			return err
		}
	}
	return nil
}

// supersede retires the row filed under old, if there is one, in favor of
// the message supersededBy: it copies every column to superseded_messages
// and removes the row. A message cannot supersede itself.
func (w *batchWriter) supersede(old MessageKey, supersededBy string) error {
	if old.Source == "" || old.SourceRef == "" {
		return fmt.Errorf("%w: a superseded message needs source and source_ref", ErrInvalid)
	}
	var oldID, rowJSON string
	err := w.tx.QueryRow(`SELECT id, `+messageRowJSON+`
		FROM messages WHERE source = ? AND source_ref = ?`, old.Source, old.SourceRef).Scan(&oldID, &rowJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if oldID == supersededBy {
		return fmt.Errorf("%w: %s %s is the message itself", ErrInvalid, old.Source, old.SourceRef)
	}
	if _, err := w.tx.Exec(`INSERT INTO superseded_messages (id, source, source_ref, superseded_by, import_id, superseded_at, row_json)
		VALUES (?,?,?,?,?,?,?)`, oldID, old.Source, old.SourceRef, supersededBy, w.importID, w.now, rowJSON); err != nil {
		return err
	}
	// A row retired earlier in favor of the one now retired follows it.
	if _, err := w.tx.Exec(`UPDATE superseded_messages SET superseded_by = ? WHERE superseded_by = ?`, supersededBy, oldID); err != nil {
		return err
	}
	if _, err := w.tx.Exec(`DELETE FROM messages WHERE id = ?`, oldID); err != nil {
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
	conversationID, err := w.conversationID(day.ConversationSource, day.ConversationSourceRef)
	if err != nil {
		return err
	}
	startsAt, endsAt, err := DaySpan(day.Day, day.TimeZone)
	if err != nil {
		return err
	}
	var id string
	err = w.tx.QueryRow(`SELECT id FROM conversation_days WHERE conversation_id = ? AND day = ?`, conversationID, day.Day).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		seq, newID, err := w.mint("conversation_days", prefixConversationDay)
		if err != nil {
			return err
		}
		if _, err := w.tx.Exec(`INSERT INTO conversation_days (seq, id, conversation_id, day, time_zone, starts_at, ends_at, journal_source, journal_source_ref, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, seq, newID, conversationID, day.Day, day.TimeZone, startsAt, endsAt, day.JournalSource, day.JournalSourceRef, w.now, w.now); err != nil {
			return err
		}
		w.counts.DaysCreated++
	case err != nil:
		return err
	default:
		// A day keeps the journal transcript it was first given unless the
		// batch names another; an importer that knows no transcript sends ''.
		result, err := w.tx.Exec(`UPDATE conversation_days SET time_zone = ?, starts_at = ?, ends_at = ?,
				journal_source = CASE WHEN ? != '' THEN ? ELSE journal_source END,
				journal_source_ref = CASE WHEN ? != '' THEN ? ELSE journal_source_ref END,
				updated_at = ?
			WHERE id = ? AND (time_zone != ? OR starts_at != ? OR ends_at != ? OR (? != '' AND (journal_source != ? OR journal_source_ref != ?)))`,
			day.TimeZone, startsAt, endsAt, day.JournalSource, day.JournalSource, day.JournalSourceRef, day.JournalSourceRef, w.now,
			id, day.TimeZone, startsAt, endsAt, day.JournalSource, day.JournalSource, day.JournalSourceRef)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			w.counts.DaysUpdated++
		}
	}
	return nil
}

// LinkJournalEntries records which journal-store entry holds each transcript,
// by the transcript's source and source_ref.
func (s *Store) LinkJournalEntries(links []JournalEntryLink) (JournalEntryLinkResult, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return JournalEntryLinkResult{}, err
	}
	defer tx.Rollback()
	result := JournalEntryLinkResult{Unmatched: []JournalEntryLink{}}
	now := nowMillis()
	for _, link := range links {
		if link.JournalSource == "" || link.JournalSourceRef == "" || link.JournalEntryID == "" {
			return JournalEntryLinkResult{}, fmt.Errorf("%w: journal_source, journal_source_ref and journal_entry_id are required", ErrInvalid)
		}
		updated, err := tx.Exec(`UPDATE conversation_days SET journal_entry_id = ?, updated_at = ? WHERE journal_source = ? AND journal_source_ref = ?`,
			link.JournalEntryID, now, link.JournalSource, link.JournalSourceRef)
		if err != nil {
			return JournalEntryLinkResult{}, err
		}
		if affected, _ := updated.RowsAffected(); affected == 0 {
			result.Unmatched = append(result.Unmatched, link)
		} else {
			result.Linked++
		}
	}
	return result, tx.Commit()
}

// SetParticipantPerson names the person behind a handle in one conversation;
// an empty person id clears it.
func (s *Store) SetParticipantPerson(conversationID, handle, personID string) (Participant, error) {
	s.writeMutex.Lock()
	result, err := s.db.Exec(`UPDATE participants SET person_id = ? WHERE conversation_id = ? AND handle = ?`, personID, conversationID, handle)
	s.writeMutex.Unlock()
	if err != nil {
		return Participant{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Participant{}, fmt.Errorf("%w: no participant %s in %s", ErrNotFound, handle, conversationID)
	}
	var participant Participant
	var isSelf int
	err = s.db.QueryRow(`SELECT handle, display_name, person_id, is_self FROM participants WHERE conversation_id = ? AND handle = ?`, conversationID, handle).
		Scan(&participant.Handle, &participant.DisplayName, &participant.PersonID, &isSelf)
	participant.IsSelf = isSelf == 1
	return participant, err
}
