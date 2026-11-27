package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

const entryColumns = `seq, owner_principal_id, kind, title, body, written_at, visibility, published_at, slug, tags,
	about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at`

func scanEntry(scan func(...any) error) (*Entry, error) {
	var entry Entry
	var seq int64
	var tags string
	err := scan(&seq, &entry.OwnerPrincipalID, &entry.Kind, &entry.Title, &entry.Body, &entry.WrittenAt, &entry.Visibility,
		&entry.PublishedAt, &entry.Slug, &tags, &entry.AboutRef, &entry.Source, &entry.SourceRef,
		&entry.SourceHash, &entry.CreatedAt, &entry.UpdatedAt, &entry.DeletedAt)
	if err != nil {
		return nil, err
	}
	entry.ID = formatID(seq)
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

// getRow reads one entry in scope, deleted or not, without its mentions. An
// entry outside the scope is not found, exactly as a missing one. forUpdate
// locks the row until the transaction ends, so a write reads the state it
// replaces and no other write can change it in between.
func getRow(ctx context.Context, q queryer, scope ownership.Scope, id string, forUpdate bool) (*Entry, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	seq, err := entryNumberOf(id)
	if err != nil {
		return nil, err
	}
	var args arguments
	query := `SELECT ` + entryColumns + ` FROM entries WHERE seq = ` + args.add(seq) + ` AND ` + args.scope(scope, "owner_principal_id")
	if forUpdate {
		query += ` FOR UPDATE`
	}
	entry, err := scanEntry(q.QueryRowContext(ctx, query, args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entry %s", ErrNotFound, id)
	}
	return entry, err
}

// getLive reads one entry in scope that has not been deleted.
func getLive(ctx context.Context, q queryer, scope ownership.Scope, id string, forUpdate bool) (*Entry, error) {
	entry, err := getRow(ctx, q, scope, id, forUpdate)
	if err != nil {
		return nil, err
	}
	if entry.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: entry %s is deleted; POST /entries/%s/restore brings it back", ErrNotFound, id, id)
	}
	return entry, nil
}

// Get returns one entry in scope with its mentions. A deleted entry is a
// not-found unless includeDeleted, so a resolver treats it as an honest miss.
func (s *Store) Get(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (*Entry, error) {
	var entry *Entry
	var err error
	if includeDeleted {
		entry, err = getRow(ctx, s.db, scope, id, false)
	} else {
		entry, err = getLive(ctx, s.db, scope, id, false)
	}
	if err != nil {
		return nil, err
	}
	if entry.Mentions, err = s.mentionsOf(ctx, entry.ID, ""); err != nil {
		return nil, err
	}
	return entry, nil
}

// mentionsOf reads an entry's mentions, which carry the entry's owner, so the
// caller has already found the entry in its scope.
func (s *Store) mentionsOf(ctx context.Context, entryID, ref string) ([]Mention, error) {
	seq, err := entryNumberOf(entryID)
	if err != nil {
		return nil, err
	}
	query := `SELECT position, line, entity_ref, entity_type, label FROM mentions WHERE entry_seq = $1`
	args := []any{seq}
	if ref != "" {
		query += ` AND entity_ref = $2`
		args = append(args, ref)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY position`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mentions := []Mention{}
	for rows.Next() {
		mention := Mention{EntryID: entryID}
		if err := rows.Scan(&mention.Position, &mention.Line, &mention.EntityRef, &mention.EntityType, &mention.Label); err != nil {
			return nil, err
		}
		mentions = append(mentions, mention)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Then the links in the timeline's live items, in time order.
	itemQuery := `SELECT m.item_seq, m.position, m.entity_ref, m.entity_type, m.label
		FROM timeline_item_mentions m JOIN timeline_items i ON i.seq = m.item_seq
		WHERE i.timeline_seq = $1 AND i.deleted_at = 0`
	itemArgs := []any{seq}
	if ref != "" {
		itemQuery += ` AND m.entity_ref = $2`
		itemArgs = append(itemArgs, ref)
	}
	itemRows, err := s.db.QueryContext(ctx, itemQuery+` ORDER BY i.occurred_at, i.seq, m.position`, itemArgs...)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		mention := Mention{EntryID: entryID}
		var itemSeq int64
		if err := itemRows.Scan(&itemSeq, &mention.Position, &mention.EntityRef, &mention.EntityType, &mention.Label); err != nil {
			return nil, err
		}
		mention.ItemID = formatTimelineItemID(itemSeq)
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
// (derived from the title when none was sent), the slug is not held by
// another of its owner's live published entries, and published_at records the
// first publication. Another owner may publish the same slug.
func applyPublishing(ctx context.Context, q queryer, entry *Entry, at int64) error {
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
	// A new entry has no number yet; 0 is no entry's.
	var seq int64
	if entry.ID != "" {
		var err error
		if seq, err = entryNumberOf(entry.ID); err != nil {
			return err
		}
	}
	var holder int64
	err := q.QueryRowContext(ctx, `SELECT seq FROM entries
		WHERE owner_principal_id = $1 AND slug = $2 AND visibility = $3 AND deleted_at = 0 AND seq <> $4`,
		entry.OwnerPrincipalID, entry.Slug, VisibilityPublished, seq).Scan(&holder)
	if err == nil {
		return fmt.Errorf("%w: slug %q is already published by %s; send another slug", ErrConflict, entry.Slug, formatID(holder))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func encodeTags(tags []string) string {
	if tags == nil {
		tags = []string{}
	}
	encoded, err := json.Marshal(tags)
	if err != nil {
		// A []string always marshals; reaching here is a programming error.
		panic(err)
	}
	return string(encoded)
}

// insert writes a new entry and gives it the number the table hands out.
func insert(ctx context.Context, tx *sql.Tx, entry *Entry) error {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO entries (owner_principal_id, kind, title, body, written_at, visibility,
		published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14, $15, $16) RETURNING seq`,
		entry.OwnerPrincipalID, entry.Kind, entry.Title, entry.Body, entry.WrittenAt, entry.Visibility, entry.PublishedAt,
		entry.Slug, encodeTags(entry.Tags), entry.AboutRef, entry.Source, entry.SourceRef, entry.SourceHash,
		entry.CreatedAt, entry.UpdatedAt, entry.DeletedAt).Scan(&seq)
	if err != nil {
		return conflictOf(err)
	}
	entry.ID = formatID(seq)
	return nil
}

// update writes every column but seq, owner and created_at from entry.
func update(ctx context.Context, tx *sql.Tx, entry *Entry) error {
	seq, err := entryNumberOf(entry.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE entries SET kind = $1, title = $2, body = $3, written_at = $4, visibility = $5,
		published_at = $6, slug = $7, tags = $8::jsonb, about_ref = $9, source = $10, source_ref = $11, source_hash = $12,
		updated_at = $13, deleted_at = $14 WHERE seq = $15`,
		entry.Kind, entry.Title, entry.Body, entry.WrittenAt, entry.Visibility, entry.PublishedAt, entry.Slug,
		encodeTags(entry.Tags), entry.AboutRef, entry.Source, entry.SourceRef, entry.SourceHash, entry.UpdatedAt,
		entry.DeletedAt, seq)
	return conflictOf(err)
}

// snapshot copies the whole current row, owner included, into entry_revisions
// before a change. It runs in the change's transaction, so a change that
// fails leaves no revision and a revision that fails stops the change.
func snapshot(ctx context.Context, tx *sql.Tx, entryID, reason string, at int64) error {
	seq, err := entryNumberOf(entryID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO entry_revisions (owner_principal_id, entry_seq, reason, replaced_at,
		kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash,
		created_at, updated_at, deleted_at)
		SELECT owner_principal_id, seq, $1, $2, kind, title, body, written_at, visibility, published_at, slug, tags,
		about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at FROM entries WHERE seq = $3`,
		reason, at, seq)
	if err != nil {
		return fmt.Errorf("snapshot before %s: %w", reason, err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("snapshot before %s of %s copied %d rows (%v)", reason, entryID, count, err)
	}
	return nil
}

// writeMentions replaces the entry's mentions with the ref: links in its
// body. They take the entry's owner.
func (s *Store) writeMentions(ctx context.Context, tx *sql.Tx, entry *Entry) error {
	seq, err := entryNumberOf(entry.ID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mentions WHERE entry_seq = $1`, seq); err != nil {
		return err
	}
	for _, mention := range ExtractMentions(entry.Body) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mentions (owner_principal_id, entry_seq, position, line, entity_ref, entity_type, label)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			entry.OwnerPrincipalID, seq, mention.Position, mention.Line, mention.EntityRef, s.registry.Classify(mention.EntityRef), mention.Label); err != nil {
			return err
		}
	}
	return nil
}

// createInTransaction validates, publishes and inserts a new entry, with its
// mentions. entry.OwnerPrincipalID is already set.
func (s *Store) createInTransaction(ctx context.Context, tx *sql.Tx, entry *Entry, at int64) error {
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
	if err := applyPublishing(ctx, tx, entry, at); err != nil {
		return err
	}
	if err := insert(ctx, tx, entry); err != nil {
		return err
	}
	return s.writeMentions(ctx, tx, entry)
}

// Create records a new entry owned by ownerPrincipalID and hands back its id.
// written_at is now only when the caller sent none.
func (s *Store) Create(ctx context.Context, ownerPrincipalID string, input EntryInput) (*Entry, error) {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return nil, fmt.Errorf("journalstore: %w", err)
	}
	entry := &Entry{OwnerPrincipalID: ownerPrincipalID, Kind: input.Kind, Title: input.Title, Body: input.Body,
		WrittenAt: input.WrittenAt, Visibility: input.Visibility, Slug: input.Slug, Tags: input.Tags, AboutRef: input.AboutRef}
	if input.WrittenAt < 0 {
		return nil, fmt.Errorf("%w: written_at must be epoch seconds, got %d", ErrInvalidEntry, input.WrittenAt)
	}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error { return s.createInTransaction(ctx, tx, entry, now()) })
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, ownership.OwnedBy(ownerPrincipalID), entry.ID, false)
}

// Patch changes the fields the patch names on a live entry in scope.
func (s *Store) Patch(ctx context.Context, scope ownership.Scope, id string, patch EntryPatch) (*Entry, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		entry, err := getLive(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		at := now()
		if err := snapshot(ctx, tx, entry.ID, "update", at); err != nil {
			return err
		}
		if patch.Kind != nil {
			if entry.Kind == KindTimeline && *patch.Kind != KindTimeline {
				var items int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM timeline_items WHERE timeline_seq = $1 AND deleted_at = 0`,
					mustNumber(entry.ID)).Scan(&items); err != nil {
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
		if err := applyPublishing(ctx, tx, entry, at); err != nil {
			return err
		}
		if err := update(ctx, tx, entry); err != nil {
			return err
		}
		return s.writeMentions(ctx, tx, entry)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, scope, id, false)
}

// mustNumber is the number of an id the store itself formatted.
func mustNumber(id string) int64 {
	seq, err := entryNumberOf(id)
	if err != nil {
		panic(fmt.Sprintf("journalstore: the store formatted %q, which does not parse: %v", id, err))
	}
	return seq
}

// Delete stamps deleted_at on an entry in scope. Deleting a deleted entry
// changes nothing, so the tombstone keeps the time of the first delete.
func (s *Store) Delete(ctx context.Context, scope ownership.Scope, id string) error {
	return s.inTransaction(ctx, func(tx *sql.Tx) error {
		entry, err := getRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if entry.DeletedAt != 0 {
			return nil
		}
		at := now()
		if err := snapshot(ctx, tx, entry.ID, "delete", at); err != nil {
			return err
		}
		entry.DeletedAt, entry.UpdatedAt = at, at
		return update(ctx, tx, entry)
	})
}

// Restore clears deleted_at on an entry in scope. The entry comes back as it
// was deleted; a published one whose slug another of its owner's entries has
// published since is a conflict.
func (s *Store) Restore(ctx context.Context, scope ownership.Scope, id string) (*Entry, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		entry, err := getRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if entry.DeletedAt == 0 {
			return nil
		}
		at := now()
		if err := snapshot(ctx, tx, entry.ID, "restore", at); err != nil {
			return err
		}
		entry.DeletedAt, entry.UpdatedAt = 0, at
		if err := applyPublishing(ctx, tx, entry, at); err != nil {
			return err
		}
		return update(ctx, tx, entry)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, scope, id, false)
}

// ListRevisions returns a page of an entry's prior states, newest first. The
// entry may be deleted: its history is how it is understood.
func (s *Store) ListRevisions(ctx context.Context, scope ownership.Scope, id string, limit, offset int) ([]Revision, error) {
	entry, err := getRow(ctx, s.db, scope, id, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision_id, reason, replaced_at, entry_seq, owner_principal_id, kind, title,
		body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at,
		updated_at, deleted_at
		FROM entry_revisions WHERE entry_seq = $1 ORDER BY revision_id DESC LIMIT $2 OFFSET $3`,
		mustNumber(entry.ID), pageLimit(limit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		revised, err := scanEntry(func(fields ...any) error {
			return rows.Scan(append([]any{&revision.RevisionID, &revision.Reason, &revision.ReplacedAt}, fields...)...)
		})
		if err != nil {
			return nil, err
		}
		revision.Entry = *revised
		revision.OwnerPrincipalID = revised.OwnerPrincipalID
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

// List returns entries in scope matching the filter, newest written first.
func (s *Store) List(ctx context.Context, scope ownership.Scope, filter Filter) ([]Entry, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := []string{args.scope(scope, "owner_principal_id")}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	if filter.Kind != "" {
		kind, ok := NormalizeKind(filter.Kind)
		if !ok {
			return nil, errUnknownKind(filter.Kind)
		}
		where = append(where, "kind = "+args.add(kind))
	}
	for _, excluded := range filter.ExcludeKinds {
		kind, ok := NormalizeKind(excluded)
		if !ok {
			return nil, errUnknownKind(excluded)
		}
		where = append(where, "kind <> "+args.add(kind))
	}
	if filter.Visibility != "" {
		visibility, ok := NormalizeVisibility(filter.Visibility)
		if !ok {
			return nil, errUnknownVisibility(filter.Visibility)
		}
		where = append(where, "visibility = "+args.add(visibility))
	}
	if filter.Tag != "" {
		where = append(where, "tags @> jsonb_build_array("+args.add(filter.Tag)+"::text)")
	}
	if filter.AboutRef != "" {
		where = append(where, "about_ref = "+args.add(filter.AboutRef))
	}
	if filter.Source != "" {
		where = append(where, "source = "+args.add(filter.Source))
	}
	if filter.WrittenFrom > 0 {
		where = append(where, "written_at >= "+args.add(filter.WrittenFrom))
	}
	if filter.WrittenTo > 0 {
		where = append(where, "written_at < "+args.add(filter.WrittenTo))
	}
	expression, searchable := searchQueryOf(filter.Query)
	if !searchable {
		return []Entry{}, nil
	}
	if expression != "" {
		where = append(where, "search_document @@ to_tsquery('simple', "+args.add(expression)+")")
	}
	query := `SELECT ` + entryColumns + ` FROM entries WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY written_at DESC, seq DESC LIMIT ` + args.add(pageLimit(filter.Limit)) + ` OFFSET ` + args.add(max(filter.Offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// ListPublished returns live published entries in scope, newest published
// first.
func (s *Store) ListPublished(ctx context.Context, scope ownership.Scope, tag string, limit, offset int) ([]Entry, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	query := `SELECT ` + entryColumns + ` FROM entries WHERE ` + args.scope(scope, "owner_principal_id") +
		` AND visibility = ` + args.add(VisibilityPublished) + ` AND deleted_at = 0`
	if tag != "" {
		query += ` AND tags @> jsonb_build_array(` + args.add(tag) + `::text)`
	}
	query += ` ORDER BY published_at DESC, seq DESC LIMIT ` + args.add(pageLimit(limit)) + ` OFFSET ` + args.add(max(offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// GetPublished returns the live published entry in scope holding slug. A slug
// is unique among one owner's entries only, so a scope of every owner that
// finds it published by two is a 400 asking which owner.
func (s *Store) GetPublished(ctx context.Context, scope ownership.Scope, slug string) (*Entry, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	query := `SELECT ` + entryColumns + ` FROM entries WHERE ` + args.scope(scope, "owner_principal_id") +
		` AND slug = ` + args.add(slug) + ` AND visibility = ` + args.add(VisibilityPublished) + ` AND deleted_at = 0
		ORDER BY seq LIMIT 2`
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	switch len(entries) {
	case 0:
		return nil, fmt.Errorf("%w: no published entry has slug %q", ErrNotFound, slug)
	case 1:
		return &entries[0], nil
	}
	return nil, fmt.Errorf("%w: slug %q is published by more than one owner; name one with ?%s=", ErrInvalidEntry, slug, ownership.OwnerParameter)
}

// Backlinks lists the live entries in scope whose body, or one of whose live
// timeline items, mentions ref, newest written first, each with its mentions
// of ref. ref is matched exactly.
func (s *Store) Backlinks(ctx context.Context, scope ownership.Scope, ref string, limit, offset int) ([]Backlink, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	ownerCondition := args.scope(scope, "owner_principal_id")
	refPlaceholder := args.add(ref)
	query := `SELECT ` + entryColumns + ` FROM entries
		WHERE ` + ownerCondition + ` AND deleted_at = 0 AND (seq IN (SELECT entry_seq FROM mentions WHERE entity_ref = ` + refPlaceholder + `)
			OR seq IN (SELECT i.timeline_seq FROM timeline_items i JOIN timeline_item_mentions m ON m.item_seq = i.seq
				WHERE i.deleted_at = 0 AND m.entity_ref = ` + refPlaceholder + `))
		ORDER BY written_at DESC, seq DESC LIMIT ` + args.add(pageLimit(limit)) + ` OFFSET ` + args.add(max(offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	backlinks := make([]Backlink, 0, len(entries))
	for _, entry := range entries {
		mentions, err := s.mentionsOf(ctx, entry.ID, ref)
		if err != nil {
			return nil, err
		}
		backlinks = append(backlinks, Backlink{Entry: entry, Mentions: mentions})
	}
	return backlinks, nil
}

// Import upserts entries owned by ownerPrincipalID by (source, source_ref),
// among that owner's entries only. A new pair is created. A known pair with
// the same source_hash is skipped. A known pair with another hash has its
// content replaced — kind, title, body, written_at, tags, about_ref — after a
// revision; its visibility, slug and published_at are the user's publishing
// decisions and are never touched by a re-import, and a deleted entry stays
// deleted. The whole request is one transaction: one bad entry writes nothing.
// With dryRun the transaction is rolled back after the counts are taken, so
// the answer says exactly what a real import would do.
func (s *Store) Import(ctx context.Context, ownerPrincipalID string, request ImportRequest, dryRun bool) (*ImportResult, error) {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return nil, fmt.Errorf("journalstore: %w", err)
	}
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
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		at := now()
		for index, item := range request.Entries {
			outcome, entryID, err := s.importOne(ctx, tx, ownerPrincipalID, item, at)
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

func (s *Store) importOne(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, item ImportEntry, at int64) (string, string, error) {
	var existingSeq int64
	var existingHash string
	err := tx.QueryRowContext(ctx, `SELECT seq, source_hash FROM entries
		WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3 FOR UPDATE`,
		ownerPrincipalID, item.Source, item.SourceRef).Scan(&existingSeq, &existingHash)
	if errors.Is(err, sql.ErrNoRows) {
		entry := &Entry{OwnerPrincipalID: ownerPrincipalID, Kind: item.Kind, Title: item.Title, Body: item.Body,
			WrittenAt: item.WrittenAt, Visibility: item.Visibility, Slug: item.Slug, Tags: item.Tags, AboutRef: item.AboutRef,
			Source: item.Source, SourceRef: item.SourceRef, SourceHash: item.SourceHash}
		if err := s.createInTransaction(ctx, tx, entry, at); err != nil {
			return "", "", err
		}
		return ImportCreated, entry.ID, nil
	}
	if err != nil {
		return "", "", err
	}
	existingID := formatID(existingSeq)
	if existingHash == item.SourceHash {
		return ImportSkipped, existingID, nil
	}
	entry, err := getRow(ctx, tx, ownership.OwnedBy(ownerPrincipalID), existingID, true)
	if err != nil {
		return "", "", err
	}
	if err := snapshot(ctx, tx, existingID, "import", at); err != nil {
		return "", "", err
	}
	entry.Kind, entry.Title, entry.Body, entry.WrittenAt = item.Kind, item.Title, item.Body, item.WrittenAt
	entry.Tags, entry.AboutRef, entry.SourceHash, entry.UpdatedAt = item.Tags, item.AboutRef, item.SourceHash, at
	if err := normalize(entry); err != nil {
		return "", "", err
	}
	if err := applyPublishing(ctx, tx, entry, at); err != nil {
		return "", "", err
	}
	if err := update(ctx, tx, entry); err != nil {
		return "", "", err
	}
	if err := s.writeMentions(ctx, tx, entry); err != nil {
		return "", "", err
	}
	return ImportUpdated, existingID, nil
}

// PromoteTodo turns checklist line `line` (1-based) of a live entry into a
// noteboard todo, then rewrites the line to link it: `- [ ] [text](ref:<id>)`.
//
// noteboard has no owner on a row and every agent on the host reads it, so a
// line of private writing may go there only on its owner's own say: caller
// must see every owner (an administrator) and be the entry's owner. Anyone
// else is ErrForbidden; an entry outside scope is not found, as always.
//
// The entry's row is locked (SELECT … FOR UPDATE) from reading the line until
// it is rewritten, noteboard call included, so the body cannot change in
// between.
func (s *Store) PromoteTodo(ctx context.Context, caller ownership.Caller, scope ownership.Scope, id string, line int) (*PromoteTodoResult, error) {
	var todoID string
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		entry, err := getLive(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if !caller.SeesEveryOwner {
			return fmt.Errorf("%w: promote-todo copies a line of the entry into noteboard, which every agent on the host reads and which keeps no owner, so only an administrator may promote a line, and only of their own entry", ErrForbidden)
		}
		if caller.PrincipalID != entry.OwnerPrincipalID {
			return fmt.Errorf("%w: %s belongs to %s; promote-todo copies a line into noteboard, which every agent on the host reads, so only the entry's owner may promote one of its lines", ErrForbidden, id, entry.OwnerPrincipalID)
		}
		lines := strings.Split(entry.Body, "\n")
		if line < 1 || line > len(lines) {
			return fmt.Errorf("%w: line %d is outside the body, which has %d lines", ErrInvalidEntry, line, len(lines))
		}
		item, ok := parseChecklistLine(lines[line-1])
		if !ok {
			return fmt.Errorf("%w: line %d is not a checklist item (- [ ] text): %q", ErrInvalidEntry, line, lines[line-1])
		}
		if item.text == "" {
			return fmt.Errorf("%w: line %d is an empty checklist item", ErrInvalidEntry, line)
		}
		if refLinkPattern.MatchString(item.text) {
			return fmt.Errorf("%w: line %d already links %s", ErrConflict, line, refLinkPattern.FindStringSubmatch(item.text)[2])
		}

		created, err := s.noteboard.CreateTodo(ctx, item.text,
			fmt.Sprintf("Promoted from journal entry %s, line %d.", id, line), []string{"journal"}, []string{id})
		if err != nil {
			return err
		}
		todoID = created
		lines[line-1] = item.linkedLine(todoID)
		entry.Body = strings.Join(lines, "\n")
		at := now()
		if err := snapshot(ctx, tx, entry.ID, "promote-todo", at); err != nil {
			return err
		}
		entry.UpdatedAt = at
		if err := update(ctx, tx, entry); err != nil {
			return err
		}
		return s.writeMentions(ctx, tx, entry)
	})
	if err != nil {
		if todoID != "" {
			return nil, fmt.Errorf("noteboard todo %s was created but the entry was not rewritten to link it: %w", todoID, err)
		}
		return nil, err
	}
	promoted, err := s.Get(ctx, scope, id, false)
	if err != nil {
		return nil, err
	}
	return &PromoteTodoResult{TodoID: todoID, Entry: *promoted}, nil
}

// BackfillMentionTypes types every mention, of every owner, stored with an
// empty type that the registry can now classify. Run after each successful
// registry read: it is the server's own housekeeping, so it answers to no
// caller's scope.
func (s *Store) BackfillMentionTypes(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT entity_ref FROM mentions WHERE entity_type = ''
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
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		for _, ref := range refs {
			entityType := s.registry.Classify(ref)
			if entityType == "" {
				continue
			}
			for _, table := range []string{"mentions", "timeline_item_mentions"} {
				result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET entity_type = $1 WHERE entity_ref = $2 AND entity_type = ''`, entityType, ref)
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
	if err != nil {
		return 0, err
	}
	return typed, nil
}

// Counts reports the table sizes GET /health shows, among the records in
// scope.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (Counts, error) {
	if err := checkScope(scope); err != nil {
		return Counts{}, err
	}
	var args arguments
	owner := args.scope(scope, "owner_principal_id")
	var counts Counts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM entries WHERE `+owner+` AND deleted_at = 0),
		(SELECT COUNT(*) FROM entries WHERE `+owner+` AND deleted_at <> 0),
		(SELECT COUNT(*) FROM entries WHERE `+owner+` AND deleted_at = 0 AND visibility = 'published'),
		(SELECT COUNT(*) FROM mentions WHERE `+owner+`),
		(SELECT COUNT(*) FROM entry_revisions WHERE `+owner+`),
		(SELECT COUNT(*) FROM timeline_items WHERE `+owner+` AND deleted_at = 0)`, args.values...).
		Scan(&counts.Entries, &counts.Deleted, &counts.Published, &counts.Mentions, &counts.Revisions, &counts.TimelineItems)
	return counts, err
}
