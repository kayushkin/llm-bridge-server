package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// A timeline is an entry of kind timeline; its dated items live here. Items
// follow the entries' rules: soft delete, a whole-row revision before every
// change, and ref: links (plus sources) recorded as mentions so a person's or
// a message's backlinks find the timelines that cite it. An item, its
// revisions and its mentions take the timeline's owner.

const timelineItemColumns = `seq, owner_principal_id, timeline_seq, occurred_at, ends_at, precision, title, body, sources,
	created_at, updated_at, deleted_at`

// formatTimelineItemID renders an item's number as its public id.
func formatTimelineItemID(seq int64) string {
	return postgresstorage.FormatRecordID(TimelineItemIDPrefix, seq)
}

// timelineItemNumberOf reads an item id's number. A malformed id is not found.
func timelineItemNumberOf(id string) (int64, error) {
	seq, err := postgresstorage.ParseRecordID(TimelineItemIDPrefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: no timeline item %s", ErrNotFound, id)
	}
	return seq, nil
}

func scanTimelineItem(scan func(...any) error) (*TimelineItem, error) {
	var item TimelineItem
	var seq, timelineSeq int64
	var sources string
	if err := scan(&seq, &item.OwnerPrincipalID, &timelineSeq, &item.OccurredAt, &item.EndsAt, &item.Precision, &item.Title,
		&item.Body, &sources, &item.CreatedAt, &item.UpdatedAt, &item.DeletedAt); err != nil {
		return nil, err
	}
	item.ID = formatTimelineItemID(seq)
	item.TimelineID = formatID(timelineSeq)
	if err := json.Unmarshal([]byte(sources), &item.Sources); err != nil {
		return nil, fmt.Errorf("timeline item %s has sources that are not a JSON array: %w", item.ID, err)
	}
	if item.Sources == nil {
		item.Sources = []string{}
	}
	return &item, nil
}

func scanTimelineItems(rows *sql.Rows) ([]TimelineItem, error) {
	defer rows.Close()
	items := []TimelineItem{}
	for rows.Next() {
		item, err := scanTimelineItem(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// getTimelineItemRow reads one item in scope, deleted or not. forUpdate locks
// the row for the rest of the transaction.
func getTimelineItemRow(ctx context.Context, q queryer, scope ownership.Scope, id string, forUpdate bool) (*TimelineItem, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	seq, err := timelineItemNumberOf(id)
	if err != nil {
		return nil, err
	}
	var args arguments
	query := `SELECT ` + timelineItemColumns + ` FROM timeline_items WHERE seq = ` + args.add(seq) +
		` AND ` + args.scope(scope, "owner_principal_id")
	if forUpdate {
		query += ` FOR UPDATE`
	}
	item, err := scanTimelineItem(q.QueryRowContext(ctx, query, args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no timeline item %s", ErrNotFound, id)
	}
	return item, err
}

// liveTimeline reads id, which must be a live entry of kind timeline in scope.
func liveTimeline(ctx context.Context, q queryer, scope ownership.Scope, id string) (*Entry, error) {
	entry, err := getLive(ctx, q, scope, id, false)
	if err != nil {
		return nil, err
	}
	if entry.Kind != KindTimeline {
		return nil, fmt.Errorf("%w: %s is kind %s, not a timeline", ErrInvalidEntry, id, entry.Kind)
	}
	return entry, nil
}

// normalizeTimelineItem checks and tidies an item before any write.
func normalizeTimelineItem(item *TimelineItem) error {
	item.Title = strings.TrimSpace(item.Title)
	if item.Title == "" {
		return fmt.Errorf("%w: a timeline item needs a title", ErrInvalidEntry)
	}
	if item.OccurredAt <= 0 {
		return fmt.Errorf("%w: occurred_at must be epoch seconds, got %d", ErrInvalidEntry, item.OccurredAt)
	}
	if item.EndsAt != 0 && item.EndsAt < item.OccurredAt {
		return fmt.Errorf("%w: ends_at %d is before occurred_at %d", ErrInvalidEntry, item.EndsAt, item.OccurredAt)
	}
	precision, ok := NormalizePrecision(item.Precision)
	if !ok {
		return fmt.Errorf("%w: unknown precision %q: use one of %s", ErrInvalidEntry, item.Precision, strings.Join(TimePrecisions, ", "))
	}
	item.Precision = precision
	sources := []string{}
	seen := map[string]bool{}
	for _, source := range item.Sources {
		source = strings.TrimSpace(source)
		if source == "" || strings.ContainsAny(source, " \t\r\n") {
			return fmt.Errorf("%w: source %q must be one id", ErrInvalidEntry, source)
		}
		if !seen[source] {
			seen[source] = true
			sources = append(sources, source)
		}
	}
	item.Sources = sources
	return nil
}

func insertTimelineItem(ctx context.Context, tx *sql.Tx, item *TimelineItem) error {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO timeline_items (owner_principal_id, timeline_seq, occurred_at, ends_at, precision,
		title, body, sources, created_at, updated_at, deleted_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)
		RETURNING seq`,
		item.OwnerPrincipalID, mustNumber(item.TimelineID), item.OccurredAt, item.EndsAt, item.Precision, item.Title, item.Body,
		encodeTags(item.Sources), item.CreatedAt, item.UpdatedAt, item.DeletedAt).Scan(&seq)
	if err != nil {
		return err
	}
	item.ID = formatTimelineItemID(seq)
	return nil
}

func mustTimelineItemNumber(id string) int64 {
	seq, err := timelineItemNumberOf(id)
	if err != nil {
		panic(fmt.Sprintf("journalstore: the store formatted %q, which does not parse: %v", id, err))
	}
	return seq
}

func updateTimelineItem(ctx context.Context, tx *sql.Tx, item *TimelineItem) error {
	_, err := tx.ExecContext(ctx, `UPDATE timeline_items SET occurred_at = $1, ends_at = $2, precision = $3, title = $4,
		body = $5, sources = $6::jsonb, updated_at = $7, deleted_at = $8 WHERE seq = $9`,
		item.OccurredAt, item.EndsAt, item.Precision, item.Title, item.Body, encodeTags(item.Sources),
		item.UpdatedAt, item.DeletedAt, mustTimelineItemNumber(item.ID))
	return err
}

// snapshotTimelineItem copies the whole current row, owner included, into
// timeline_item_revisions, in the change's own transaction.
func snapshotTimelineItem(ctx context.Context, tx *sql.Tx, itemID, reason string, at int64) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO timeline_item_revisions (owner_principal_id, item_seq, reason, replaced_at,
		timeline_seq, occurred_at, ends_at, precision, title, body, sources, created_at, updated_at, deleted_at)
		SELECT owner_principal_id, seq, $1, $2, timeline_seq, occurred_at, ends_at, precision, title, body, sources,
		created_at, updated_at, deleted_at FROM timeline_items WHERE seq = $3`, reason, at, mustTimelineItemNumber(itemID))
	if err != nil {
		return fmt.Errorf("snapshot before %s: %w", reason, err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("snapshot before %s of %s copied %d rows (%v)", reason, itemID, count, err)
	}
	return nil
}

// writeTimelineItemMentions replaces the item's mentions with the ref: links
// in its body, then its sources. They take the item's owner.
func (s *Store) writeTimelineItemMentions(ctx context.Context, tx *sql.Tx, item *TimelineItem) error {
	seq := mustTimelineItemNumber(item.ID)
	if _, err := tx.ExecContext(ctx, `DELETE FROM timeline_item_mentions WHERE item_seq = $1`, seq); err != nil {
		return err
	}
	position := 0
	write := func(ref, label string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO timeline_item_mentions (owner_principal_id, item_seq, position, entity_ref,
			entity_type, label) VALUES ($1, $2, $3, $4, $5, $6)`,
			item.OwnerPrincipalID, seq, position, ref, s.registry.Classify(ref), label)
		position++
		return err
	}
	for _, mention := range ExtractMentions(item.Body) {
		if err := write(mention.EntityRef, mention.Label); err != nil {
			return err
		}
	}
	for _, source := range item.Sources {
		if err := write(source, ""); err != nil {
			return err
		}
	}
	return nil
}

// CreateTimelineItems adds items to a live timeline in scope, all or none.
// The items take the timeline's owner.
func (s *Store) CreateTimelineItems(ctx context.Context, scope ownership.Scope, timelineID string, inputs []TimelineItemInput) ([]TimelineItem, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: items is empty", ErrInvalidEntry)
	}
	var owner string
	ids := make([]string, 0, len(inputs))
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		// Locked, so the timeline cannot stop being a timeline, or be
		// deleted, while its items are written.
		timeline, err := getLive(ctx, tx, scope, timelineID, true)
		if err != nil {
			return err
		}
		if timeline.Kind != KindTimeline {
			return fmt.Errorf("%w: %s is kind %s, not a timeline", ErrInvalidEntry, timelineID, timeline.Kind)
		}
		owner = timeline.OwnerPrincipalID
		at := now()
		for index, input := range inputs {
			item := &TimelineItem{OwnerPrincipalID: owner, TimelineID: timeline.ID, OccurredAt: input.OccurredAt,
				EndsAt: input.EndsAt, Precision: input.Precision, Title: input.Title, Body: input.Body,
				Sources: input.Sources, CreatedAt: at, UpdatedAt: at}
			if err := normalizeTimelineItem(item); err != nil {
				return fmt.Errorf("item %d: %w", index, err)
			}
			if err := insertTimelineItem(ctx, tx, item); err != nil {
				return err
			}
			if err := s.writeTimelineItemMentions(ctx, tx, item); err != nil {
				return err
			}
			ids = append(ids, item.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	items := make([]TimelineItem, 0, len(ids))
	for _, id := range ids {
		item, err := getTimelineItemRow(ctx, s.db, ownership.OwnedBy(owner), id, false)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, nil
}

// GetTimelineItem reads one item in scope. A deleted item is not found unless
// asked for.
func (s *Store) GetTimelineItem(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (*TimelineItem, error) {
	item, err := getTimelineItemRow(ctx, s.db, scope, id, false)
	if err != nil {
		return nil, err
	}
	if item.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: timeline item %s is deleted", ErrNotFound, id)
	}
	return item, nil
}

// ListTimelineItems is every item on a timeline in scope, oldest first.
func (s *Store) ListTimelineItems(ctx context.Context, scope ownership.Scope, timelineID string, includeDeleted bool) ([]TimelineItem, error) {
	entry, err := s.Get(ctx, scope, timelineID, includeDeleted)
	if err != nil {
		return nil, err
	}
	if entry.Kind != KindTimeline {
		return nil, fmt.Errorf("%w: %s is kind %s, not a timeline", ErrInvalidEntry, timelineID, entry.Kind)
	}
	query := `SELECT ` + timelineItemColumns + ` FROM timeline_items WHERE timeline_seq = $1`
	if !includeDeleted {
		query += ` AND deleted_at = 0`
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY occurred_at, seq`, mustNumber(entry.ID))
	if err != nil {
		return nil, err
	}
	return scanTimelineItems(rows)
}

// OverlayTimelineItems merges the live items of several live timelines in
// scope into one list in time order. A timeline outside the scope is not
// found, as a missing one is. from is inclusive and to exclusive, on
// occurred_at; 0 leaves a side open.
func (s *Store) OverlayTimelineItems(ctx context.Context, scope ownership.Scope, timelineIDs []string, from, to int64, limit, offset int) ([]TimelineItem, error) {
	if len(timelineIDs) == 0 {
		return nil, fmt.Errorf("%w: name at least one timeline", ErrInvalidEntry)
	}
	numbers := make([]int64, 0, len(timelineIDs))
	for _, id := range timelineIDs {
		timeline, err := liveTimeline(ctx, s.db, scope, id)
		if err != nil {
			return nil, err
		}
		numbers = append(numbers, mustNumber(timeline.ID))
	}
	var args arguments
	query := `SELECT ` + timelineItemColumns + ` FROM timeline_items WHERE deleted_at = 0 AND timeline_seq = ANY(` + args.add(numbers) + `)`
	if from > 0 {
		query += ` AND occurred_at >= ` + args.add(from)
	}
	if to > 0 {
		query += ` AND occurred_at < ` + args.add(to)
	}
	query += ` ORDER BY occurred_at, seq LIMIT ` + args.add(overlayLimit(limit)) + ` OFFSET ` + args.add(max(offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	return scanTimelineItems(rows)
}

// The overlay answers more per page than an entry list: a few timelines over
// a year run to hundreds of items.
const (
	defaultOverlayLimit = 500
	maximumOverlayLimit = 2000
)

func overlayLimit(limit int) int {
	if limit <= 0 {
		return defaultOverlayLimit
	}
	return min(limit, maximumOverlayLimit)
}

// PatchTimelineItem changes the fields the patch names on a live item in scope.
func (s *Store) PatchTimelineItem(ctx context.Context, scope ownership.Scope, id string, patch TimelineItemPatch) (*TimelineItem, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		item, err := getTimelineItemRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if item.DeletedAt != 0 {
			return fmt.Errorf("%w: timeline item %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshotTimelineItem(ctx, tx, item.ID, "update", at); err != nil {
			return err
		}
		if patch.OccurredAt != nil {
			item.OccurredAt = *patch.OccurredAt
		}
		if patch.EndsAt != nil {
			item.EndsAt = *patch.EndsAt
		}
		if patch.Precision != nil {
			item.Precision = *patch.Precision
		}
		if patch.Title != nil {
			item.Title = *patch.Title
		}
		if patch.Body != nil {
			item.Body = *patch.Body
		}
		if patch.Sources != nil {
			item.Sources = *patch.Sources
		}
		item.UpdatedAt = at
		if err := normalizeTimelineItem(item); err != nil {
			return err
		}
		if err := updateTimelineItem(ctx, tx, item); err != nil {
			return err
		}
		return s.writeTimelineItemMentions(ctx, tx, item)
	})
	if err != nil {
		return nil, err
	}
	return s.GetTimelineItem(ctx, scope, id, false)
}

// DeleteTimelineItem stamps deleted_at on an item in scope. Deleting a
// deleted item changes nothing.
func (s *Store) DeleteTimelineItem(ctx context.Context, scope ownership.Scope, id string) error {
	return s.inTransaction(ctx, func(tx *sql.Tx) error {
		item, err := getTimelineItemRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if item.DeletedAt != 0 {
			return nil
		}
		at := now()
		if err := snapshotTimelineItem(ctx, tx, item.ID, "delete", at); err != nil {
			return err
		}
		item.DeletedAt, item.UpdatedAt = at, at
		return updateTimelineItem(ctx, tx, item)
	})
}

// RestoreTimelineItem brings a deleted item in scope back as it was.
func (s *Store) RestoreTimelineItem(ctx context.Context, scope ownership.Scope, id string) (*TimelineItem, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		item, err := getTimelineItemRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if item.DeletedAt == 0 {
			return nil
		}
		at := now()
		if err := snapshotTimelineItem(ctx, tx, item.ID, "restore", at); err != nil {
			return err
		}
		item.DeletedAt, item.UpdatedAt = 0, at
		return updateTimelineItem(ctx, tx, item)
	})
	if err != nil {
		return nil, err
	}
	return s.GetTimelineItem(ctx, scope, id, false)
}

// ListTimelineItemRevisions is the history of an item in scope, newest first.
func (s *Store) ListTimelineItemRevisions(ctx context.Context, scope ownership.Scope, id string, limit, offset int) ([]TimelineItemRevision, error) {
	if _, err := getTimelineItemRow(ctx, s.db, scope, id, false); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision_id, reason, replaced_at, item_seq, owner_principal_id, timeline_seq,
		occurred_at, ends_at, precision, title, body, sources, created_at, updated_at, deleted_at
		FROM timeline_item_revisions WHERE item_seq = $1 ORDER BY revision_id DESC LIMIT $2 OFFSET $3`,
		mustTimelineItemNumber(id), pageLimit(limit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []TimelineItemRevision{}
	for rows.Next() {
		var revision TimelineItemRevision
		item, err := scanTimelineItem(func(fields ...any) error {
			return rows.Scan(append([]any{&revision.RevisionID, &revision.Reason, &revision.ReplacedAt}, fields...)...)
		})
		if err != nil {
			return nil, fmt.Errorf("revision of %s: %w", id, err)
		}
		revision.Item = *item
		revision.OwnerPrincipalID = item.OwnerPrincipalID
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}
