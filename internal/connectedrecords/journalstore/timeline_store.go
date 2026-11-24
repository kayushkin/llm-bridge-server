package journalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A timeline is an entry of kind timeline; its dated items live here. Items
// follow the entries' rules: soft delete, a whole-row revision before every
// change, and ref: links (plus sources) recorded as mentions so a person's or
// a message's backlinks find the timelines that cite it.

const timelineItemColumns = `id, timeline_id, occurred_at, ends_at, precision, title, body, sources,
	created_at, updated_at, deleted_at`

// formatTimelineItemID renders a sequence number as an item's public id.
func formatTimelineItemID(seq int64) string { return fmt.Sprintf("timeline_item_%06d", seq) }

func scanTimelineItem(scan func(...any) error) (*TimelineItem, error) {
	var item TimelineItem
	var sources string
	if err := scan(&item.ID, &item.TimelineID, &item.OccurredAt, &item.EndsAt, &item.Precision, &item.Title,
		&item.Body, &sources, &item.CreatedAt, &item.UpdatedAt, &item.DeletedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sources), &item.Sources); err != nil {
		return nil, fmt.Errorf("timeline item %s has sources that are not a JSON array: %w", item.ID, err)
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

func getTimelineItemRow(q queryer, id string) (*TimelineItem, error) {
	item, err := scanTimelineItem(q.QueryRow(`SELECT `+timelineItemColumns+` FROM timeline_items WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no timeline item %s", ErrNotFound, id)
	}
	return item, err
}

// liveTimeline checks that id is a live entry of kind timeline.
func liveTimeline(q queryer, id string) error {
	entry, err := getLive(q, id)
	if err != nil {
		return err
	}
	if entry.Kind != KindTimeline {
		return fmt.Errorf("%w: %s is kind %s, not a timeline", ErrInvalidEntry, id, entry.Kind)
	}
	return nil
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

func insertTimelineItem(tx *sql.Tx, item *TimelineItem) error {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM timeline_items`).Scan(&maxSeq); err != nil {
		return err
	}
	seq := maxSeq.Int64 + 1
	item.ID = formatTimelineItemID(seq)
	_, err := tx.Exec(`INSERT INTO timeline_items (id, seq, timeline_id, occurred_at, ends_at, precision, title, body,
		sources, created_at, updated_at, deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, seq, item.TimelineID, item.OccurredAt, item.EndsAt, item.Precision, item.Title, item.Body,
		encodeTags(item.Sources), item.CreatedAt, item.UpdatedAt, item.DeletedAt)
	return err
}

func updateTimelineItem(tx *sql.Tx, item *TimelineItem) error {
	_, err := tx.Exec(`UPDATE timeline_items SET occurred_at=?, ends_at=?, precision=?, title=?, body=?, sources=?,
		updated_at=?, deleted_at=? WHERE id=?`,
		item.OccurredAt, item.EndsAt, item.Precision, item.Title, item.Body, encodeTags(item.Sources),
		item.UpdatedAt, item.DeletedAt, item.ID)
	return err
}

// snapshotTimelineItem copies the whole current row into
// timeline_item_revisions, in the change's own transaction.
func snapshotTimelineItem(tx *sql.Tx, itemID, reason string, at int64) error {
	result, err := tx.Exec(`INSERT INTO timeline_item_revisions (item_id, reason, replaced_at, timeline_id, occurred_at,
		ends_at, precision, title, body, sources, created_at, updated_at, deleted_at)
		SELECT id, ?, ?, timeline_id, occurred_at, ends_at, precision, title, body, sources, created_at, updated_at,
		deleted_at FROM timeline_items WHERE id = ?`, reason, at, itemID)
	if err != nil {
		return fmt.Errorf("snapshot before %s: %w", reason, err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("snapshot before %s of %s copied %d rows (%v)", reason, itemID, count, err)
	}
	return nil
}

// writeTimelineItemMentions replaces the item's mentions with the ref: links
// in its body, then its sources.
func (s *Store) writeTimelineItemMentions(tx *sql.Tx, item *TimelineItem) error {
	if _, err := tx.Exec(`DELETE FROM timeline_item_mentions WHERE item_id = ?`, item.ID); err != nil {
		return err
	}
	position := 0
	write := func(ref, label string) error {
		_, err := tx.Exec(`INSERT INTO timeline_item_mentions (item_id, position, entity_ref, entity_type, label) VALUES (?,?,?,?,?)`,
			item.ID, position, ref, s.registry.Classify(ref), label)
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

// CreateTimelineItems adds items to a live timeline, all or none.
func (s *Store) CreateTimelineItems(timelineID string, inputs []TimelineItemInput) ([]TimelineItem, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: items is empty", ErrInvalidEntry)
	}
	ids := make([]string, 0, len(inputs))
	err := s.inTransaction(func(tx *sql.Tx) error {
		if err := liveTimeline(tx, timelineID); err != nil {
			return err
		}
		at := now()
		for index, input := range inputs {
			item := &TimelineItem{TimelineID: timelineID, OccurredAt: input.OccurredAt, EndsAt: input.EndsAt,
				Precision: input.Precision, Title: input.Title, Body: input.Body, Sources: input.Sources,
				CreatedAt: at, UpdatedAt: at}
			if err := normalizeTimelineItem(item); err != nil {
				return fmt.Errorf("item %d: %w", index, err)
			}
			if err := insertTimelineItem(tx, item); err != nil {
				return err
			}
			if err := s.writeTimelineItemMentions(tx, item); err != nil {
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
		item, err := getTimelineItemRow(s.db, id)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, nil
}

// GetTimelineItem reads one item. A deleted item is not found unless asked for.
func (s *Store) GetTimelineItem(id string, includeDeleted bool) (*TimelineItem, error) {
	item, err := getTimelineItemRow(s.db, id)
	if err != nil {
		return nil, err
	}
	if item.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: timeline item %s is deleted", ErrNotFound, id)
	}
	return item, nil
}

// ListTimelineItems is every item on a timeline, oldest first.
func (s *Store) ListTimelineItems(timelineID string, includeDeleted bool) ([]TimelineItem, error) {
	entry, err := s.Get(timelineID, includeDeleted)
	if err != nil {
		return nil, err
	}
	if entry.Kind != KindTimeline {
		return nil, fmt.Errorf("%w: %s is kind %s, not a timeline", ErrInvalidEntry, timelineID, entry.Kind)
	}
	query := `SELECT ` + timelineItemColumns + ` FROM timeline_items WHERE timeline_id = ?`
	if !includeDeleted {
		query += ` AND deleted_at = 0`
	}
	rows, err := s.db.Query(query+` ORDER BY occurred_at, seq`, timelineID)
	if err != nil {
		return nil, err
	}
	return scanTimelineItems(rows)
}

// OverlayTimelineItems merges the live items of several live timelines into
// one list in time order. from is inclusive and to exclusive, on occurred_at;
// 0 leaves a side open.
func (s *Store) OverlayTimelineItems(timelineIDs []string, from, to int64, limit, offset int) ([]TimelineItem, error) {
	if len(timelineIDs) == 0 {
		return nil, fmt.Errorf("%w: name at least one timeline", ErrInvalidEntry)
	}
	for _, id := range timelineIDs {
		if err := liveTimeline(s.db, id); err != nil {
			return nil, err
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(timelineIDs)), ",")
	query := `SELECT ` + timelineItemColumns + ` FROM timeline_items WHERE deleted_at = 0 AND timeline_id IN (` + placeholders + `)`
	args := make([]any, 0, len(timelineIDs)+4)
	for _, id := range timelineIDs {
		args = append(args, id)
	}
	if from > 0 {
		query += ` AND occurred_at >= ?`
		args = append(args, from)
	}
	if to > 0 {
		query += ` AND occurred_at < ?`
		args = append(args, to)
	}
	query += ` ORDER BY occurred_at, seq LIMIT ? OFFSET ?`
	args = append(args, overlayLimit(limit), max(offset, 0))
	rows, err := s.db.Query(query, args...)
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

// PatchTimelineItem changes the fields the patch names on a live item.
func (s *Store) PatchTimelineItem(id string, patch TimelineItemPatch) (*TimelineItem, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		item, err := getTimelineItemRow(tx, id)
		if err != nil {
			return err
		}
		if item.DeletedAt != 0 {
			return fmt.Errorf("%w: timeline item %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshotTimelineItem(tx, id, "update", at); err != nil {
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
		if err := updateTimelineItem(tx, item); err != nil {
			return err
		}
		return s.writeTimelineItemMentions(tx, item)
	})
	if err != nil {
		return nil, err
	}
	return s.GetTimelineItem(id, false)
}

// DeleteTimelineItem stamps deleted_at. Deleting a deleted item changes nothing.
func (s *Store) DeleteTimelineItem(id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		item, err := getTimelineItemRow(tx, id)
		if err != nil {
			return err
		}
		if item.DeletedAt != 0 {
			return nil
		}
		at := now()
		if err := snapshotTimelineItem(tx, id, "delete", at); err != nil {
			return err
		}
		item.DeletedAt, item.UpdatedAt = at, at
		return updateTimelineItem(tx, item)
	})
}

// RestoreTimelineItem brings a deleted item back as it was.
func (s *Store) RestoreTimelineItem(id string) (*TimelineItem, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		item, err := getTimelineItemRow(tx, id)
		if err != nil {
			return err
		}
		if item.DeletedAt == 0 {
			return nil
		}
		at := now()
		if err := snapshotTimelineItem(tx, id, "restore", at); err != nil {
			return err
		}
		item.DeletedAt, item.UpdatedAt = 0, at
		return updateTimelineItem(tx, item)
	})
	if err != nil {
		return nil, err
	}
	return s.GetTimelineItem(id, false)
}

// ListTimelineItemRevisions is an item's history, newest first.
func (s *Store) ListTimelineItemRevisions(id string, limit, offset int) ([]TimelineItemRevision, error) {
	if _, err := getTimelineItemRow(s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT revision_id, reason, replaced_at, item_id, timeline_id, occurred_at, ends_at,
		precision, title, body, sources, created_at, updated_at, deleted_at
		FROM timeline_item_revisions WHERE item_id = ? ORDER BY revision_id DESC LIMIT ? OFFSET ?`,
		id, pageLimit(limit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []TimelineItemRevision{}
	for rows.Next() {
		var revision TimelineItemRevision
		var sources string
		item := &revision.Item
		if err := rows.Scan(&revision.RevisionID, &revision.Reason, &revision.ReplacedAt, &item.ID, &item.TimelineID,
			&item.OccurredAt, &item.EndsAt, &item.Precision, &item.Title, &item.Body, &sources, &item.CreatedAt,
			&item.UpdatedAt, &item.DeletedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(sources), &item.Sources); err != nil {
			return nil, fmt.Errorf("revision %d of %s has sources that are not a JSON array: %w", revision.RevisionID, id, err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}
