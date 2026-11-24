package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kayushkin/noteboard/model"
	_ "modernc.org/sqlite"
)

// dsnParams configures every connection modernc.org/sqlite opens.
//
// _pragma is the ONLY way this driver accepts pragmas — it silently ignores
// unrecognized DSN keys, so the mattn-style "_journal_mode=WAL&_busy_timeout=5000"
// this used to carry would be accepted and do nothing at all.
//
// _time_format=sqlite pins how a time.Time binds to a DATETIME column. The
// driver's default is Go's time.Time.String() — "2026-07-13 01:47:00.78 +0000
// UTC" — which is not a timestamp SQLite can read: date(), julianday() and
// strftime() all return NULL on it, silently. "sqlite" selects SQLite's own
// format, which is also byte-for-byte what mattn/go-sqlite3 wrote for this
// table's entire history, so rows written before and after the driver swap are
// indistinguishable. TestTimestampsAreStoredInSQLiteFormat and
// TestStoredTimestampsAreReadableBySQLiteDateFunctions pin both halves.
//
// journal_mode used to be in here with them and no longer is. The difference is
// what the setting belongs to: busy_timeout and _time_format are properties of a
// connection, so the driver is right to replay them on every one the pool opens,
// while journal_mode is a property of the file and needs a brief exclusive lock
// to change. Run from the DSN, that one statement lands in a place that cannot
// retry, and reports its failure as whatever statement happened to open the
// connection. See switchJournalMode.
const dsnParams = "_pragma=busy_timeout(5000)&_time_format=sqlite"

// busyTimeoutMilliseconds is how long a contended statement waits for a lock. It
// is the value spelled into dsnParams above, named here so the conversion below
// can be given the same patience rather than a second number of its own.
const busyTimeoutMilliseconds = 5000

// journalMode is the journal this database ends up in. WAL lets readers run
// while a writer holds the file, which is the whole reason to convert at all.
const journalMode = "WAL"

// switchJournalMode moves the database file into journalMode, waiting out
// another process converting the same fresh file.
//
// Switching a rollback-journal database into WAL takes a brief exclusive lock,
// and that one statement is the only part of opening a store that busy_timeout
// cannot mediate. Measured in memory-store, four connections racing to convert
// one fresh file, 160 opens per row:
//
//	journal_mode alone              120 failed
//	busy_timeout + journal_mode       1 failed, after 2ms
//	busy_timeout(30000) + same        2 failed, after 1ms
//
// The third row is the finding: six times the timeout changes nothing and the
// failure still lands in a millisecond, so the wait is never being entered.
// SQLite declines to run the busy handler when a connection has to upgrade a
// lock it already holds, because waiting there is how two connections deadlock;
// it returns SQLITE_BUSY on the spot instead. A longer timeout has nothing to
// give, so the wait has to be ours.
//
// Retrying converges because the race is only ever over the first conversion:
// once any process has won it the file is in WAL, and every later connection
// reads the mode back instead of changing it. This is not a retry loop around
// ordinary reads and writes — that tries to do WAL's job by waiting, and was
// measured worse than doing nothing. This runs once per store, on the one
// statement that turns WAL on.
func switchJournalMode(db *sql.DB) error {
	deadline := time.Now().Add(busyTimeoutMilliseconds * time.Millisecond)

	var settled string
	var err error
	for backoff := time.Millisecond; ; backoff += time.Millisecond {
		err = db.QueryRow("PRAGMA journal_mode(" + journalMode + ")").Scan(&settled)
		if err == nil && strings.EqualFold(settled, journalMode) {
			return nil
		}
		if !time.Now().Add(backoff).Before(deadline) {
			break
		}
		time.Sleep(backoff)
	}

	if err != nil {
		return fmt.Errorf("switch journal mode to %s: %w", journalMode, err)
	}
	// A lost race can also come back quietly, reporting the mode it stayed on
	// rather than an error, and a database left on the rollback journal is the
	// serialized queue WAL exists to prevent.
	return fmt.Errorf("journal mode settled on %q, want %s", settled, journalMode)
}

// readOnlyConnectionCount is how many connections the read-only pool may hold
// open at once. WAL lets each of them read while the single writer writes, so
// a large list read no longer queues every other request behind it.
const readOnlyConnectionCount = 4

type Store struct {
	db *sql.DB
	// readOnlyDB is a second pool on the same file that only reads. The list,
	// search and query reads run here, so a caller paging the whole table does
	// not hold the one connection every write and single-item read needs.
	// query_only makes SQLite refuse a write sent here by mistake.
	readOnlyDB *sql.DB
	// itemUpdateMutex makes UpdateItem's read, precondition check and write one
	// step against every other UpdateItem. The store is one process on one
	// connection, but a connection serializes statements, not the sequence of
	// them: without this, two updates that both expected the same updated_at
	// could both pass the check and the second would overwrite the first.
	itemUpdateMutex sync.Mutex
}

func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?"+dsnParams)
	if err != nil {
		return nil, err
	}

	// modernc.org/sqlite still surfaces SQLITE_BUSY to concurrent writers even
	// with WAL and a busy_timeout set, so the pool is pinned to a single
	// connection and writes serialize in database/sql instead.
	db.SetMaxOpenConns(1)

	if err := switchJournalMode(db); err != nil {
		db.Close()
		return nil, err
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	readOnlyDB, err := sql.Open("sqlite", dbPath+"?"+dsnParams+"&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, err
	}
	readOnlyDB.SetMaxOpenConns(readOnlyConnectionCount)
	if err := readOnlyDB.Ping(); err != nil {
		readOnlyDB.Close()
		db.Close()
		return nil, fmt.Errorf("open read-only pool: %w", err)
	}

	return &Store{db: db, readOnlyDB: readOnlyDB}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS items (
			id          TEXT PRIMARY KEY,
			type        TEXT NOT NULL,
			title       TEXT NOT NULL,
			body        TEXT DEFAULT '',
			tags        TEXT DEFAULT '[]',
			priority    INTEGER DEFAULT 0,
			rank        REAL DEFAULT 0,
			status      TEXT DEFAULT 'open',
			list_id     TEXT DEFAULT '',
			due_at      DATETIME,
			parent_id   TEXT,
			links       TEXT DEFAULT '[]',
			created_by  TEXT DEFAULT '',
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_items_type ON items(type);
		CREATE INDEX IF NOT EXISTS idx_items_status ON items(status);
		CREATE INDEX IF NOT EXISTS idx_items_list ON items(list_id);
		CREATE INDEX IF NOT EXISTS idx_items_created_by ON items(created_by);
	`)
	if err != nil {
		return err
	}

	// Reversible delete. SQLite has no ADD COLUMN IF NOT EXISTS, and this runs on
	// every boot, so the duplicate-column error on re-run is expected and ignored
	// — a failure here is not distinguishable from success, which is why the
	// column is verified by the index below rather than by this error.
	db.Exec(`ALTER TABLE items ADD COLUMN deleted_at DATETIME`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_items_deleted ON items(deleted_at)`); err != nil {
		return fmt.Errorf("items.deleted_at missing (ALTER failed and this is not a re-run): %w", err)
	}

	// Recurrence rule (RFC 5545), as JSON. Same ALTER-then-verify shape as
	// deleted_at above: the duplicate-column error on re-run is expected and
	// indistinguishable from success, so the partial index below is what actually
	// proves the column landed. The index is not merely a probe — it is the one
	// the coordinator scans on ("every item that has a rule"), which is a tiny
	// slice of a table that is mostly unscheduled bot todos.
	db.Exec(`ALTER TABLE items ADD COLUMN schedule TEXT`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_items_scheduled ON items(id) WHERE schedule IS NOT NULL`); err != nil {
		return fmt.Errorf("items.schedule missing (ALTER failed and this is not a re-run): %w", err)
	}

	// The agent gate. Same ALTER-then-verify shape as the two above. The partial
	// index names BOTH new columns, so it fails unless both landed — an index on
	// held_at alone would prove nothing about hold_reason.
	db.Exec(`ALTER TABLE items ADD COLUMN held_at DATETIME`)
	db.Exec(`ALTER TABLE items ADD COLUMN hold_reason TEXT DEFAULT ''`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_items_held ON items(id, hold_reason) WHERE held_at IS NOT NULL`); err != nil {
		return fmt.Errorf("items.held_at/hold_reason missing (ALTER failed and this is not a re-run): %w", err)
	}

	// Spend ceiling. NULL (not 0) means "no ceiling" — 0 is a real, meaningful
	// value here ("hold before spending anything"), so a NOT NULL DEFAULT 0 would
	// silently arm a ceiling on all 4100 existing rows. The partial index is the
	// curator's scan: the items with a ceiling are a tiny slice of the table.
	db.Exec(`ALTER TABLE items ADD COLUMN auto_hold_at_usd REAL`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_items_spend_ceiling ON items(id) WHERE auto_hold_at_usd IS NOT NULL`); err != nil {
		return fmt.Errorf("items.auto_hold_at_usd missing (ALTER failed and this is not a re-run): %w", err)
	}

	// Prior state of every item, snapshotted before each mutation. Nothing in
	// this store destroys data: DELETE sets deleted_at, and an UPDATE that
	// overwrites a body leaves the old one here. A `workspace` is rewritten on
	// every run of its job, so an agent that corrupts its own working memory
	// would otherwise erase the accumulated judgment with no way back.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS item_revisions (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			item_id     TEXT NOT NULL,
			title       TEXT NOT NULL,
			body        TEXT DEFAULT '',
			tags        TEXT DEFAULT '[]',
			status      TEXT DEFAULT 'open',
			priority    INTEGER DEFAULT 0,
			list_id     TEXT DEFAULT '',
			parent_id   TEXT,
			links       TEXT DEFAULT '[]',
			deleted_at  DATETIME,
			reason      TEXT NOT NULL,
			replaced_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_item_revisions_item ON item_revisions(item_id, id DESC);
	`); err != nil {
		return err
	}

	// Create FTS table if not exists
	var ftsExists int
	db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='items_fts'").Scan(&ftsExists)
	if ftsExists == 0 {
		_, err = db.Exec(`CREATE VIRTUAL TABLE items_fts USING fts5(title, body, content=items, content_rowid=rowid)`)
		if err != nil {
			return err
		}
		// Populate FTS from existing data
		_, err = db.Exec(`INSERT INTO items_fts(rowid, title, body) SELECT rowid, title, body FROM items`)
		if err != nil {
			return err
		}
	}

	// Triggers to keep FTS in sync
	db.Exec(`CREATE TRIGGER IF NOT EXISTS items_ai AFTER INSERT ON items BEGIN
		INSERT INTO items_fts(rowid, title, body) VALUES (new.rowid, new.title, new.body);
	END`)
	db.Exec(`CREATE TRIGGER IF NOT EXISTS items_ad AFTER DELETE ON items BEGIN
		INSERT INTO items_fts(items_fts, rowid, title, body) VALUES('delete', old.rowid, old.title, old.body);
	END`)
	db.Exec(`CREATE TRIGGER IF NOT EXISTS items_au AFTER UPDATE ON items BEGIN
		INSERT INTO items_fts(items_fts, rowid, title, body) VALUES('delete', old.rowid, old.title, old.body);
		INSERT INTO items_fts(rowid, title, body) VALUES (new.rowid, new.title, new.body);
	END`)

	return nil
}

func (s *Store) Close() error {
	return errors.Join(s.readOnlyDB.Close(), s.db.Close())
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

func scanItem(row interface{ Scan(...any) error }) (*model.Item, error) {
	var item model.Item
	var tagsJSON, linksJSON string
	// due_at is declared DATETIME, so the driver decodes it to a time.Time for
	// us — scanning it as a string would take a lossy detour back through
	// RFC3339Nano. Scanning it as a time also means an unparseable value fails
	// here instead of silently becoming the zero time.
	var dueAt sql.NullTime
	var parentID sql.NullString
	var deletedAt sql.NullTime
	var scheduleJSON sql.NullString
	var heldAt sql.NullTime
	var holdReason sql.NullString
	// NullFloat64, not float64: NULL means "no ceiling" and 0 means "hold before
	// spending a cent". Scanning into a plain float64 would collapse the two.
	var autoHoldAtUSD sql.NullFloat64

	err := row.Scan(
		&item.ID, &item.Type, &item.Title, &item.Body,
		&tagsJSON, &item.Priority, &item.Rank, &item.Status,
		&item.ListID, &dueAt, &parentID, &linksJSON,
		&item.CreatedBy, &item.CreatedAt, &item.UpdatedAt, &deletedAt,
		&scheduleJSON, &heldAt, &holdReason, &autoHoldAtUSD,
	)
	if err == sql.ErrNoRows {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, err
	}

	if deletedAt.Valid {
		item.DeletedAt = &deletedAt.Time
	}
	if heldAt.Valid {
		item.HeldAt = &heldAt.Time
	}
	if holdReason.Valid {
		item.HoldReason = holdReason.String
	}
	if autoHoldAtUSD.Valid {
		v := autoHoldAtUSD.Float64
		item.AutoHoldAtUSD = &v
	}
	if scheduleJSON.Valid && scheduleJSON.String != "" {
		// Unlike tags and links above, a malformed schedule is NOT swallowed. A
		// tags blob that fails to parse degrades to an empty list and the item is
		// still usable; a rule that fails to parse means the coordinator would
		// expand it to nothing and silently remind nobody, forever. That failure
		// is invisible by construction, so it has to be loud here.
		var sched model.Schedule
		if err := json.Unmarshal([]byte(scheduleJSON.String), &sched); err != nil {
			return nil, fmt.Errorf("item %s has an unreadable schedule: %w", item.ID, err)
		}
		item.Schedule = &sched
	}

	json.Unmarshal([]byte(tagsJSON), &item.Tags)
	if item.Tags == nil {
		item.Tags = []string{}
	}
	json.Unmarshal([]byte(linksJSON), &item.Links)
	if item.Links == nil {
		item.Links = []string{}
	}
	if dueAt.Valid {
		// Kept in whatever zone it was stored with, not normalized to UTC: the
		// offset is part of the value a caller gave us, and JSON renders it back.
		t := dueAt.Time
		item.DueAt = &t
	}
	if parentID.Valid {
		item.ParentID = &parentID.String
	}

	return &item, nil
}

// itemCols is the READ projection and includes deleted_at. insertCols is the
// write set for a new row and deliberately does not: an item is never born
// deleted, so the two lists are different sets rather than one list with a
// NULL padded onto every INSERT.
const itemCols = "id, type, title, body, tags, priority, rank, status, list_id, due_at, parent_id, links, created_by, created_at, updated_at, deleted_at, schedule, held_at, hold_reason, auto_hold_at_usd"
const insertCols = "id, type, title, body, tags, priority, rank, status, list_id, due_at, parent_id, links, created_by, created_at, updated_at, schedule, held_at, hold_reason, auto_hold_at_usd"

// notDeleted is the standing filter on every read path. A soft delete that any
// list still returns is not a delete.
const notDeleted = "deleted_at IS NULL"

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
var ErrUnknownParent = fmt.Errorf("parent_id names no item")

// checkParent rejects a parent_id that names no live item, and one whose own
// ancestry already contains id. Walking up from the proposed parent covers the
// self-parent case without a special case for it: id is its own first ancestor.
//
// A deleted parent is refused too. A tombstone cannot seed the held subtree by
// design, so pointing a new child at one is the same hole as pointing it at
// nothing. Rows that already point at an item deleted after the fact are left
// alone — the delete is reversible, and the child is waiting for the restore.
func (s *Store) checkParent(id string, parentID *string) error {
	if parentID == nil || *parentID == "" {
		return nil
	}

	var alive int
	err := s.db.QueryRow(
		`SELECT 1 FROM items WHERE id = ? AND deleted_at IS NULL`, *parentID,
	).Scan(&alive)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %q", ErrUnknownParent, *parentID)
	}
	if err != nil {
		return err
	}

	if id == "" {
		return nil // a brand-new item is in nobody's ancestry yet
	}

	var cycles int
	err = s.db.QueryRow(`
		WITH RECURSIVE ancestors(id) AS (
			SELECT ?
			UNION
			SELECT i.parent_id FROM items i
			JOIN ancestors a ON i.id = a.id
			WHERE i.parent_id IS NOT NULL AND i.parent_id != ''
		)
		SELECT 1 FROM ancestors WHERE id = ?`,
		*parentID, id,
	).Scan(&cycles)
	if err == sql.ErrNoRows {
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

func (s *Store) CreateItem(req *model.CreateItemRequest) (*model.Item, error) {
	now := time.Now().UTC()
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
		if err := s.checkParent("", req.ParentID); err != nil {
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

	tagsJSON, _ := json.Marshal(item.Tags)
	linksJSON, _ := json.Marshal(item.Links)

	var dueAt interface{}
	if item.DueAt != nil {
		dueAt = item.DueAt.Format(time.RFC3339)
	}
	schedule, err := marshalSchedule(item.Schedule)
	if err != nil {
		return nil, err
	}

	// Born held, in the same INSERT — not created and then held in a second
	// write, which would leave a window in which an agent could list the item.
	var heldAt interface{}
	if req.Hold {
		item.HeldAt = &now
		item.HoldReason = req.HoldReason
		heldAt = now
	}

	item.AutoHoldAtUSD = req.AutoHoldAtUSD
	var autoHoldAtUSD interface{}
	if item.AutoHoldAtUSD != nil {
		autoHoldAtUSD = *item.AutoHoldAtUSD
	}

	_, err = s.db.Exec(
		"INSERT INTO items ("+insertCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		item.ID, item.Type, item.Title, item.Body,
		string(tagsJSON), item.Priority, item.Rank, item.Status,
		item.ListID, dueAt, item.ParentID, string(linksJSON),
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
func marshalSchedule(sched *model.Schedule) (interface{}, error) {
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
	row := s.db.QueryRow("SELECT "+itemCols+" FROM items WHERE id = ? AND "+notDeleted, id)
	return scanItem(row)
}

func (s *Store) GetItemIncludingDeleted(id string) (*model.Item, error) {
	row := s.db.QueryRow("SELECT "+itemCols+" FROM items WHERE id = ?", id)
	return scanItem(row)
}

// snapshot records the CURRENT state of an item before it is mutated, so every
// change is reversible. Called inside the same transaction-less path as the
// mutation it precedes: if the snapshot fails the mutation must not proceed,
// because a change nobody can undo is exactly what this store no longer does.
func (s *Store) snapshot(item *model.Item, reason string) error {
	tagsJSON, _ := json.Marshal(item.Tags)
	linksJSON, _ := json.Marshal(item.Links)
	_, err := s.db.Exec(
		`INSERT INTO item_revisions
		   (item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.Title, item.Body, string(tagsJSON), item.Status, item.Priority,
		item.ListID, item.ParentID, string(linksJSON), item.DeletedAt, reason, time.Now().UTC(),
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
	q := `SELECT id, item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at
		   FROM item_revisions WHERE item_id = ? ORDER BY id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	} else if offset > 0 {
		// SQLite will not take OFFSET without LIMIT, and -1 is its own spelling
		// of "no limit". Without this an offset asked for on its own would be
		// dropped silently and the caller would be handed page one again.
		q += " LIMIT -1"
	}
	if offset > 0 {
		q += fmt.Sprintf(" OFFSET %d", offset)
	}

	rows, err := s.readOnlyDB.Query(q, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	revisions := []*model.Revision{}
	for rows.Next() {
		var rev model.Revision
		var tagsJSON, linksJSON string
		var parentID sql.NullString
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&rev.ID, &rev.ItemID, &rev.Title, &rev.Body, &tagsJSON, &rev.Status,
			&rev.Priority, &rev.ListID, &parentID, &linksJSON, &deletedAt,
			&rev.Reason, &rev.ReplacedAt,
		); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(tagsJSON), &rev.Tags)
		if rev.Tags == nil {
			rev.Tags = []string{}
		}
		json.Unmarshal([]byte(linksJSON), &rev.Links)
		if rev.Links == nil {
			rev.Links = []string{}
		}
		if parentID.Valid {
			rev.ParentID = &parentID.String
		}
		if deletedAt.Valid {
			rev.DeletedAt = &deletedAt.Time
		}
		revisions = append(revisions, &rev)
	}
	return revisions, rows.Err()
}

func (s *Store) UpdateItem(id string, req *model.UpdateItemRequest) (*model.Item, error) {
	s.itemUpdateMutex.Lock()
	defer s.itemUpdateMutex.Unlock()

	existing, err := s.GetItem(id)
	if err != nil {
		return nil, err
	}
	// Checked before the snapshot, so a refused update leaves no revision.
	if req.ExpectedUpdatedAt != nil && !existing.UpdatedAt.Equal(*req.ExpectedUpdatedAt) {
		return nil, &ItemChangedError{Current: existing}
	}
	if err := s.snapshot(existing, "update"); err != nil {
		return nil, fmt.Errorf("snapshot before update: %w", err)
	}

	sets := []string{}
	args := []interface{}{}

	if req.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *req.Title)
	}
	if req.Body != nil {
		sets = append(sets, "body = ?")
		args = append(args, *req.Body)
	}
	if req.HasTags {
		tags := req.Tags
		if tags == nil {
			tags = []string{}
		}
		j, _ := json.Marshal(tags)
		sets = append(sets, "tags = ?")
		args = append(args, string(j))
	}
	if req.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *req.Priority)
	}
	if req.Rank != nil {
		sets = append(sets, "rank = ?")
		args = append(args, *req.Rank)
	}
	if req.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *req.Status)
	}
	if req.ListID != nil {
		sets = append(sets, "list_id = ?")
		args = append(args, *req.ListID)
	}
	// due_at keys off HasDueAt, not non-nil, so an explicit `"due_at": null`
	// clears the date. Non-nil alone made a due date unremovable, which a rolling
	// schedule needs at the end of its series.
	if req.HasDueAt {
		sets = append(sets, "due_at = ?")
		if req.DueAt == nil {
			args = append(args, nil)
		} else {
			args = append(args, req.DueAt.Format(time.RFC3339))
		}
	}
	if req.ParentID != nil {
		if err := s.checkParent(id, req.ParentID); err != nil {
			return nil, err
		}
		sets = append(sets, "parent_id = ?")
		args = append(args, *req.ParentID)
	}
	if req.HasLinks {
		links := req.Links
		if links == nil {
			links = []string{}
		}
		j, _ := json.Marshal(links)
		sets = append(sets, "links = ?")
		args = append(args, string(j))
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
			return nil, err
		}
	}
	if req.HasSchedule {
		schedule, err := marshalSchedule(req.Schedule)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "schedule = ?")
		args = append(args, schedule)
	}

	// Spend ceiling. Keys off HasAutoHoldAtUSD so an explicit `null` clears it.
	if req.HasAutoHoldAtUSD {
		if req.AutoHoldAtUSD != nil && *req.AutoHoldAtUSD < 0 {
			return nil, fmt.Errorf("auto_hold_at_usd must not be negative (got %v)", *req.AutoHoldAtUSD)
		}
		sets = append(sets, "auto_hold_at_usd = ?")
		if req.AutoHoldAtUSD == nil {
			args = append(args, nil)
		} else {
			args = append(args, *req.AutoHoldAtUSD)
		}
	}

	if len(sets) == 0 {
		return existing, nil
	}

	now := time.Now().UTC()
	sets = append(sets, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	_, err = s.db.Exec("UPDATE items SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return nil, err
	}
	return s.GetItem(id)
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
	existing, err := s.GetItemIncludingDeleted(id)
	if err != nil {
		return err
	}
	reason := "delete"
	if hard {
		reason = "purge"
	}
	if err := s.snapshot(existing, reason); err != nil {
		return fmt.Errorf("snapshot before %s: %w", reason, err)
	}

	if hard {
		_, err := s.db.Exec("DELETE FROM items WHERE id = ?", id)
		return err
	}
	if existing.DeletedAt != nil {
		return nil // already deleted; don't move the tombstone's timestamp
	}
	now := time.Now().UTC()
	_, err = s.db.Exec("UPDATE items SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	return err
}

// RestoreItem clears the tombstone. The item returns in exactly the state it
// was deleted in — status included, which is only possible because the delete
// did not overwrite it.
func (s *Store) RestoreItem(id string) (*model.Item, error) {
	existing, err := s.GetItemIncludingDeleted(id)
	if err != nil {
		return nil, err
	}
	if existing.DeletedAt == nil {
		return existing, nil
	}
	if err := s.snapshot(existing, "restore"); err != nil {
		return nil, fmt.Errorf("snapshot before restore: %w", err)
	}
	if _, err := s.db.Exec("UPDATE items SET deleted_at = NULL, updated_at = ? WHERE id = ?", time.Now().UTC(), id); err != nil {
		return nil, err
	}
	return s.GetItem(id)
}

// HoldItem parks an item: it stays open and visible to the user, and drops out
// of every agent discovery path until it is cleared. Re-holding an already-held
// item does not move its timestamp — the hold dates from when it was first
// applied, not from the last time someone pressed the button.
func (s *Store) HoldItem(id, reason string) (*model.Item, error) {
	existing, err := s.GetItem(id)
	if err != nil {
		return nil, err
	}
	if existing.HeldAt != nil {
		return existing, nil
	}
	if err := s.snapshot(existing, "hold"); err != nil {
		return nil, fmt.Errorf("snapshot before hold: %w", err)
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(
		"UPDATE items SET held_at = ?, hold_reason = ?, updated_at = ? WHERE id = ?",
		now, reason, now, id,
	); err != nil {
		return nil, err
	}
	return s.GetItem(id)
}

// UnholdItem clears the gate — the item becomes agent-visible again. The reason
// is cleared with it: a stale "why this was parked" on live work is worse than
// none, because it reads as if the gate were still closed.
func (s *Store) UnholdItem(id string) (*model.Item, error) {
	existing, err := s.GetItem(id)
	if err != nil {
		return nil, err
	}
	if existing.HeldAt == nil {
		return existing, nil
	}
	if err := s.snapshot(existing, "unhold"); err != nil {
		return nil, fmt.Errorf("snapshot before unhold: %w", err)
	}
	if _, err := s.db.Exec(
		"UPDATE items SET held_at = NULL, hold_reason = '', updated_at = ? WHERE id = ?",
		time.Now().UTC(), id,
	); err != nil {
		return nil, err
	}
	return s.GetItem(id)
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
	rows, err := s.readOnlyDB.Query(
		"SELECT " + itemCols + " FROM items WHERE schedule IS NOT NULL AND " + notDeleted + " ORDER BY created_at",
	)
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

func (s *Store) ListItems(p ListParams) ([]*model.Item, error) {
	where := []string{}
	args := []interface{}{}

	if !p.IncludeDeleted {
		where = append(where, notDeleted)
	}
	if !p.IncludeHeld {
		where = append(where, notHeld)
	}
	if p.ParentID != "" {
		where = append(where, "parent_id = ?")
		args = append(args, p.ParentID)
	}
	if p.Type != "" {
		where = append(where, "type = ?")
		args = append(args, p.Type)
	}
	if p.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(tags) WHERE json_each.value = ?)")
		args = append(args, p.Tag)
	}
	for _, ex := range p.ExcludeTags {
		if ex == "" {
			continue
		}
		where = append(where, "NOT EXISTS (SELECT 1 FROM json_each(tags) WHERE json_each.value = ?)")
		args = append(args, ex)
	}
	if p.Status != "" {
		where = append(where, "status = ?")
		args = append(args, p.Status)
	}
	if p.ListID != "" {
		where = append(where, "list_id = ?")
		args = append(args, p.ListID)
	}
	if p.CreatedBy != "" {
		where = append(where, "created_by = ?")
		args = append(args, p.CreatedBy)
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
	// "ORDER BY priority DESC" leaves SQLite free to return the tied rows in any
	// order it likes. Pair that with LIMIT and two identical reads can hand back
	// two different slices with no write in between, so a caller reading the top
	// N never learns that the rows it did not get exist.
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

	if p.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", p.Limit)
	} else {
		q += " LIMIT 100"
	}
	if p.Offset > 0 {
		q += fmt.Sprintf(" OFFSET %d", p.Offset)
	}

	rows, err := s.readOnlyDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*model.Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if items == nil {
		items = []*model.Item{}
	}
	return items, nil
}

func (s *Store) Rerank(items []model.RerankItem) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("UPDATE items SET rank = ?, updated_at = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC()
	for _, item := range items {
		_, err := stmt.Exec(item.Rank, now, item.ID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListLists() ([]model.ListInfo, error) {
	rows, err := s.readOnlyDB.Query("SELECT list_id, COUNT(*) as count FROM items WHERE list_id != '' AND status != 'archived' AND deleted_at IS NULL GROUP BY list_id ORDER BY list_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lists []model.ListInfo
	for rows.Next() {
		var li model.ListInfo
		if err := rows.Scan(&li.ListID, &li.Count); err != nil {
			return nil, err
		}
		lists = append(lists, li)
	}
	if lists == nil {
		lists = []model.ListInfo{}
	}
	return lists, nil
}

func (s *Store) ListTags() ([]model.TagInfo, error) {
	rows, err := s.readOnlyDB.Query("SELECT j.value as tag, COUNT(*) as count FROM items, json_each(items.tags) as j WHERE items.status != 'archived' AND items.deleted_at IS NULL GROUP BY j.value ORDER BY count DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []model.TagInfo
	for rows.Next() {
		var ti model.TagInfo
		if err := rows.Scan(&ti.Tag, &ti.Count); err != nil {
			return nil, err
		}
		tags = append(tags, ti)
	}
	if tags == nil {
		tags = []model.TagInfo{}
	}
	return tags, nil
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

// ftsMatchQuery turns a raw user query into a safe FTS5 MATCH expression.
//
// FTS5's MATCH grammar treats a bare query string as a full query expression,
// so characters that are operators there — a hyphen (column-filter / the "-"
// NOT prefix), ':', '*', '(', '"', 'AND'/'OR'/'NOT' — make a plain word like
// "hello-world" a syntax error, which surfaced as a 500 on every hyphenated
// search. We tokenize on whitespace and wrap each token in a double-quoted FTS5
// string literal (doubling any embedded quote to escape it). A quoted token is
// literal text, not grammar, so punctuation inside it is harmless; joining the
// tokens with spaces keeps the implicit-AND ("all terms must match") behavior a
// raw multi-word query already had. An all-whitespace or empty query yields ""
// and the caller returns no results rather than issuing an invalid MATCH.
func ftsMatchQuery(raw string) string {
	fields := strings.Fields(raw)
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

func (s *Store) Search(p SearchParams) ([]*model.Item, error) {
	match := ftsMatchQuery(p.Query)
	if match == "" {
		return []*model.Item{}, nil
	}

	where := []string{"items.rowid IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ?)", notDeleted}
	args := []interface{}{match}

	if !p.IncludeHeld {
		where = append(where, notHeld)
	}

	if p.Type != "" {
		where = append(where, "type = ?")
		args = append(args, p.Type)
	}
	if p.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(tags) WHERE json_each.value = ?)")
		args = append(args, p.Tag)
	}
	if p.Status != "" {
		where = append(where, "status = ?")
		args = append(args, p.Status)
	}

	limit := 50
	if p.Limit > 0 {
		limit = p.Limit
	}

	q := fmt.Sprintf("SELECT %s FROM items WHERE %s ORDER BY created_at DESC LIMIT %d", itemCols, strings.Join(where, " AND "), limit)
	if !p.IncludeHeld {
		q = heldSubtreeCTE + " " + q
	}

	rows, err := s.readOnlyDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*model.Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if items == nil {
		items = []*model.Item{}
	}
	return items, nil
}

func (s *Store) ItemCount() (int, error) {
	var count int
	err := s.readOnlyDB.QueryRow("SELECT COUNT(*) FROM items WHERE status != 'archived' AND deleted_at IS NULL").Scan(&count)
	return count, err
}
