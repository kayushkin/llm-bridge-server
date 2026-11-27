// Package items is the record store two modules share: notes keeps notes,
// ranked list entries and workspaces with it, and issues keeps its issues
// (todos) with it. Each module has its own Postgres schema holding an items
// table and an item_revisions table of the same shape, and hands its pool to
// New with a Holding that says which item types it keeps. The server
// provisions, migrates and opens each module; this package keeps no schema of
// its own.
package items

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// Holding says which item types one store keeps, and where the others are.
type Holding struct {
	// Name is the module's name, for messages: "notes" or "issues".
	Name string
	// Types are the item types the store keeps.
	Types []string
	// DefaultType is the type an item made without one gets. Empty means the
	// caller must name one.
	DefaultType string
	// TypesKeptElsewhere says, for each type another module keeps, where it
	// is kept. A request naming one is refused with that text, so a caller
	// that still writes a todo to notes learns where todos went.
	TypesKeptElsewhere map[string]string
	// ListMembershipKeptElsewhere, when not empty, refuses a list_id on a
	// write and says where ranked lists are kept. Empty keeps list_id.
	ListMembershipKeptElsewhere string
}

// Keeps reports whether the store keeps items of itemType.
func (holding Holding) Keeps(itemType string) bool {
	return slices.Contains(holding.Types, itemType)
}

// ErrTypeNotKept refuses a request about an item type this store does not
// keep: a todo sent to notes, a note sent to issues.
var ErrTypeNotKept = errors.New("this store does not keep items of that type")

// ErrListMembershipNotKept refuses a list_id on a store whose items do not
// sit in ranked lists.
var ErrListMembershipNotKept = errors.New("items here do not sit in ranked lists")

// checkType refuses itemType unless the store keeps it.
func (holding Holding) checkType(itemType string) error {
	if holding.Keeps(itemType) {
		return nil
	}
	if where, elsewhere := holding.TypesKeptElsewhere[itemType]; elsewhere {
		return fmt.Errorf("%w: %s keeps no %s items; %s", ErrTypeNotKept, holding.Name, itemType, where)
	}
	return fmt.Errorf("%w: %s keeps items of type %s, not %q", ErrTypeNotKept, holding.Name, strings.Join(holding.Types, ", "), itemType)
}

// checkListID refuses a list_id the store does not keep. An empty one, which
// takes an item out of every list, is always allowed.
func (holding Holding) checkListID(listID *string) error {
	if listID == nil || *listID == "" || holding.ListMembershipKeptElsewhere == "" {
		return nil
	}
	return fmt.Errorf("%w: %s keeps no list_id; %s", ErrListMembershipNotKept, holding.Name, holding.ListMembershipKeptElsewhere)
}

// Store reads and writes items through one module's connection pool.
type Store struct {
	db      *sql.DB
	holding Holding
}

// New returns the store over db, the pool postgresstorage.Open gave a module
// whose schema holds the items and item_revisions tables, keeping the item
// types holding names. The schema must already be migrated.
func New(db *sql.DB, holding Holding) *Store {
	if len(holding.Types) == 0 || holding.Name == "" {
		panic("items: a store needs a Holding with a name and at least one type")
	}
	if holding.DefaultType != "" && !holding.Keeps(holding.DefaultType) {
		panic("items: the default type " + holding.DefaultType + " is not one the store keeps")
	}
	return &Store{db: db, holding: holding}
}

// Holding is what the store keeps.
func (s *Store) Holding() Holding { return s.holding }

// databaseHandle is what a statement runs on: the pool, or a transaction.
type databaseHandle interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// storedNow is the time a write records. timestamptz keeps microseconds, so
// the time is cut to them here: the item a write answers with then carries the
// same updated_at a later read does, and an If-Match made from either matches.
func storedNow() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// queryArguments numbers the placeholders of a statement built piece by piece.
type queryArguments struct {
	values []any
}

// add appends value and returns its placeholder.
func (arguments *queryArguments) add(value any) string {
	arguments.values = append(arguments.values, value)
	return "$" + strconv.Itoa(len(arguments.values))
}

// ErrItemNotFound reports that an id names no row. Every read of a single item
// goes through scanItem, so this is the one place the condition is born and the
// one error every caller has to recognize — the driver's own sql.ErrNoRows says
// "a query returned nothing", which is true of a great many failures that are
// not the caller naming a row that isn't there.
//
// It exists so a handler can tell the caller's mistake from the store's failure.
// Without it the only signal is sql.ErrNoRows, and a handler that wants to
// answer 404 for a missing item has to either match on the driver's error or
// collapse every error to 404 — the second reports "not found" for an item that
// is plainly there, which is the same hole ErrUnknownParent was added to close.
var ErrItemNotFound = fmt.Errorf("no such item")

// ErrTextHoldsNulCharacter refuses text holding U+0000. Postgres text and
// jsonb cannot keep that character, so the store says so instead of failing
// in the database; the SQLite store before it kept it.
var ErrTextHoldsNulCharacter = errors.New("text holds a NUL character (U+0000), which this store cannot keep")

// refuseNulCharacters returns ErrTextHoldsNulCharacter naming the first field
// whose text holds U+0000.
func refuseNulCharacters(fields map[string][]string) error {
	for name, values := range fields {
		for _, value := range values {
			if strings.ContainsRune(value, 0) {
				return fmt.Errorf("%w: %s", ErrTextHoldsNulCharacter, name)
			}
		}
	}
	return nil
}

func optionalText(value *string) []string {
	if value == nil {
		return nil
	}
	return []string{*value}
}

// scheduleText is the schedule's text for the NUL check. JSON writes U+0000
// as the escape \u0000, which jsonb refuses as it refuses the character, so
// the escape is read back as the character here.
func scheduleText(schedule any) []string {
	if text, ok := schedule.(string); ok {
		return []string{strings.ReplaceAll(text, `\u0000`, "\x00")}
	}
	return nil
}

// ItemChangedError refuses an update whose caller expected an older version of
// the item than the one stored: someone else wrote in between, and applying
// the update would overwrite their change unseen. Current is the item as it is
// now, so the caller can show what changed and decide again.
type ItemChangedError struct {
	Current *model.Item
}

func (e *ItemChangedError) Error() string {
	return fmt.Sprintf("item %s was changed at %s, after the version this update was made against; nothing was written",
		e.Current.ID, e.Current.UpdatedAt.Format(time.RFC3339Nano))
}

// zoneOfOffset is the zone a stored due date is given back in: UTC for an
// offset of zero, so it reads as Z as it was written, and otherwise the offset
// itself, so 12:00-07:00 reads back as 12:00-07:00.
func zoneOfOffset(offsetSeconds int32) *time.Location {
	if offsetSeconds == 0 {
		return time.UTC
	}
	return time.FixedZone("", int(offsetSeconds))
}

// dueAtColumns is a due date as its two columns: the instant, to the second as
// the store always kept it, and the offset its writer sent. A nil due date is
// two NULLs.
func dueAtColumns(dueAt *time.Time) (any, any) {
	if dueAt == nil {
		return nil, nil
	}
	_, offsetSeconds := dueAt.Zone()
	return dueAt.Truncate(time.Second), offsetSeconds
}

func timePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	utc := value.Time.UTC()
	return &utc
}

func unmarshalStringList(raw []byte, column, itemID string) ([]string, error) {
	list := []string{}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("item %s has unreadable %s %s: %w", itemID, column, raw, err)
	}
	if list == nil {
		list = []string{}
	}
	return list, nil
}

func marshalStringList(list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func scanItem(row interface{ Scan(...any) error }) (*model.Item, error) {
	var item model.Item
	var tagsJSON, linksJSON []byte
	var dueAt sql.NullTime
	var dueAtOffsetSeconds sql.NullInt32
	var parentID sql.NullString
	var createdAt, updatedAt time.Time
	var deletedAt, heldAt sql.NullTime
	var scheduleJSON sql.NullString
	// NullFloat64, not float64: NULL means "no ceiling" and 0 means "hold before
	// spending a cent". Scanning into a plain float64 would collapse the two.
	var autoHoldAtUSD sql.NullFloat64

	err := row.Scan(
		&item.ID, &item.Type, &item.Title, &item.Body,
		&tagsJSON, &item.Priority, &item.Rank, &item.Status,
		&item.ListID, &dueAt, &dueAtOffsetSeconds, &parentID, &linksJSON,
		&item.CreatedBy, &createdAt, &updatedAt, &deletedAt,
		&scheduleJSON, &heldAt, &item.HoldReason, &autoHoldAtUSD,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, err
	}
	item.CreatedAt = createdAt.UTC()
	item.UpdatedAt = updatedAt.UTC()
	item.DeletedAt = timePointer(deletedAt)
	item.HeldAt = timePointer(heldAt)
	if autoHoldAtUSD.Valid {
		value := autoHoldAtUSD.Float64
		item.AutoHoldAtUSD = &value
	}
	if scheduleJSON.Valid {
		// A rule that fails to parse means the coordinator would expand it to
		// nothing and silently remind nobody, forever. That failure is invisible
		// by construction, so it has to be loud here.
		var schedule model.Schedule
		if err := json.Unmarshal([]byte(scheduleJSON.String), &schedule); err != nil {
			return nil, fmt.Errorf("item %s has an unreadable schedule: %w", item.ID, err)
		}
		item.Schedule = &schedule
	}
	if item.Tags, err = unmarshalStringList(tagsJSON, "tags", item.ID); err != nil {
		return nil, err
	}
	if item.Links, err = unmarshalStringList(linksJSON, "links", item.ID); err != nil {
		return nil, err
	}
	if dueAt.Valid {
		// Given back in the offset its writer sent, not normalized to UTC: the
		// offset is part of the value a caller gave us, and JSON renders it back.
		due := dueAt.Time.In(zoneOfOffset(dueAtOffsetSeconds.Int32))
		item.DueAt = &due
	}
	if parentID.Valid {
		item.ParentID = &parentID.String
	}
	return &item, nil
}

// itemCols is the READ projection and includes deleted_at. insertCols is the
// write set for a new row and deliberately does not: an item is never born
// deleted.
const itemCols = "id, type, title, body, tags, priority, rank, status, list_id, due_at, due_at_offset_seconds, parent_id, links, created_by, created_at, updated_at, deleted_at, schedule, held_at, hold_reason, auto_hold_at_usd"
const insertCols = "id, type, title, body, tags, priority, rank, status, list_id, due_at, due_at_offset_seconds, parent_id, links, created_by, created_at, updated_at, schedule, held_at, hold_reason, auto_hold_at_usd"

// notDeleted is the standing filter on every read path. A soft delete that any
// list still returns is not a delete.
const notDeleted = "items.deleted_at IS NULL"

// heldSubtreeCTE names every item that is held, INCLUDING BY INHERITANCE: an item
// whose parent (or grandparent, …) is held is itself withheld.
//
// A hold that stops the parent but not its children does not stop the work. The
// autoworker's own prompt tells a worker that a too-large todo must be SPLIT into
// child todos, so "park this task" has to mean "park the tree", or parking a task
// is escapable by the very decomposition we asked for. The same holds for the
// spend ceiling built on top of this: a $5 cap a child can walk around is not a
// cap.
//
// The hold itself is stored on exactly one row and inherited at READ time. The
// alternative — cascading a write down to every descendant — makes a second copy
// of the truth, and then unhold has to guess which descendants were held on their
// own account and which only inherited it.
//
// UNION (not UNION ALL) dedupes, so a parent_id cycle terminates instead of
// spinning forever. Deleted rows cannot seed the set — a tombstone must not park
// its live children.
const heldSubtreeCTE = `WITH RECURSIVE held_subtree(id) AS (
		SELECT id FROM items WHERE held_at IS NOT NULL AND deleted_at IS NULL
		UNION
		SELECT i.id FROM items i JOIN held_subtree h ON i.parent_id = h.id
	)`

// ErrUnknownParent is returned for a parent_id that names no live item, or one
// that would put the item inside its own ancestry.
//
// This is not tidiness. The hold gate and the spend ceiling both roll up over
// parent_id, and heldSubtreeCTE above does it by JOINing a child to its parent —
// so a child whose parent_id matches no row joins to nothing and inherits
// neither. A mistyped parent_id is a child that walks out from under the hold
// its parent is under and out from under the dollar limit the user put on that
// tree, and every read path then reports it as ordinary open work. There is no
// foreign key on the column and nothing downstream can tell the difference
// later, so the write is the only place that can refuse.
//
// A cycle is the same escape by another route: every item in it has a parent, so
// none of them is reachable from outside it, and a hold placed above can never
// arrive. The walk terminates on a cycle rather than spinning (UNION dedupes),
// which is precisely why one would otherwise be silent.
var ErrUnknownParent = fmt.Errorf("parent_id names no item of this store")

// checkParent rejects a parent_id that names no live item, and one whose own
// ancestry already contains id. Walking up from the proposed parent covers the
// self-parent case without a special case for it: id is its own first ancestor.
//
// A deleted parent is refused too. A tombstone cannot seed the held subtree by
// design, so pointing a new child at one is the same hole as pointing it at
// nothing. Rows that already point at an item deleted after the fact are left
// alone — the delete is reversible, and the child is waiting for the restore.
func checkParent(handle databaseHandle, id string, parentID *string) error {
	if parentID == nil || *parentID == "" {
		return nil
	}

	var alive int
	err := handle.QueryRow(`SELECT 1 FROM items WHERE id = $1 AND deleted_at IS NULL`, *parentID).Scan(&alive)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrUnknownParent, *parentID)
	}
	if err != nil {
		return err
	}

	if id == "" {
		return nil // a brand-new item is in nobody's ancestry yet
	}

	var cycles int
	err = handle.QueryRow(`
		WITH RECURSIVE ancestors(id) AS (
			SELECT $1::text COLLATE "C"
			UNION
			SELECT i.parent_id FROM items i
			JOIN ancestors a ON i.id = a.id
			WHERE i.parent_id IS NOT NULL AND i.parent_id != ''
		)
		SELECT 1 FROM ancestors WHERE id = $2`,
		*parentID, id,
	).Scan(&cycles)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %q is already below %q, and an item inside its own ancestry can never be reached by a hold placed above it", ErrUnknownParent, *parentID, id)
}

// notHeld is the standing filter on every DISCOVERY path (list, search) — the
// paths an agent uses to find work it was not handed. It is deliberately absent
// from GetItem: fetching by id is not discovery, and a caller holding the id has
// already been handed the item.
//
// Excluding held items by DEFAULT is the whole design. The alternative — an
// opt-in filter every consumer must remember to pass — fails open: a consumer
// added later that knows nothing about the gate silently bypasses it. There are
// already five consumers (autoworker, dispatcher, scoper, classifier, reviewer),
// and the one that forgets is the one that picks up the parked work.
//
// Requires heldSubtreeCTE to be prepended to the query.
const notHeld = "items.id NOT IN (SELECT id FROM held_subtree)"

// inTransaction runs work in one transaction on the store's pool.
func (s *Store) inTransaction(work func(tx *sql.Tx) error) error {
	return postgresstorage.InTransaction(context.Background(), s.db, work)
}

func (s *Store) CreateItem(req *model.CreateItemRequest) (*model.Item, error) {
	if err := s.holding.checkType(req.Type); err != nil {
		return nil, err
	}
	if err := s.holding.checkListID(req.ListID); err != nil {
		return nil, err
	}
	now := storedNow()
	item := &model.Item{
		ID:        uuid.New().String(),
		Type:      req.Type,
		Title:     req.Title,
		Tags:      []string{},
		Links:     []string{},
		Status:    "open",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if req.Body != nil {
		item.Body = *req.Body
	}
	if req.Tags != nil {
		item.Tags = req.Tags
	}
	if req.Priority != nil {
		item.Priority = *req.Priority
	}
	if req.Rank != nil {
		item.Rank = *req.Rank
	}
	if req.Status != nil {
		item.Status = *req.Status
	}
	if req.ListID != nil {
		item.ListID = *req.ListID
	}
	if req.DueAt != nil {
		item.DueAt = req.DueAt
	}
	if req.ParentID != nil {
		if err := checkParent(s.db, "", req.ParentID); err != nil {
			return nil, err
		}
		item.ParentID = req.ParentID
	}
	if req.Links != nil {
		item.Links = req.Links
	}
	if req.CreatedBy != nil {
		item.CreatedBy = *req.CreatedBy
	}
	if req.Schedule != nil {
		item.Schedule = req.Schedule
	}

	tagsJSON, err := marshalStringList(item.Tags)
	if err != nil {
		return nil, err
	}
	linksJSON, err := marshalStringList(item.Links)
	if err != nil {
		return nil, err
	}
	dueAt, dueAtOffsetSeconds := dueAtColumns(item.DueAt)
	schedule, err := marshalSchedule(item.Schedule)
	if err != nil {
		return nil, err
	}

	if err := refuseNulCharacters(map[string][]string{"type": {item.Type}, "title": {item.Title}, "body": {item.Body}, "tags": item.Tags,
		"status": {item.Status}, "list_id": {item.ListID}, "parent_id": optionalText(item.ParentID), "links": item.Links,
		"created_by": {item.CreatedBy}, "hold_reason": {req.HoldReason}, "schedule": scheduleText(schedule)}); err != nil {
		return nil, err
	}

	// Born held, in the same INSERT — not created and then held in a second
	// write, which would leave a window in which an agent could list the item.
	var heldAt any
	if req.Hold {
		item.HeldAt = &now
		item.HoldReason = req.HoldReason
		heldAt = now
	}

	item.AutoHoldAtUSD = req.AutoHoldAtUSD
	var autoHoldAtUSD any
	if item.AutoHoldAtUSD != nil {
		autoHoldAtUSD = *item.AutoHoldAtUSD
	}

	_, err = s.db.Exec(
		"INSERT INTO items ("+insertCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)",
		item.ID, item.Type, item.Title, item.Body,
		tagsJSON, item.Priority, item.Rank, item.Status,
		item.ListID, dueAt, dueAtOffsetSeconds, item.ParentID, linksJSON,
		item.CreatedBy, item.CreatedAt, item.UpdatedAt, schedule,
		heldAt, item.HoldReason, autoHoldAtUSD,
	)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// marshalSchedule renders a schedule for storage. A nil schedule is stored as
// SQL NULL rather than "null" or "{}", so "has a rule" is a property the partial
// index can be built on and the coordinator can query for directly.
func marshalSchedule(sched *model.Schedule) (any, error) {
	if sched == nil {
		return nil, nil
	}
	j, err := json.Marshal(sched)
	if err != nil {
		return nil, fmt.Errorf("marshal schedule: %w", err)
	}
	return string(j), nil
}

// GetItem returns a live item. A deleted item is not found — callers that
// genuinely need one (restore, revision history) ask for it by name via
// GetItemIncludingDeleted, so no caller returns a tombstone by accident.
func (s *Store) GetItem(id string) (*model.Item, error) {
	return getItem(s.db, id, false, false)
}

func (s *Store) GetItemIncludingDeleted(id string) (*model.Item, error) {
	return getItem(s.db, id, true, false)
}

// getItem reads one item. forUpdate locks its row until handle's transaction
// ends, so the read, the check made on it and the write after it are one step
// against every other writer of that item, in this process or any other.
func getItem(handle databaseHandle, id string, includeDeleted, forUpdate bool) (*model.Item, error) {
	query := "SELECT " + itemCols + " FROM items WHERE id = $1"
	if !includeDeleted {
		query += " AND " + notDeleted
	}
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanItem(handle.QueryRow(query, id))
}

// snapshot records the CURRENT state of an item before it is mutated, so every
// change is reversible. It runs in the mutation's own transaction: if the
// snapshot fails the mutation does not happen, and if the mutation fails the
// snapshot is not kept.
func snapshot(handle databaseHandle, item *model.Item, reason string) error {
	tagsJSON, err := marshalStringList(item.Tags)
	if err != nil {
		return err
	}
	linksJSON, err := marshalStringList(item.Links)
	if err != nil {
		return err
	}
	_, err = handle.Exec(
		`INSERT INTO item_revisions
		   (item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		item.ID, item.Title, item.Body, tagsJSON, item.Status, item.Priority,
		item.ListID, item.ParentID, linksJSON, item.DeletedAt, reason, storedNow(),
	)
	return err
}

// ListRevisions returns a page of an item's prior states, newest first.
//
// limit <= 0 means every revision, which is what this call did before it could
// be paged and stays the default so no existing caller changes. That default is
// not cheap: a revision carries the WHOLE body it replaced, so the history of a
// long-lived item is the sum of every version of it. Measured 2026-08-21 on the
// nightly signpost todo, 308 revisions: 246 MB from one GET. An item that is
// rewritten on a schedule — the workspace type exists to be — grows this way by
// construction, and the workspace contract scheduler injects into every agent
// job's prompt points at exactly this call as the reason rewriting is safe.
//
// So a caller that only wants to see the last few states has to be able to say
// so. offset pages backwards through the rest. The ordering is by revision id,
// which is unique per row, so a page boundary cannot repeat or skip a revision
// the way an ordering full of ties can.
func (s *Store) ListRevisions(itemID string, limit, offset int) ([]*model.Revision, error) {
	arguments := &queryArguments{}
	q := `SELECT id, item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at
		   FROM item_revisions WHERE item_id = ` + arguments.add(itemID) + ` ORDER BY id DESC`
	if limit > 0 {
		q += " LIMIT " + arguments.add(limit)
	}
	if offset > 0 {
		q += " OFFSET " + arguments.add(offset)
	}

	rows, err := s.db.Query(q, arguments.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	revisions := []*model.Revision{}
	for rows.Next() {
		var rev model.Revision
		var tagsJSON, linksJSON []byte
		var parentID sql.NullString
		var deletedAt sql.NullTime
		var replacedAt time.Time
		if err := rows.Scan(
			&rev.ID, &rev.ItemID, &rev.Title, &rev.Body, &tagsJSON, &rev.Status,
			&rev.Priority, &rev.ListID, &parentID, &linksJSON, &deletedAt,
			&rev.Reason, &replacedAt,
		); err != nil {
			return nil, err
		}
		if rev.Tags, err = unmarshalStringList(tagsJSON, "revision tags", rev.ItemID); err != nil {
			return nil, err
		}
		if rev.Links, err = unmarshalStringList(linksJSON, "revision links", rev.ItemID); err != nil {
			return nil, err
		}
		if parentID.Valid {
			rev.ParentID = &parentID.String
		}
		rev.DeletedAt = timePointer(deletedAt)
		rev.ReplacedAt = replacedAt.UTC()
		revisions = append(revisions, &rev)
	}
	return revisions, rows.Err()
}

func (s *Store) UpdateItem(id string, req *model.UpdateItemRequest) (*model.Item, error) {
	var updated *model.Item
	err := s.inTransaction(func(tx *sql.Tx) error {
		// The row stays locked until the transaction ends, so of two updates
		// that both expected the same updated_at only the first is applied.
		existing, err := getItem(tx, id, false, true)
		if err != nil {
			return err
		}
		if err := refuseNulCharacters(map[string][]string{"title": optionalText(req.Title), "body": optionalText(req.Body), "tags": req.Tags,
			"status": optionalText(req.Status), "list_id": optionalText(req.ListID), "parent_id": optionalText(req.ParentID), "links": req.Links}); err != nil {
			return err
		}
		if err := s.holding.checkListID(req.ListID); err != nil {
			return err
		}
		// Checked before the snapshot, so a refused update leaves no revision.
		if req.ExpectedUpdatedAt != nil && !existing.UpdatedAt.Equal(*req.ExpectedUpdatedAt) {
			return &ItemChangedError{Current: existing}
		}
		if err := snapshot(tx, existing, "update"); err != nil {
			return fmt.Errorf("snapshot before update: %w", err)
		}

		sets := []string{}
		arguments := &queryArguments{}

		if req.Title != nil {
			sets = append(sets, "title = "+arguments.add(*req.Title))
		}
		if req.Body != nil {
			sets = append(sets, "body = "+arguments.add(*req.Body))
		}
		if req.HasTags {
			tags, err := marshalStringList(req.Tags)
			if err != nil {
				return err
			}
			sets = append(sets, "tags = "+arguments.add(tags))
		}
		if req.Priority != nil {
			sets = append(sets, "priority = "+arguments.add(*req.Priority))
		}
		if req.Rank != nil {
			sets = append(sets, "rank = "+arguments.add(*req.Rank))
		}
		if req.Status != nil {
			sets = append(sets, "status = "+arguments.add(*req.Status))
		}
		if req.ListID != nil {
			sets = append(sets, "list_id = "+arguments.add(*req.ListID))
		}
		// due_at keys off HasDueAt, not non-nil, so an explicit `"due_at": null`
		// clears the date. Non-nil alone made a due date unremovable, which a rolling
		// schedule needs at the end of its series.
		if req.HasDueAt {
			dueAt, dueAtOffsetSeconds := dueAtColumns(req.DueAt)
			sets = append(sets, "due_at = "+arguments.add(dueAt), "due_at_offset_seconds = "+arguments.add(dueAtOffsetSeconds))
		}
		if req.ParentID != nil {
			if err := checkParent(tx, id, req.ParentID); err != nil {
				return err
			}
			sets = append(sets, "parent_id = "+arguments.add(*req.ParentID))
		}
		if req.HasLinks {
			links, err := marshalStringList(req.Links)
			if err != nil {
				return err
			}
			sets = append(sets, "links = "+arguments.add(links))
		}

		// The anchor is checked against the MERGED item, not the request. A rule can
		// anchor on the item's due date, so a PATCH that clears due_at while leaving a
		// stored rule in place would strand that rule with nothing to count from — it
		// would expand to nothing and silently stop reminding. Neither half of that
		// PATCH looks wrong on its own; only the merge does.
		mergedDueAt := existing.DueAt
		if req.HasDueAt {
			mergedDueAt = req.DueAt
		}
		mergedSchedule := existing.Schedule
		if req.HasSchedule {
			mergedSchedule = req.Schedule
		}
		if mergedSchedule != nil {
			if _, err := mergedSchedule.Anchor(mergedDueAt); err != nil {
				return err
			}
		}
		if req.HasSchedule {
			schedule, err := marshalSchedule(req.Schedule)
			if err != nil {
				return err
			}
			if err := refuseNulCharacters(map[string][]string{"schedule": scheduleText(schedule)}); err != nil {
				return err
			}
			sets = append(sets, "schedule = "+arguments.add(schedule))
		}

		// Spend ceiling. Keys off HasAutoHoldAtUSD so an explicit `null` clears it.
		if req.HasAutoHoldAtUSD {
			if req.AutoHoldAtUSD != nil && *req.AutoHoldAtUSD < 0 {
				return fmt.Errorf("auto_hold_at_usd must not be negative (got %v)", *req.AutoHoldAtUSD)
			}
			var ceiling any
			if req.AutoHoldAtUSD != nil {
				ceiling = *req.AutoHoldAtUSD
			}
			sets = append(sets, "auto_hold_at_usd = "+arguments.add(ceiling))
		}

		if len(sets) == 0 {
			updated = existing
			return nil
		}

		sets = append(sets, "updated_at = "+arguments.add(storedNow()))
		query := "UPDATE items SET " + strings.Join(sets, ", ") + " WHERE id = " + arguments.add(id)
		if _, err := tx.Exec(query, arguments.values...); err != nil {
			return err
		}
		updated, err = getItem(tx, id, false, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteItem soft-deletes by stamping deleted_at. It no longer flips status to
// 'archived': archived is a state the user chose for a LIVE item, and deletion
// is the item being taken away. Collapsing the two meant a restore could not
// tell "he archived this" from "this was deleted", and it made the archive a
// dumping ground for things nobody meant to keep.
//
// `hard` still purges the row — kept for genuinely heavy data, not for ordinary
// deletes. It snapshots first, so even a purge leaves the content recoverable
// from item_revisions.
func (s *Store) DeleteItem(id string, hard bool) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		existing, err := getItem(tx, id, true, true)
		if err != nil {
			return err
		}
		reason := "delete"
		if hard {
			reason = "purge"
		}
		if err := snapshot(tx, existing, reason); err != nil {
			return fmt.Errorf("snapshot before %s: %w", reason, err)
		}

		if hard {
			_, err := tx.Exec("DELETE FROM items WHERE id = $1", id)
			return err
		}
		if existing.DeletedAt != nil {
			return nil // already deleted; don't move the tombstone's timestamp
		}
		now := storedNow()
		_, err = tx.Exec("UPDATE items SET deleted_at = $1, updated_at = $2 WHERE id = $3", now, now, id)
		return err
	})
}

// RestoreItem clears the tombstone. The item returns in exactly the state it
// was deleted in — status included, which is only possible because the delete
// did not overwrite it.
func (s *Store) RestoreItem(id string) (*model.Item, error) {
	var restored *model.Item
	err := s.inTransaction(func(tx *sql.Tx) error {
		existing, err := getItem(tx, id, true, true)
		if err != nil {
			return err
		}
		if existing.DeletedAt == nil {
			restored = existing
			return nil
		}
		if err := snapshot(tx, existing, "restore"); err != nil {
			return fmt.Errorf("snapshot before restore: %w", err)
		}
		if _, err := tx.Exec("UPDATE items SET deleted_at = NULL, updated_at = $1 WHERE id = $2", storedNow(), id); err != nil {
			return err
		}
		restored, err = getItem(tx, id, false, false)
		return err
	})
	return restored, err
}

// HoldItem parks an item: it stays open and visible to the user, and drops out
// of every agent discovery path until it is cleared. Re-holding an already-held
// item does not move its timestamp — the hold dates from when it was first
// applied, not from the last time someone pressed the button.
func (s *Store) HoldItem(id, reason string) (*model.Item, error) {
	if err := refuseNulCharacters(map[string][]string{"reason": {reason}}); err != nil {
		return nil, err
	}
	var held *model.Item
	err := s.inTransaction(func(tx *sql.Tx) error {
		existing, err := getItem(tx, id, false, true)
		if err != nil {
			return err
		}
		if existing.HeldAt != nil {
			held = existing
			return nil
		}
		if err := snapshot(tx, existing, "hold"); err != nil {
			return fmt.Errorf("snapshot before hold: %w", err)
		}
		now := storedNow()
		if _, err := tx.Exec("UPDATE items SET held_at = $1, hold_reason = $2, updated_at = $3 WHERE id = $4", now, reason, now, id); err != nil {
			return err
		}
		held, err = getItem(tx, id, false, false)
		return err
	})
	return held, err
}

// UnholdItem clears the gate — the item becomes agent-visible again. The reason
// is cleared with it: a stale "why this was parked" on live work is worse than
// none, because it reads as if the gate were still closed.
func (s *Store) UnholdItem(id string) (*model.Item, error) {
	var unheld *model.Item
	err := s.inTransaction(func(tx *sql.Tx) error {
		existing, err := getItem(tx, id, false, true)
		if err != nil {
			return err
		}
		if existing.HeldAt == nil {
			unheld = existing
			return nil
		}
		if err := snapshot(tx, existing, "unhold"); err != nil {
			return fmt.Errorf("snapshot before unhold: %w", err)
		}
		if _, err := tx.Exec("UPDATE items SET held_at = NULL, hold_reason = '', updated_at = $1 WHERE id = $2", storedNow(), id); err != nil {
			return err
		}
		unheld, err = getItem(tx, id, false, false)
		return err
	})
	return unheld, err
}

// ListScheduledItems returns every live item carrying a recurrence rule, in the
// order they were created. This is the reminder coordinator's scan: the set is a
// tiny slice of a table that is overwhelmingly unscheduled bot todos, so it rides
// the partial index rather than filtering the whole table in application code.
//
// Status is deliberately not filtered here. A `done` item can still be a live
// recurring TEMPLATE — the template is not the work — and the coordinator has to
// see it to keep generating occurrences. Deciding what a status means is the
// coordinator's job, not the store's.
func (s *Store) ListScheduledItems() ([]*model.Item, error) {
	return queryItems(s.db, "SELECT "+itemCols+" FROM items WHERE schedule IS NOT NULL AND "+notDeleted+" ORDER BY created_at, id")
}

func queryItems(handle databaseHandle, query string, arguments ...any) ([]*model.Item, error) {
	rows, err := handle.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*model.Item{}
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type ListParams struct {
	Type           string
	Tag            string
	ExcludeTags    []string
	Status         string
	ListID         string
	CreatedBy      string
	ParentID       string
	IncludeDeleted bool
	// IncludeHeld surfaces parked work. Default false, so a caller that has never
	// heard of the gate cannot pick up held work. The surfaces that exist to
	// manage the hold — the kanban board, the notes UI — set it to true.
	IncludeHeld bool
	Limit       int
	Offset      int
	Sort        string
}

// hasTag is the condition that an item's tags hold tag exactly. jsonb's
// containment reads the GIN index on tags.
func hasTag(arguments *queryArguments, tag string) string {
	return "items.tags @> jsonb_build_array(" + arguments.add(tag) + "::text)"
}

func (s *Store) ListItems(p ListParams) ([]*model.Item, error) {
	if p.Type != "" {
		if err := s.holding.checkType(p.Type); err != nil {
			return nil, err
		}
	}
	where := []string{}
	arguments := &queryArguments{}

	if !p.IncludeDeleted {
		where = append(where, notDeleted)
	}
	if !p.IncludeHeld {
		where = append(where, notHeld)
	}
	if p.ParentID != "" {
		where = append(where, "items.parent_id = "+arguments.add(p.ParentID))
	}
	if p.Type != "" {
		where = append(where, "items.type = "+arguments.add(p.Type))
	}
	if p.Tag != "" {
		where = append(where, hasTag(arguments, p.Tag))
	}
	for _, excluded := range p.ExcludeTags {
		if excluded == "" {
			continue
		}
		where = append(where, "NOT "+hasTag(arguments, excluded))
	}
	if p.Status != "" {
		where = append(where, "items.status = "+arguments.add(p.Status))
	}
	if p.ListID != "" {
		where = append(where, "items.list_id = "+arguments.add(p.ListID))
	}
	if p.CreatedBy != "" {
		where = append(where, "items.created_by = "+arguments.add(p.CreatedBy))
	}

	// The held-subtree CTE is only prepended when the hold filter is actually in
	// play. A caller asking to SEE held work should not pay for the recursion.
	q := "SELECT " + itemCols + " FROM items"
	if !p.IncludeHeld {
		q = heldSubtreeCTE + " " + q
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	// Every ordering ends in a tiebreaker that no two rows can share. The
	// columns callers sort by are full of ties — most open todos carry the same
	// priority, and rank is 0 for everything outside a ranked list — and a bare
	// "ORDER BY priority DESC" leaves the database free to return the tied rows
	// in any order it likes. Pair that with LIMIT and two identical reads can
	// hand back two different slices with no write in between, so a caller
	// reading the top N never learns that the rows it did not get exist.
	sort := "created_at DESC, id"
	switch p.Sort {
	case "rank":
		sort = "rank ASC, created_at DESC, id"
	case "priority":
		sort = "priority DESC, created_at DESC, id"
	case "updated_at":
		sort = "updated_at DESC, id"
	case "created_at":
		sort = "created_at DESC, id"
	}
	q += " ORDER BY " + sort

	limit := 100
	if p.Limit > 0 {
		limit = p.Limit
	}
	q += " LIMIT " + arguments.add(limit)
	if p.Offset > 0 {
		q += " OFFSET " + arguments.add(p.Offset)
	}
	return queryItems(s.db, q, arguments.values...)
}

func (s *Store) Rerank(items []model.RerankItem) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		now := storedNow()
		for _, item := range items {
			if _, err := tx.Exec("UPDATE items SET rank = $1, updated_at = $2 WHERE id = $3", item.Rank, now, item.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListLists() ([]model.ListInfo, error) {
	rows, err := s.db.Query("SELECT list_id, COUNT(*) AS count FROM items WHERE list_id != '' AND status != 'archived' AND deleted_at IS NULL GROUP BY list_id ORDER BY list_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lists := []model.ListInfo{}
	for rows.Next() {
		var li model.ListInfo
		if err := rows.Scan(&li.ListID, &li.Count); err != nil {
			return nil, err
		}
		lists = append(lists, li)
	}
	return lists, rows.Err()
}

func (s *Store) ListTags() ([]model.TagInfo, error) {
	// Ties in the count are broken by the tag's bytes, so the order is the
	// same every time.
	rows, err := s.db.Query(`SELECT tag, COUNT(*) AS count FROM items, jsonb_array_elements_text(items.tags) AS tag
		WHERE items.status != 'archived' AND items.deleted_at IS NULL GROUP BY tag ORDER BY count DESC, tag COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tags := []model.TagInfo{}
	for rows.Next() {
		var ti model.TagInfo
		if err := rows.Scan(&ti.Tag, &ti.Count); err != nil {
			return nil, err
		}
		tags = append(tags, ti)
	}
	return tags, rows.Err()
}

type SearchParams struct {
	Query  string
	Type   string
	Tag    string
	Status string
	Limit  int
	// IncludeHeld surfaces parked work. Search is a discovery path — an agent
	// told "find the todo about X" would otherwise route around the gate that
	// ListItems closes.
	IncludeHeld bool
}

func (s *Store) Search(p SearchParams) ([]*model.Item, error) {
	if p.Type != "" {
		if err := s.holding.checkType(p.Type); err != nil {
			return nil, err
		}
	}
	terms := searchTermsOf(p.Query)
	if len(terms) == 0 {
		return []*model.Item{}, nil
	}

	arguments := &queryArguments{}
	where := []string{}
	// Every term must match, each as a phrase of its words in order: the
	// implicit AND of the FTS5 query each term used to be quoted into.
	for _, term := range terms {
		where = append(where, "items.search_document @@ phraseto_tsquery('simple', "+arguments.add(term)+")")
	}
	where = append(where, notDeleted)
	if !p.IncludeHeld {
		where = append(where, notHeld)
	}
	if p.Type != "" {
		where = append(where, "items.type = "+arguments.add(p.Type))
	}
	if p.Tag != "" {
		where = append(where, hasTag(arguments, p.Tag))
	}
	if p.Status != "" {
		where = append(where, "items.status = "+arguments.add(p.Status))
	}

	limit := 50
	if p.Limit > 0 {
		limit = p.Limit
	}

	q := "SELECT " + itemCols + " FROM items WHERE " + strings.Join(where, " AND ") + " ORDER BY created_at DESC, id LIMIT " + arguments.add(limit)
	if !p.IncludeHeld {
		q = heldSubtreeCTE + " " + q
	}
	return queryItems(s.db, q, arguments.values...)
}

func (s *Store) ItemCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM items WHERE status != 'archived' AND deleted_at IS NULL").Scan(&count)
	return count, err
}

// SearchEach searches every store with the same parameters and answers the
// matches together in the order each store answers its own, newest first
// (created_at, then id), cut to the limit. It is the one search over notes and
// issues, whose items shared one search in noteboard. A type a store does not
// keep skips that store rather than failing, so a search for todos reads
// issues alone.
func SearchEach(stores []*Store, p SearchParams) ([]*model.Item, error) {
	found := []*model.Item{}
	for _, store := range stores {
		if p.Type != "" && !store.holding.Keeps(p.Type) {
			continue
		}
		matches, err := store.Search(p)
		if err != nil {
			return nil, fmt.Errorf("searching %s: %w", store.holding.Name, err)
		}
		found = append(found, matches...)
	}
	slices.SortStableFunc(found, func(a, b *model.Item) int {
		if byTime := b.CreatedAt.Compare(a.CreatedAt); byTime != 0 {
			return byTime
		}
		return strings.Compare(a.ID, b.ID)
	})
	limit := 50
	if p.Limit > 0 {
		limit = p.Limit
	}
	if len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}
