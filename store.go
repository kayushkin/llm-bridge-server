// Package activitystore keeps what the user did online: searches, pages
// visited, videos watched, apps used and places looked up, each one activity
// at one time, imported from Google Takeout.
//
// Activities are cited elsewhere by their ids (activity_000001), which
// kanban-store's registry claims, so a journal timeline or a note can link
// one as a chip.
package activitystore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
	ErrGone     = errors.New("gone")
)

// Store owns the database and the attachment files.
type Store struct {
	db      *sql.DB
	dataDir string
	// writeMutex serializes every write; the next id is MAX(seq)+1 read inside
	// the write's transaction.
	writeMutex sync.Mutex
}

// Open creates the data directory (0700) if needed, opens the database and
// applies the schema.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open store: no data directory given")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, attachmentDirectory), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "activity-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		if strings.Contains(err.Error(), "no such module: fts5") {
			return nil, fmt.Errorf("migrate: %w — this binary was built without FTS5; build with: go build -tags sqlite_fts5", err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database lives in.
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

type queryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Store) inTransaction(work func(tx *sql.Tx) error) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
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

// nextID mints the next id of a table: prefix_000001.
func nextID(tx queryer, table, prefix string) (string, int64, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM ` + table).Scan(&maxSeq); err != nil {
		return "", 0, err
	}
	seq := maxSeq.Int64 + 1
	return fmt.Sprintf("%s_%06d", prefix, seq), seq, nil
}

// DedupeKey is what finds the same activity seen by two sources: the url and
// the second it happened at. An activity with no url has none.
func DedupeKey(url string, occurredAt int64) string {
	if url == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(url + "\x00" + strconv.FormatInt(occurredAt, 10)))
	return hex.EncodeToString(digest[:16])
}

// searchableDetails is the text of Details that full-text search reads.
func searchableDetails(details Details) string {
	parts := []string{details.Header}
	parts = append(parts, details.Notes...)
	if details.Channel != nil {
		parts = append(parts, details.Channel.Name)
	}
	for _, location := range details.Locations {
		parts = append(parts, location.Name)
	}
	for _, line := range details.Lines {
		parts = append(parts, line.Text)
	}
	parts = append(parts, details.BookmarkFolder)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// ---- reading activities ----

const activityColumns = `a.id, a.product, a.action, a.action_text, a.title, a.url, a.occurred_at, a.latitude, a.longitude,
	a.details, a.source, a.source_ref, COALESCE(a.import_id, ''), a.created_at, a.updated_at, a.deleted_at, a.purged_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanActivity(row rowScanner) (Activity, error) {
	var activity Activity
	var latitude, longitude sql.NullFloat64
	var details string
	if err := row.Scan(&activity.ID, &activity.Product, &activity.Action, &activity.ActionText, &activity.Title, &activity.URL,
		&activity.OccurredAt, &latitude, &longitude, &details, &activity.Source, &activity.SourceRef, &activity.ImportID,
		&activity.CreatedAt, &activity.UpdatedAt, &activity.DeletedAt, &activity.PurgedAt); err != nil {
		return Activity{}, err
	}
	if latitude.Valid {
		activity.Latitude = &latitude.Float64
	}
	if longitude.Valid {
		activity.Longitude = &longitude.Float64
	}
	if err := json.Unmarshal([]byte(details), &activity.Details); err != nil {
		return Activity{}, fmt.Errorf("activity %s has details that are not JSON: %w", activity.ID, err)
	}
	activity.Sightings = []Sighting{}
	activity.Attachments = []Attachment{}
	return activity, nil
}

// fillRelated reads the sightings and attachments of each activity.
func fillRelated(q queryer, activities []Activity) error {
	for index := range activities {
		activity := &activities[index]
		rows, err := q.Query(`SELECT source, source_ref, product, action, import_id, occurred_at, seen_at FROM sightings
			WHERE activity_id = ? ORDER BY seen_at, rowid`, activity.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sighting Sighting
			if err := rows.Scan(&sighting.Source, &sighting.SourceRef, &sighting.Product, &sighting.Action, &sighting.ImportID,
				&sighting.OccurredAt, &sighting.SeenAt); err != nil {
				rows.Close()
				return err
			}
			activity.Sightings = append(activity.Sightings, sighting)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		attachments, err := attachmentsOf(q, activity.ID)
		if err != nil {
			return err
		}
		activity.Attachments = attachments
	}
	return nil
}

func getActivity(q queryer, id string) (Activity, error) {
	activity, err := scanActivity(q.QueryRow(`SELECT `+activityColumns+` FROM activities a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Activity{}, fmt.Errorf("%w: no activity %s", ErrNotFound, id)
	}
	if err != nil {
		return Activity{}, err
	}
	activities := []Activity{activity}
	if err := fillRelated(q, activities); err != nil {
		return Activity{}, err
	}
	return activities[0], nil
}

// GetActivity reads one activity. A deleted one is ErrNotFound unless
// includeDeleted.
func (s *Store) GetActivity(id string, includeDeleted bool) (Activity, error) {
	activity, err := getActivity(s.db, id)
	if err != nil {
		return Activity{}, err
	}
	if activity.DeletedAt != 0 && !includeDeleted {
		return Activity{}, fmt.Errorf("%w: activity %s is deleted", ErrNotFound, id)
	}
	return activity, nil
}

// ActivityFilter selects activities for ListActivities.
type ActivityFilter struct {
	From, To       int64 // occurred_at, from inclusive, to exclusive; 0 = open
	Product        string
	Action         string
	Source         string // any sighting from this source
	Query          string // full-text: every word, in title, url or details
	IncludeDeleted bool
	Descending     bool
	Limit, Offset  int
}

// ftsQuery turns what a person typed into an FTS5 query that matches every
// word, each quoted so that no word is read as FTS syntax.
func ftsQuery(typed string) string {
	words := strings.Fields(typed)
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

// ListActivities reads one page of activities and how many match in all.
func (s *Store) ListActivities(filter ActivityFilter) (ActivityPage, error) {
	if filter.Action != "" && !knownWord(Actions, filter.Action) {
		return ActivityPage{}, fmt.Errorf("%w: action %q is not one of %v", ErrInvalid, filter.Action, Actions)
	}
	if filter.Source != "" && !knownWord(Sources, filter.Source) {
		return ActivityPage{}, fmt.Errorf("%w: source %q is not one of %v", ErrInvalid, filter.Source, Sources)
	}
	from := ` FROM activities a`
	clauses := []string{}
	args := []any{}
	if strings.TrimSpace(filter.Query) != "" {
		from += ` JOIN activities_fts ON activities_fts.rowid = a.seq`
		clauses = append(clauses, `activities_fts MATCH ?`)
		args = append(args, ftsQuery(filter.Query))
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `a.deleted_at = 0`)
	}
	if filter.From != 0 {
		clauses = append(clauses, `a.occurred_at >= ?`)
		args = append(args, filter.From)
	}
	if filter.To != 0 {
		clauses = append(clauses, `a.occurred_at < ?`)
		args = append(args, filter.To)
	}
	if filter.Product != "" {
		clauses = append(clauses, `a.product = ?`)
		args = append(args, filter.Product)
	}
	if filter.Action != "" {
		clauses = append(clauses, `a.action = ?`)
		args = append(args, filter.Action)
	}
	if filter.Source != "" {
		clauses = append(clauses, `EXISTS (SELECT 1 FROM sightings g WHERE g.activity_id = a.id AND g.source = ?)`)
		args = append(args, filter.Source)
	}
	where := ""
	if len(clauses) > 0 {
		where = ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	page := ActivityPage{Activities: []Activity{}, Limit: filter.Limit, Offset: filter.Offset}
	if err := s.db.QueryRow(`SELECT COUNT(*)`+from+where, args...).Scan(&page.Total); err != nil {
		return ActivityPage{}, searchError(err)
	}
	order := ` ORDER BY a.occurred_at, a.seq`
	if filter.Descending {
		order = ` ORDER BY a.occurred_at DESC, a.seq DESC`
	}
	rows, err := s.db.Query(`SELECT `+activityColumns+from+where+order+` LIMIT ? OFFSET ?`, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return ActivityPage{}, searchError(err)
	}
	defer rows.Close()
	for rows.Next() {
		activity, err := scanActivity(rows)
		if err != nil {
			return ActivityPage{}, err
		}
		page.Activities = append(page.Activities, activity)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, err
	}
	if err := fillRelated(s.db, page.Activities); err != nil {
		return ActivityPage{}, err
	}
	return page, nil
}

// searchError makes an FTS5 syntax error a 400; ftsQuery quotes every word,
// so one should not happen, but if it does it is the caller's text.
func searchError(err error) error {
	if strings.Contains(err.Error(), "fts5") {
		return fmt.Errorf("%w: search: %v", ErrInvalid, err)
	}
	return err
}

// ---- counts ----

// Counts are the store's sizes, for GET /health.
func (s *Store) Counts() (HealthCounts, error) {
	var counts HealthCounts
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM activities WHERE deleted_at = 0),
		(SELECT COUNT(*) FROM activities WHERE deleted_at != 0),
		(SELECT COUNT(*) FROM activities WHERE purged_at != 0),
		(SELECT COUNT(*) FROM sightings),
		(SELECT COUNT(*) FROM attachments WHERE purged_at = 0),
		(SELECT COUNT(*) FROM imports),
		(SELECT COUNT(*) FROM revisions)`).Scan(&counts.Activities, &counts.Deleted, &counts.Purged, &counts.Sightings,
		&counts.Attachments, &counts.Imports, &counts.Revisions)
	return counts, err
}

func namedCounts(q queryer, query string, args ...any) ([]NamedCount, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := []NamedCount{}
	for rows.Next() {
		var count NamedCount
		if err := rows.Scan(&count.Name, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

// Stats counts live activities by product, source and action.
func (s *Store) Stats() (Stats, error) {
	var stats Stats
	var first, last sql.NullInt64
	if err := s.db.QueryRow(`SELECT COUNT(*), MIN(occurred_at), MAX(occurred_at) FROM activities WHERE deleted_at = 0`).
		Scan(&stats.Total, &first, &last); err != nil {
		return Stats{}, err
	}
	stats.FirstAt, stats.LastAt = first.Int64, last.Int64
	var err error
	if stats.ByProduct, err = namedCounts(s.db, `SELECT product, COUNT(*) FROM activities WHERE deleted_at = 0 GROUP BY product ORDER BY 2 DESC, 1`); err != nil {
		return Stats{}, err
	}
	if stats.BySource, err = namedCounts(s.db, `SELECT source, COUNT(*) FROM activities WHERE deleted_at = 0 GROUP BY source ORDER BY 2 DESC, 1`); err != nil {
		return Stats{}, err
	}
	if stats.ByAction, err = namedCounts(s.db, `SELECT action, COUNT(*) FROM activities WHERE deleted_at = 0 GROUP BY action ORDER BY 2 DESC, 1`); err != nil {
		return Stats{}, err
	}
	if stats.SightingsFrom, err = namedCounts(s.db, `SELECT g.source, COUNT(*) FROM sightings g JOIN activities a ON a.id = g.activity_id
		WHERE a.deleted_at = 0 GROUP BY g.source ORDER BY 2 DESC, 1`); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// ---- delete, restore, purge, revisions ----

func writeRevision(tx queryer, activity Activity, reason string) error {
	snapshot, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO revisions (activity_id, reason, revised_at, snapshot) VALUES (?, ?, ?, ?)`,
		activity.ID, reason, now(), string(snapshot))
	return err
}

// DeleteActivity stamps deleted_at after keeping a revision. Deleting a
// deleted activity changes nothing.
func (s *Store) DeleteActivity(id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		activity, err := getActivity(tx, id)
		if err != nil {
			return err
		}
		if activity.DeletedAt != 0 {
			return nil
		}
		if err := writeRevision(tx, activity, RevisionDelete); err != nil {
			return err
		}
		stamp := now()
		_, err = tx.Exec(`UPDATE activities SET deleted_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id)
		return err
	})
}

// RestoreActivity undoes a delete. A purged activity cannot come back.
func (s *Store) RestoreActivity(id string) (Activity, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		activity, err := getActivity(tx, id)
		if err != nil {
			return err
		}
		if activity.PurgedAt != 0 {
			return fmt.Errorf("%w: activity %s was purged and cannot be restored", ErrConflict, id)
		}
		if activity.DeletedAt == 0 {
			return nil
		}
		if err := writeRevision(tx, activity, RevisionRestore); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE activities SET deleted_at = 0, updated_at = ? WHERE id = ?`, now(), id)
		return err
	})
	if err != nil {
		return Activity{}, err
	}
	return getActivity(s.db, id)
}

// PurgeActivity removes what an activity said, for something that must not
// be kept: it blanks the title, url, words, details and location in the row
// and in every revision, deletes its search entry and its attachment files,
// and marks it deleted and purged. The row, its sightings and its dedupe key
// stay, so a citation still resolves and no later import brings it back.
// Purging again changes nothing.
func (s *Store) PurgeActivity(id string) (Activity, error) {
	var filesToRemove []string
	err := s.inTransaction(func(tx *sql.Tx) error {
		activity, err := getActivity(tx, id)
		if err != nil {
			return err
		}
		if activity.PurgedAt != 0 {
			return nil
		}
		if err := writeRevision(tx, blankActivity(activity), RevisionPurge); err != nil {
			return err
		}
		stamp := now()
		deletedAt := activity.DeletedAt
		if deletedAt == 0 {
			deletedAt = stamp
		}
		if _, err := tx.Exec(`UPDATE activities SET title = '', url = '', action_text = '', details = '{}', latitude = NULL, longitude = NULL,
			deleted_at = ?, purged_at = ?, updated_at = ? WHERE id = ?`, deletedAt, stamp, stamp, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM activities_fts WHERE rowid = (SELECT seq FROM activities WHERE id = ?)`, id); err != nil {
			return err
		}
		if err := blankRevisions(tx, id); err != nil {
			return err
		}
		filesToRemove, err = purgeAttachments(tx, id, stamp)
		return err
	})
	if err != nil {
		return Activity{}, err
	}
	for _, path := range filesToRemove {
		if err := os.Remove(filepath.Join(s.dataDir, path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Activity{}, fmt.Errorf("activity %s is purged but its file %s could not be removed: %w", id, path, err)
		}
	}
	return getActivity(s.db, id)
}

// blankActivity is an activity with everything it said removed.
func blankActivity(activity Activity) Activity {
	activity.Title, activity.URL, activity.ActionText = "", "", ""
	activity.Details = Details{}
	activity.Latitude, activity.Longitude = nil, nil
	for index := range activity.Attachments {
		activity.Attachments[index].FileName = ""
	}
	return activity
}

func blankRevisions(tx *sql.Tx, id string) error {
	rows, err := tx.Query(`SELECT id, snapshot FROM revisions WHERE activity_id = ?`, id)
	if err != nil {
		return err
	}
	type blanked struct {
		id       int64
		snapshot string
	}
	var updates []blanked
	for rows.Next() {
		var revisionID int64
		var snapshot string
		if err := rows.Scan(&revisionID, &snapshot); err != nil {
			rows.Close()
			return err
		}
		var activity Activity
		if err := json.Unmarshal([]byte(snapshot), &activity); err != nil {
			rows.Close()
			return fmt.Errorf("revision %d is not JSON: %w", revisionID, err)
		}
		encoded, err := json.Marshal(blankActivity(activity))
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, blanked{revisionID, string(encoded)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.Exec(`UPDATE revisions SET snapshot = ? WHERE id = ?`, update.snapshot, update.id); err != nil {
			return err
		}
	}
	return nil
}

// Revisions lists an activity's revisions, newest first.
func (s *Store) Revisions(id string) ([]Revision, error) {
	if _, err := getActivity(s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, activity_id, reason, revised_at, snapshot FROM revisions WHERE activity_id = ? ORDER BY id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var snapshot string
		if err := rows.Scan(&revision.ID, &revision.ActivityID, &revision.Reason, &revision.RevisedAt, &snapshot); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(snapshot), &revision.Activity); err != nil {
			return nil, fmt.Errorf("revision %d is not JSON: %w", revision.ID, err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}
