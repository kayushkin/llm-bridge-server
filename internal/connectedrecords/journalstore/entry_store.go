package journalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const entryColumns = `id, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref,
	source, source_ref, source_hash, created_at, updated_at, deleted_at`

func scanEntry(scan func(...any) error) (*Entry, error) {
	var entry Entry
	var tags string
	err := scan(&entry.ID, &entry.Kind, &entry.Title, &entry.Body, &entry.WrittenAt, &entry.Visibility,
		&entry.PublishedAt, &entry.Slug, &tags, &entry.AboutRef, &entry.Source, &entry.SourceRef,
		&entry.SourceHash, &entry.CreatedAt, &entry.UpdatedAt, &entry.DeletedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tags), &entry.Tags); err != nil {
		return nil, fmt.Errorf("entry %s: tags column is not a JSON array: %w", entry.ID, err)
	}
	if entry.Tags == nil {
		entry.Tags = []string{}
	}
	return &entry, nil
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		entry, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, rows.Err()
}

// getRow reads one entry, deleted or not, without its mentions.
func getRow(q queryer, id string) (*Entry, error) {
	entry, err := scanEntry(q.QueryRow(`SELECT `+entryColumns+` FROM entries WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entry %s", ErrNotFound, id)
	}
	return entry, err
}

// getLive reads one entry that has not been deleted.
func getLive(q queryer, id string) (*Entry, error) {
	entry, err := getRow(q, id)
	if err != nil {
		return nil, err
	}
	if entry.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: entry %s is deleted; POST /entries/%s/restore brings it back", ErrNotFound, id, id)
	}
	return entry, nil
}

// Get returns one entry with its mentions. A deleted entry is a not-found
// unless includeDeleted, so dash's resolver treats it as an honest miss.
func (s *Store) Get(id string, includeDeleted bool) (*Entry, error) {
	var entry *Entry
	var err error
	if includeDeleted {
		entry, err = getRow(s.db, id)
	} else {
		entry, err = getLive(s.db, id)
	}
	if err != nil {
		return nil, err
	}
	if entry.Mentions, err = s.mentionsOf(id, ""); err != nil {
		return nil, err
	}
	return entry, nil
}

func (s *Store) mentionsOf(entryID, ref string) ([]Mention, error) {
	query := `SELECT entry_id, position, line, entity_ref, entity_type, label FROM mentions WHERE entry_id = ?`
	args := []any{entryID}
	if ref != "" {
		query += ` AND entity_ref = ?`
		args = append(args, ref)
	}
	rows, err := s.db.Query(query+` ORDER BY position`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mentions := []Mention{}
	for rows.Next() {
		var mention Mention
		if err := rows.Scan(&mention.EntryID, &mention.Position, &mention.Line, &mention.EntityRef, &mention.EntityType, &mention.Label); err != nil {
			return nil, err
		}
		mentions = append(mentions, mention)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Then the links in the timeline's live items, in time order.
	itemQuery := `SELECT i.timeline_id, m.item_id, m.position, m.entity_ref, m.entity_type, m.label
		FROM timeline_item_mentions m JOIN timeline_items i ON i.id = m.item_id
		WHERE i.timeline_id = ? AND i.deleted_at = 0`
	itemArgs := []any{entryID}
	if ref != "" {
		itemQuery += ` AND m.entity_ref = ?`
		itemArgs = append(itemArgs, ref)
	}
	itemRows, err := s.db.Query(itemQuery+` ORDER BY i.occurred_at, i.seq, m.position`, itemArgs...)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var mention Mention
		if err := itemRows.Scan(&mention.EntryID, &mention.ItemID, &mention.Position, &mention.EntityRef, &mention.EntityType, &mention.Label); err != nil {
			return nil, err
		}
		mentions = append(mentions, mention)
	}
	return mentions, itemRows.Err()
}

// normalize checks and tidies the fields every entry must carry, whatever the
// write path. It does not touch the publishing rules; see applyPublishing.
func normalize(entry *Entry) error {
	kind, ok := NormalizeKind(entry.Kind)
	if !ok {
		return errUnknownKind(entry.Kind)
	}
	entry.Kind = kind
	visibility, ok := NormalizeVisibility(entry.Visibility)
	if !ok {
		return errUnknownVisibility(entry.Visibility)
	}
	entry.Visibility = visibility
	entry.Title = strings.TrimSpace(entry.Title)
	if entry.Title == "" && entry.Kind != KindJournal {
		return fmt.Errorf("%w: title is required for kind %s; only a journal entry may have none", ErrInvalidEntry, entry.Kind)
	}
	if entry.WrittenAt <= 0 {
		return fmt.Errorf("%w: written_at must be epoch seconds, got %d", ErrInvalidEntry, entry.WrittenAt)
	}
	entry.AboutRef = strings.TrimSpace(entry.AboutRef)
	if entry.Kind == KindAbout && entry.AboutRef == "" {
		return fmt.Errorf("%w: kind about needs about_ref, the id of what it is about (person_000012)", ErrInvalidEntry)
	}
	if entry.Kind != KindAbout && entry.Kind != KindTimeline && entry.AboutRef != "" {
		return fmt.Errorf("%w: about_ref is for kinds about and timeline only; this entry is kind %s", ErrInvalidEntry, entry.Kind)
	}
	if strings.ContainsAny(entry.AboutRef, " \t\r\n") {
		return fmt.Errorf("%w: about_ref %q holds whitespace; it must be one id", ErrInvalidEntry, entry.AboutRef)
	}
	tags := []string{}
	seen := map[string]bool{}
	for _, tag := range entry.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return fmt.Errorf("%w: a tag is empty", ErrInvalidEntry)
		}
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	entry.Tags = tags
	entry.Slug = strings.TrimSpace(entry.Slug)
	if entry.Slug != "" && !validSlug(entry.Slug) {
		return fmt.Errorf("%w: slug %q must be lowercase letters and digits joined by single hyphens", ErrInvalidEntry, entry.Slug)
	}
	return nil
}

// applyPublishing enforces the rules of a published entry: it has a slug
// (derived from the title when none was sent), the slug is not held by another
// live published entry, and published_at records the first publication.
func applyPublishing(q queryer, entry *Entry, at int64) error {
	if entry.Visibility != VisibilityPublished || entry.DeletedAt != 0 {
		return nil
	}
	if entry.Slug == "" {
		entry.Slug = DeriveSlug(entry.Title)
		if entry.Slug == "" {
			return fmt.Errorf("%w: a published entry needs a slug; send slug, or a title with a letter or digit to derive one from", ErrInvalidEntry)
		}
	}
	if entry.PublishedAt == 0 {
		entry.PublishedAt = at
	}
	var holder string
	err := q.QueryRow(`SELECT id FROM entries WHERE slug = ? AND visibility = ? AND deleted_at = 0 AND id != ?`,
		entry.Slug, VisibilityPublished, entry.ID).Scan(&holder)
	if err == nil {
		return fmt.Errorf("%w: slug %q is already published by %s; send another slug", ErrConflict, entry.Slug, holder)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func encodeTags(tags []string) string {
	encoded, err := json.Marshal(tags)
	if err != nil {
		// A []string always marshals; reaching here is a programming error.
		panic(err)
	}
	return string(encoded)
}

// insert gives the entry the next id and writes it.
func insert(tx *sql.Tx, entry *Entry) error {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM entries`).Scan(&maxSeq); err != nil {
		return err
	}
	seq := maxSeq.Int64 + 1
	entry.ID = formatID(seq)
	_, err := tx.Exec(`INSERT INTO entries (id, seq, kind, title, body, written_at, visibility, published_at, slug, tags,
		about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		entry.ID, seq, entry.Kind, entry.Title, entry.Body, entry.WrittenAt, entry.Visibility, entry.PublishedAt,
		entry.Slug, encodeTags(entry.Tags), entry.AboutRef, entry.Source, entry.SourceRef, entry.SourceHash,
		entry.CreatedAt, entry.UpdatedAt, entry.DeletedAt)
	return err
}

// update writes every column but id, seq and created_at from entry.
func update(tx *sql.Tx, entry *Entry) error {
	_, err := tx.Exec(`UPDATE entries SET kind=?, title=?, body=?, written_at=?, visibility=?, published_at=?, slug=?,
		tags=?, about_ref=?, source=?, source_ref=?, source_hash=?, updated_at=?, deleted_at=? WHERE id=?`,
		entry.Kind, entry.Title, entry.Body, entry.WrittenAt, entry.Visibility, entry.PublishedAt, entry.Slug,
		encodeTags(entry.Tags), entry.AboutRef, entry.Source, entry.SourceRef, entry.SourceHash, entry.UpdatedAt,
		entry.DeletedAt, entry.ID)
	return err
}

// snapshot copies the whole current row into entry_revisions before a change.
// It runs in the change's transaction, so a change that fails leaves no
// revision and a revision that fails stops the change.
func snapshot(tx *sql.Tx, entryID, reason string, at int64) error {
	result, err := tx.Exec(`INSERT INTO entry_revisions (entry_id, reason, replaced_at, kind, title, body, written_at,
		visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
		SELECT id, ?, ?, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source,
		source_ref, source_hash, created_at, updated_at, deleted_at FROM entries WHERE id = ?`, reason, at, entryID)
	if err != nil {
		return fmt.Errorf("snapshot before %s: %w", reason, err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("snapshot before %s of %s copied %d rows (%v)", reason, entryID, count, err)
	}
	return nil
}

// writeMentions replaces the entry's mentions with the ref: links in body.
func (s *Store) writeMentions(tx *sql.Tx, entryID, body string) error {
	if _, err := tx.Exec(`DELETE FROM mentions WHERE entry_id = ?`, entryID); err != nil {
		return err
	}
	for _, mention := range ExtractMentions(body) {
		if _, err := tx.Exec(`INSERT INTO mentions (entry_id, position, line, entity_ref, entity_type, label) VALUES (?,?,?,?,?,?)`,
			entryID, mention.Position, mention.Line, mention.EntityRef, s.registry.Classify(mention.EntityRef), mention.Label); err != nil {
			return err
		}
	}
	return nil
}

// inTransaction runs work in one transaction under the write lock.
func (s *Store) inTransaction(work func(tx *sql.Tx) error) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	return s.inTransactionLocked(work)
}

func (s *Store) inTransactionLocked(work func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := work(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// createLocked validates, publishes and inserts a new entry, with its mentions.
func (s *Store) createLocked(tx *sql.Tx, entry *Entry, at int64) error {
	if entry.WrittenAt == 0 {
		entry.WrittenAt = at
	}
	if entry.Visibility == "" {
		entry.Visibility = VisibilityPrivate
	}
	entry.CreatedAt, entry.UpdatedAt = at, at
	if err := normalize(entry); err != nil {
		return err
	}
	if err := applyPublishing(tx, entry, at); err != nil {
		return err
	}
	if err := insert(tx, entry); err != nil {
		return err
	}
	return s.writeMentions(tx, entry.ID, entry.Body)
}

// Create records a new entry and hands back its id. written_at is now only
// when the caller sent none.
func (s *Store) Create(input EntryInput) (*Entry, error) {
	entry := &Entry{Kind: input.Kind, Title: input.Title, Body: input.Body, WrittenAt: input.WrittenAt,
		Visibility: input.Visibility, Slug: input.Slug, Tags: input.Tags, AboutRef: input.AboutRef}
	if input.WrittenAt < 0 {
		return nil, fmt.Errorf("%w: written_at must be epoch seconds, got %d", ErrInvalidEntry, input.WrittenAt)
	}
	err := s.inTransaction(func(tx *sql.Tx) error { return s.createLocked(tx, entry, now()) })
	if err != nil {
		return nil, err
	}
	return s.Get(entry.ID, false)
}

// Patch changes the fields the patch names on a live entry.
func (s *Store) Patch(id string, patch EntryPatch) (*Entry, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		entry, err := getLive(tx, id)
		if err != nil {
			return err
		}
		at := now()
		if err := snapshot(tx, id, "update", at); err != nil {
			return err
		}
		if patch.Kind != nil {
			if entry.Kind == KindTimeline && *patch.Kind != KindTimeline {
				var items int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM timeline_items WHERE timeline_id = ? AND deleted_at = 0`, id).Scan(&items); err != nil {
					return err
				}
				if items > 0 {
					return fmt.Errorf("%w: %s is a timeline with %d items; delete them before changing its kind", ErrConflict, id, items)
				}
			}
			entry.Kind = *patch.Kind
		}
		if patch.Title != nil {
			entry.Title = *patch.Title
		}
		if patch.Body != nil {
			entry.Body = *patch.Body
		}
		if patch.WrittenAt != nil {
			entry.WrittenAt = *patch.WrittenAt
		}
		if patch.Visibility != nil {
			entry.Visibility = *patch.Visibility
		}
		if patch.Slug != nil {
			entry.Slug = *patch.Slug
		}
		if patch.Tags != nil {
			entry.Tags = *patch.Tags
		}
		if patch.AboutRef != nil {
			entry.AboutRef = *patch.AboutRef
		}
		entry.UpdatedAt = at
		if err := normalize(entry); err != nil {
			return err
		}
		if err := applyPublishing(tx, entry, at); err != nil {
			return err
		}
		if err := update(tx, entry); err != nil {
			return err
		}
		return s.writeMentions(tx, id, entry.Body)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// Delete stamps deleted_at. Deleting a deleted entry changes nothing, so the
// tombstone keeps the time of the first delete.
func (s *Store) Delete(id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		entry, err := getRow(tx, id)
		if err != nil {
			return err
		}
		if entry.DeletedAt != 0 {
			return nil
		}
		at := now()
		if err := snapshot(tx, id, "delete", at); err != nil {
			return err
		}
		entry.DeletedAt, entry.UpdatedAt = at, at
		return update(tx, entry)
	})
}

// Restore clears deleted_at. The entry comes back as it was deleted; a
// published one whose slug another entry has published since is a conflict.
func (s *Store) Restore(id string) (*Entry, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		entry, err := getRow(tx, id)
		if err != nil {
			return err
		}
		if entry.DeletedAt == 0 {
			return nil
		}
		at := now()
		if err := snapshot(tx, id, "restore", at); err != nil {
			return err
		}
		entry.DeletedAt, entry.UpdatedAt = 0, at
		if err := applyPublishing(tx, entry, at); err != nil {
			return err
		}
		return update(tx, entry)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// ListRevisions returns a page of an entry's prior states, newest first. The
// entry may be deleted: its history is how it is understood.
func (s *Store) ListRevisions(id string, limit, offset int) ([]Revision, error) {
	if _, err := getRow(s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT revision_id, reason, replaced_at, entry_id, kind, title, body, written_at, visibility,
		published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at
		FROM entry_revisions WHERE entry_id = ? ORDER BY revision_id DESC LIMIT ? OFFSET ?`,
		id, pageLimit(limit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		entry, err := scanEntry(func(fields ...any) error {
			return rows.Scan(append([]any{&revision.RevisionID, &revision.Reason, &revision.ReplacedAt}, fields...)...)
		})
		if err != nil {
			return nil, err
		}
		revision.Entry = *entry
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

const (
	defaultPageLimit = 50
	maximumPageLimit = 500
)

func pageLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	return min(limit, maximumPageLimit)
}

// searchExpression turns what a person typed into a safe FTS5 query: each word
// quoted, so punctuation is text and not grammar (noteboard's lesson: a
// hyphen used to be a 500), and each a prefix, so "carol" finds "Carol's".
// Words are ANDed.
func searchExpression(raw string) string {
	words := strings.Fields(raw)
	terms := make([]string, 0, len(words))
	for _, word := range words {
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

// List returns entries matching the filter, newest written first.
func (s *Store) List(filter Filter) ([]Entry, error) {
	where := []string{"1=1"}
	var args []any
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	if filter.Kind != "" {
		kind, ok := NormalizeKind(filter.Kind)
		if !ok {
			return nil, errUnknownKind(filter.Kind)
		}
		where, args = append(where, "kind = ?"), append(args, kind)
	}
	for _, excluded := range filter.ExcludeKinds {
		kind, ok := NormalizeKind(excluded)
		if !ok {
			return nil, errUnknownKind(excluded)
		}
		where, args = append(where, "kind != ?"), append(args, kind)
	}
	if filter.Visibility != "" {
		visibility, ok := NormalizeVisibility(filter.Visibility)
		if !ok {
			return nil, errUnknownVisibility(filter.Visibility)
		}
		where, args = append(where, "visibility = ?"), append(args, visibility)
	}
	if filter.Tag != "" {
		where, args = append(where, "EXISTS (SELECT 1 FROM json_each(entries.tags) WHERE json_each.value = ?)"), append(args, filter.Tag)
	}
	if filter.AboutRef != "" {
		where, args = append(where, "about_ref = ?"), append(args, filter.AboutRef)
	}
	if filter.Source != "" {
		where, args = append(where, "source = ?"), append(args, filter.Source)
	}
	if filter.WrittenFrom > 0 {
		where, args = append(where, "written_at >= ?"), append(args, filter.WrittenFrom)
	}
	if filter.WrittenTo > 0 {
		where, args = append(where, "written_at < ?"), append(args, filter.WrittenTo)
	}
	if expression := searchExpression(filter.Query); expression != "" {
		where, args = append(where, "seq IN (SELECT rowid FROM entries_fts WHERE entries_fts MATCH ?)"), append(args, expression)
	}
	args = append(args, pageLimit(filter.Limit), max(filter.Offset, 0))
	rows, err := s.db.Query(`SELECT `+entryColumns+` FROM entries WHERE `+strings.Join(where, " AND ")+
		` ORDER BY written_at DESC, seq DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// ListPublished returns live published entries, newest published first.
func (s *Store) ListPublished(tag string, limit, offset int) ([]Entry, error) {
	query := `SELECT ` + entryColumns + ` FROM entries WHERE visibility = ? AND deleted_at = 0`
	args := []any{VisibilityPublished}
	if tag != "" {
		query += ` AND EXISTS (SELECT 1 FROM json_each(entries.tags) WHERE json_each.value = ?)`
		args = append(args, tag)
	}
	rows, err := s.db.Query(query+` ORDER BY published_at DESC, seq DESC LIMIT ? OFFSET ?`,
		append(args, pageLimit(limit), max(offset, 0))...)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// GetPublished returns the live published entry holding slug.
func (s *Store) GetPublished(slug string) (*Entry, error) {
	entry, err := scanEntry(s.db.QueryRow(`SELECT `+entryColumns+` FROM entries
		WHERE slug = ? AND visibility = ? AND deleted_at = 0`, slug, VisibilityPublished).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no published entry has slug %q", ErrNotFound, slug)
	}
	return entry, err
}

// Backlinks lists the live entries whose body mentions ref, newest written
// first, each with its mentions of ref. ref is matched exactly.
func (s *Store) Backlinks(ref string, limit, offset int) ([]Backlink, error) {
	rows, err := s.db.Query(`SELECT `+entryColumns+` FROM entries
		WHERE deleted_at = 0 AND (id IN (SELECT entry_id FROM mentions WHERE entity_ref = ?)
			OR id IN (SELECT i.timeline_id FROM timeline_items i JOIN timeline_item_mentions m ON m.item_id = i.id
				WHERE i.deleted_at = 0 AND m.entity_ref = ?))
		ORDER BY written_at DESC, seq DESC LIMIT ? OFFSET ?`, ref, ref, pageLimit(limit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	backlinks := make([]Backlink, 0, len(entries))
	for _, entry := range entries {
		mentions, err := s.mentionsOf(entry.ID, ref)
		if err != nil {
			return nil, err
		}
		backlinks = append(backlinks, Backlink{Entry: entry, Mentions: mentions})
	}
	return backlinks, nil
}

// Import upserts entries by (source, source_ref). A new pair is created. A
// known pair with the same source_hash is skipped. A known pair with another
// hash has its content replaced — kind, title, body, written_at, tags,
// about_ref — after a revision; its visibility, slug and published_at are the
// user's publishing decisions and are never touched by a re-import, and a
// deleted entry stays deleted. The whole request is one transaction: one bad
// entry writes nothing. With dryRun the transaction is rolled back after the
// counts are taken, so the answer says exactly what a real import would do.
func (s *Store) Import(request ImportRequest, dryRun bool) (*ImportResult, error) {
	if len(request.Entries) == 0 {
		return nil, fmt.Errorf("%w: entries is empty", ErrInvalidEntry)
	}
	seen := map[[2]string]bool{}
	for index, item := range request.Entries {
		if strings.TrimSpace(item.Source) == "" || strings.TrimSpace(item.SourceRef) == "" || strings.TrimSpace(item.SourceHash) == "" {
			return nil, fmt.Errorf("%w: entries[%d]: source, source_ref and source_hash are all required", ErrInvalidEntry, index)
		}
		if item.WrittenAt <= 0 {
			return nil, fmt.Errorf("%w: entries[%d] (%s): written_at is required for an import, the date the original was written", ErrInvalidEntry, index, item.SourceRef)
		}
		key := [2]string{item.Source, item.SourceRef}
		if seen[key] {
			return nil, fmt.Errorf("%w: entries[%d]: %s %s appears twice in one import", ErrInvalidEntry, index, item.Source, item.SourceRef)
		}
		seen[key] = true
	}
	result := &ImportResult{Outcomes: []ImportOutcome{}}
	err := s.inTransaction(func(tx *sql.Tx) error {
		at := now()
		for index, item := range request.Entries {
			outcome, entryID, err := s.importOne(tx, item, at)
			if err != nil {
				return fmt.Errorf("entries[%d] (%s %s): %w", index, item.Source, item.SourceRef, err)
			}
			switch outcome {
			case ImportCreated:
				result.Created++
			case ImportUpdated:
				result.Updated++
			case ImportSkipped:
				result.Skipped++
			}
			result.Outcomes = append(result.Outcomes, ImportOutcome{Source: item.Source, SourceRef: item.SourceRef, EntryID: entryID, Outcome: outcome})
		}
		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRunRollback) {
		return nil, err
	}
	return result, nil
}

// errDryRunRollback ends a dry-run import's transaction without committing.
var errDryRunRollback = errors.New("dry run: rolled back")

func (s *Store) importOne(tx *sql.Tx, item ImportEntry, at int64) (string, string, error) {
	var existingID, existingHash string
	err := tx.QueryRow(`SELECT id, source_hash FROM entries WHERE source = ? AND source_ref = ?`, item.Source, item.SourceRef).
		Scan(&existingID, &existingHash)
	if errors.Is(err, sql.ErrNoRows) {
		entry := &Entry{Kind: item.Kind, Title: item.Title, Body: item.Body, WrittenAt: item.WrittenAt,
			Visibility: item.Visibility, Slug: item.Slug, Tags: item.Tags, AboutRef: item.AboutRef,
			Source: item.Source, SourceRef: item.SourceRef, SourceHash: item.SourceHash}
		if err := s.createLocked(tx, entry, at); err != nil {
			return "", "", err
		}
		return ImportCreated, entry.ID, nil
	}
	if err != nil {
		return "", "", err
	}
	if existingHash == item.SourceHash {
		return ImportSkipped, existingID, nil
	}
	entry, err := getRow(tx, existingID)
	if err != nil {
		return "", "", err
	}
	if err := snapshot(tx, existingID, "import", at); err != nil {
		return "", "", err
	}
	entry.Kind, entry.Title, entry.Body, entry.WrittenAt = item.Kind, item.Title, item.Body, item.WrittenAt
	entry.Tags, entry.AboutRef, entry.SourceHash, entry.UpdatedAt = item.Tags, item.AboutRef, item.SourceHash, at
	if err := normalize(entry); err != nil {
		return "", "", err
	}
	if err := applyPublishing(tx, entry, at); err != nil {
		return "", "", err
	}
	if err := update(tx, entry); err != nil {
		return "", "", err
	}
	if err := s.writeMentions(tx, existingID, entry.Body); err != nil {
		return "", "", err
	}
	return ImportUpdated, existingID, nil
}

// PromoteTodo turns checklist line `line` (1-based) of a live entry into a
// noteboard todo, then rewrites the line to link it: `- [ ] [text](ref:<id>)`.
// noteboard is called under the write lock, so the body cannot change
// between reading the line and rewriting it.
func (s *Store) PromoteTodo(id string, line int) (*PromoteTodoResult, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()

	entry, err := getLive(s.db, id)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(entry.Body, "\n")
	if line < 1 || line > len(lines) {
		return nil, fmt.Errorf("%w: line %d is outside the body, which has %d lines", ErrInvalidEntry, line, len(lines))
	}
	item, ok := parseChecklistLine(lines[line-1])
	if !ok {
		return nil, fmt.Errorf("%w: line %d is not a checklist item (- [ ] text): %q", ErrInvalidEntry, line, lines[line-1])
	}
	if item.text == "" {
		return nil, fmt.Errorf("%w: line %d is an empty checklist item", ErrInvalidEntry, line)
	}
	if refLinkPattern.MatchString(item.text) {
		return nil, fmt.Errorf("%w: line %d already links %s", ErrConflict, line, refLinkPattern.FindStringSubmatch(item.text)[2])
	}

	todoID, err := s.noteboard.CreateTodo(item.text,
		fmt.Sprintf("Promoted from journal entry %s, line %d.", id, line), []string{"journal"}, []string{id})
	if err != nil {
		return nil, err
	}
	lines[line-1] = item.linkedLine(todoID)
	entry.Body = strings.Join(lines, "\n")
	err = s.inTransactionLocked(func(tx *sql.Tx) error {
		at := now()
		if err := snapshot(tx, id, "promote-todo", at); err != nil {
			return err
		}
		entry.UpdatedAt = at
		if err := update(tx, entry); err != nil {
			return err
		}
		return s.writeMentions(tx, id, entry.Body)
	})
	if err != nil {
		return nil, fmt.Errorf("noteboard todo %s was created but the entry was not rewritten to link it: %w", todoID, err)
	}
	promoted, err := s.Get(id, false)
	if err != nil {
		return nil, err
	}
	return &PromoteTodoResult{TodoID: todoID, Entry: *promoted}, nil
}

// BackfillMentionTypes types every mention stored with an empty type that the
// registry can now classify. Run after each successful registry read.
func (s *Store) BackfillMentionTypes() (int, error) {
	rows, err := s.db.Query(`SELECT entity_ref FROM mentions WHERE entity_type = ''
		UNION SELECT entity_ref FROM timeline_item_mentions WHERE entity_type = ''`)
	if err != nil {
		return 0, err
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	typed := 0
	err = s.inTransaction(func(tx *sql.Tx) error {
		for _, ref := range refs {
			entityType := s.registry.Classify(ref)
			if entityType == "" {
				continue
			}
			for _, table := range []string{"mentions", "timeline_item_mentions"} {
				result, err := tx.Exec(`UPDATE `+table+` SET entity_type = ? WHERE entity_ref = ? AND entity_type = ''`, entityType, ref)
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				typed += int(count)
			}
		}
		return nil
	})
	return typed, err
}

// Counts reports the table sizes GET /health shows.
func (s *Store) Counts() (Counts, error) {
	var counts Counts
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM entries WHERE deleted_at = 0),
		(SELECT COUNT(*) FROM entries WHERE deleted_at != 0),
		(SELECT COUNT(*) FROM entries WHERE deleted_at = 0 AND visibility = 'published'),
		(SELECT COUNT(*) FROM mentions),
		(SELECT COUNT(*) FROM entry_revisions),
		(SELECT COUNT(*) FROM timeline_items WHERE deleted_at = 0)`).
		Scan(&counts.Entries, &counts.Deleted, &counts.Published, &counts.Mentions, &counts.Revisions, &counts.TimelineItems)
	return counts, err
}
