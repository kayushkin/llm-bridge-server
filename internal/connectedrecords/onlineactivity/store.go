// Package onlineactivity keeps what a person did online: searches, pages
// visited, videos watched, apps used and places looked up, each one activity
// at one time, imported from Google Takeout.
//
// Activities are cited elsewhere by their ids (activity_000001), which
// reference-store lists, so a journal timeline or a note can link one as a
// chip.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage, and the bytes of attachments in file-store, through
// AttachmentFiles. The server provisions, migrates and opens the module, then
// hands the pool to New. Every record has an owner (owner_principal_id), and
// every read and write is narrowed to an ownership.Scope.
package onlineactivity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
	ErrGone     = errors.New("gone")
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them. Until 2026-11-27 they were named activity and
// activity_module; an install that holds them is renamed at start.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "online_activity", Role: "online_activity_module", Migrations: migrations,
		FormerNames: []postgresstorage.FormerName{{Schema: "activity", Role: "activity_module"}}}, nil
}

// AttachmentFiles keeps attachment bytes. The server answers it with the
// file-store module in this process.
type AttachmentFiles interface {
	// Put keeps content and answers file-store's id (file_000001).
	Put(ctx context.Context, content io.Reader, file AttachmentFile) (fileID string, err error)
	// Open reads the bytes of a file Put kept.
	Open(ctx context.Context, fileID string) (io.ReadCloser, error)
	// Purge destroys a file Put kept.
	Purge(ctx context.Context, fileID string) error
}

// AttachmentFile is what Put is told: OwnerRef is the activity id.
type AttachmentFile struct{ ActivityID, FileName, ContentType string }

// Store is the records in the module's pool, and the attachment bytes in
// files.
type Store struct {
	db    *sql.DB
	files AttachmentFiles
	// writeMutex serializes every write, as the standalone store did: an
	// import's match on (source, source_ref) and on the dedupe key reads, then
	// writes, and two batches at once must not both decide a record is new.
	writeMutex sync.Mutex
}

// New is the store over db, the module's pool (Module), keeping attachment
// bytes in files.
func New(db *sql.DB, files AttachmentFiles) *Store {
	return &Store{db: db, files: files}
}

func now() int64 { return time.Now().Unix() }

// queryer is a pool or a transaction.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) inTransaction(ctx context.Context, work func(tx *sql.Tx) error) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	return postgresstorage.InTransaction(ctx, s.db, work)
}

// arguments numbers placeholders as they are added.
type arguments []any

func (a *arguments) add(value any) string {
	*a = append(*a, value)
	return "$" + strconv.Itoa(len(*a))
}

// scopeCondition is the scope as a condition on column, with its argument
// added to args. A scope that was never set is an error, not "everyone".
func scopeCondition(scope ownership.Scope, column string, args *arguments) (string, error) {
	if err := scope.Check(); err != nil {
		return "", err
	}
	condition, scopeArgs := scope.Condition(column, len(*args)+1)
	*args = append(*args, scopeArgs...)
	return condition, nil
}

// numberOf reads the number out of a public id; a malformed id is a missing
// record, never a guess.
func numberOf(prefix, id string) (int64, error) {
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: no %s %s", ErrNotFound, prefix, id)
	}
	return number, nil
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

const activityColumns = `a.seq, a.owner_principal_id, a.product, a.action, a.action_text, a.title, a.url, a.occurred_at, a.latitude, a.longitude,
	a.details, a.source, a.source_ref, a.import_seq, a.created_at, a.updated_at, a.deleted_at, a.purged_at`

type rowScanner interface {
	Scan(dest ...any) error
}

// scannedActivity is an activity and its number.
type scannedActivity struct {
	Activity
	seq int64
}

func scanActivity(row rowScanner) (scannedActivity, error) {
	var activity scannedActivity
	var latitude, longitude sql.NullFloat64
	var importSeq sql.NullInt64
	var details string
	if err := row.Scan(&activity.seq, &activity.OwnerPrincipalID, &activity.Product, &activity.Action, &activity.ActionText, &activity.Title, &activity.URL,
		&activity.OccurredAt, &latitude, &longitude, &details, &activity.Source, &activity.SourceRef, &importSeq,
		&activity.CreatedAt, &activity.UpdatedAt, &activity.DeletedAt, &activity.PurgedAt); err != nil {
		return scannedActivity{}, err
	}
	activity.ID = postgresstorage.FormatRecordID(IDPrefixActivity, activity.seq)
	if importSeq.Valid {
		activity.ImportID = postgresstorage.FormatRecordID(IDPrefixImport, importSeq.Int64)
	}
	if latitude.Valid {
		activity.Latitude = &latitude.Float64
	}
	if longitude.Valid {
		activity.Longitude = &longitude.Float64
	}
	if err := json.Unmarshal([]byte(details), &activity.Details); err != nil {
		return scannedActivity{}, fmt.Errorf("activity %s has details that are not JSON: %w", activity.ID, err)
	}
	activity.Sightings = []Sighting{}
	activity.Attachments = []Attachment{}
	return activity, nil
}

// fillRelated reads the sightings and attachments of each activity.
func fillRelated(ctx context.Context, q queryer, activities []scannedActivity) error {
	for index := range activities {
		activity := &activities[index]
		rows, err := q.QueryContext(ctx, `SELECT source, source_ref, product, action, import_seq, occurred_at, seen_at FROM sightings
			WHERE activity_seq = $1 ORDER BY seen_at, seq`, activity.seq)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sighting Sighting
			var importSeq int64
			if err := rows.Scan(&sighting.Source, &sighting.SourceRef, &sighting.Product, &sighting.Action, &importSeq,
				&sighting.OccurredAt, &sighting.SeenAt); err != nil {
				rows.Close()
				return err
			}
			sighting.ImportID = postgresstorage.FormatRecordID(IDPrefixImport, importSeq)
			activity.Sightings = append(activity.Sightings, sighting)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		attachments, err := attachmentsOf(ctx, q, activity.seq)
		if err != nil {
			return err
		}
		activity.Attachments = attachments
	}
	return nil
}

func activitiesOf(scanned []scannedActivity) []Activity {
	activities := make([]Activity, len(scanned))
	for index := range scanned {
		activities[index] = scanned[index].Activity
	}
	return activities
}

// getActivity reads one activity in scope, whole. One out of scope is
// ErrNotFound, as a missing one is.
func getActivity(ctx context.Context, q queryer, scope ownership.Scope, id string) (scannedActivity, error) {
	seq, err := numberOf(IDPrefixActivity, id)
	if err != nil {
		return scannedActivity{}, err
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "a.owner_principal_id", &args)
	if err != nil {
		return scannedActivity{}, err
	}
	activity, err := scanActivity(q.QueryRowContext(ctx, `SELECT `+activityColumns+` FROM activities a WHERE a.seq = $1 AND `+condition, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return scannedActivity{}, fmt.Errorf("%w: no activity %s", ErrNotFound, id)
	}
	if err != nil {
		return scannedActivity{}, err
	}
	activities := []scannedActivity{activity}
	if err := fillRelated(ctx, q, activities); err != nil {
		return scannedActivity{}, err
	}
	return activities[0], nil
}

// GetActivity reads one activity. A deleted one is ErrNotFound unless
// includeDeleted.
func (s *Store) GetActivity(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (Activity, error) {
	activity, err := getActivity(ctx, s.db, scope, id)
	if err != nil {
		return Activity{}, err
	}
	if activity.DeletedAt != 0 && !includeDeleted {
		return Activity{}, fmt.Errorf("%w: activity %s is deleted", ErrNotFound, id)
	}
	return activity.Activity, nil
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

// ListActivities reads one page of activities in scope and how many match in
// all.
func (s *Store) ListActivities(ctx context.Context, scope ownership.Scope, filter ActivityFilter) (ActivityPage, error) {
	if filter.Action != "" && !knownWord(Actions, filter.Action) {
		return ActivityPage{}, fmt.Errorf("%w: action %q is not one of %v", ErrInvalid, filter.Action, Actions)
	}
	if filter.Source != "" && !knownWord(Sources, filter.Source) {
		return ActivityPage{}, fmt.Errorf("%w: source %q is not one of %v", ErrInvalid, filter.Source, Sources)
	}
	page := ActivityPage{Activities: []Activity{}, Limit: filter.Limit, Offset: filter.Offset}
	var args arguments
	condition, err := scopeCondition(scope, "a.owner_principal_id", &args)
	if err != nil {
		return ActivityPage{}, err
	}
	clauses := []string{condition}
	if strings.TrimSpace(filter.Query) != "" {
		expression, searchable := searchQueryOf(filter.Query)
		if !searchable {
			// Words with no letter or digit in them match nothing.
			return page, nil
		}
		clauses = append(clauses, `a.search_document @@ to_tsquery('simple', `+args.add(expression)+`)`)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `a.deleted_at = 0`)
	}
	if filter.From != 0 {
		clauses = append(clauses, `a.occurred_at >= `+args.add(filter.From))
	}
	if filter.To != 0 {
		clauses = append(clauses, `a.occurred_at < `+args.add(filter.To))
	}
	if filter.Product != "" {
		clauses = append(clauses, `a.product = `+args.add(filter.Product))
	}
	if filter.Action != "" {
		clauses = append(clauses, `a.action = `+args.add(filter.Action))
	}
	if filter.Source != "" {
		clauses = append(clauses, `EXISTS (SELECT 1 FROM sightings g WHERE g.activity_seq = a.seq AND g.source = `+args.add(filter.Source)+`)`)
	}
	where := ` FROM activities a WHERE ` + strings.Join(clauses, ` AND `)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where, args...).Scan(&page.Total); err != nil {
		return ActivityPage{}, err
	}
	order := ` ORDER BY a.occurred_at, a.seq`
	if filter.Descending {
		order = ` ORDER BY a.occurred_at DESC, a.seq DESC`
	}
	limit := args.add(filter.Limit)
	offset := args.add(filter.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT `+activityColumns+where+order+` LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return ActivityPage{}, err
	}
	defer rows.Close()
	var scanned []scannedActivity
	for rows.Next() {
		activity, err := scanActivity(rows)
		if err != nil {
			return ActivityPage{}, err
		}
		scanned = append(scanned, activity)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, err
	}
	rows.Close()
	if err := fillRelated(ctx, s.db, scanned); err != nil {
		return ActivityPage{}, err
	}
	page.Activities = append(page.Activities, activitiesOf(scanned)...)
	return page, nil
}

// ---- counts ----

// Counts are the sizes of the store in scope, for GET /health.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (HealthCounts, error) {
	var args arguments
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return HealthCounts{}, err
	}
	var counts HealthCounts
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM activities WHERE deleted_at = 0 AND `+condition+`),
		(SELECT COUNT(*) FROM activities WHERE deleted_at <> 0 AND `+condition+`),
		(SELECT COUNT(*) FROM activities WHERE purged_at <> 0 AND `+condition+`),
		(SELECT COUNT(*) FROM sightings WHERE `+condition+`),
		(SELECT COUNT(*) FROM attachments WHERE purged_at = 0 AND `+condition+`),
		(SELECT COUNT(*) FROM imports WHERE `+condition+`),
		(SELECT COUNT(*) FROM revisions WHERE `+condition+`)`, args...).Scan(&counts.Activities, &counts.Deleted, &counts.Purged, &counts.Sightings,
		&counts.Attachments, &counts.Imports, &counts.Revisions)
	return counts, err
}

func namedCounts(ctx context.Context, q queryer, query string, args ...any) ([]NamedCount, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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

// Stats counts live activities in scope by product, source and action.
func (s *Store) Stats(ctx context.Context, scope ownership.Scope) (Stats, error) {
	var args arguments
	condition, err := scopeCondition(scope, "a.owner_principal_id", &args)
	if err != nil {
		return Stats{}, err
	}
	live := ` FROM activities a WHERE a.deleted_at = 0 AND ` + condition
	var stats Stats
	var first, last sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(a.occurred_at), MAX(a.occurred_at)`+live, args...).
		Scan(&stats.Total, &first, &last); err != nil {
		return Stats{}, err
	}
	stats.FirstAt, stats.LastAt = first.Int64, last.Int64
	if stats.ByProduct, err = namedCounts(ctx, s.db, `SELECT a.product, COUNT(*)`+live+` GROUP BY a.product ORDER BY 2 DESC, 1`, args...); err != nil {
		return Stats{}, err
	}
	if stats.BySource, err = namedCounts(ctx, s.db, `SELECT a.source, COUNT(*)`+live+` GROUP BY a.source ORDER BY 2 DESC, 1`, args...); err != nil {
		return Stats{}, err
	}
	if stats.ByAction, err = namedCounts(ctx, s.db, `SELECT a.action, COUNT(*)`+live+` GROUP BY a.action ORDER BY 2 DESC, 1`, args...); err != nil {
		return Stats{}, err
	}
	if stats.SightingsFrom, err = namedCounts(ctx, s.db, `SELECT g.source, COUNT(*) FROM sightings g JOIN activities a ON a.seq = g.activity_seq
		WHERE a.deleted_at = 0 AND `+condition+` GROUP BY g.source ORDER BY 2 DESC, 1`, args...); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// ---- delete, restore, purge, revisions ----

func writeRevision(ctx context.Context, tx queryer, activity scannedActivity, reason string) error {
	snapshot, err := json.Marshal(activity.Activity)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO revisions (owner_principal_id, activity_seq, reason, revised_at, snapshot) VALUES ($1, $2, $3, $4, $5)`,
		activity.OwnerPrincipalID, activity.seq, reason, now(), string(snapshot))
	return err
}

// DeleteActivity stamps deleted_at after keeping a revision. Deleting a
// deleted activity changes nothing.
func (s *Store) DeleteActivity(ctx context.Context, scope ownership.Scope, id string) error {
	return s.inTransaction(ctx, func(tx *sql.Tx) error {
		activity, err := getActivity(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if activity.DeletedAt != 0 {
			return nil
		}
		if err := writeRevision(ctx, tx, activity, RevisionDelete); err != nil {
			return err
		}
		stamp := now()
		_, err = tx.ExecContext(ctx, `UPDATE activities SET deleted_at = $1, updated_at = $2 WHERE seq = $3`, stamp, stamp, activity.seq)
		return err
	})
}

// RestoreActivity undoes a delete. A purged activity cannot come back.
func (s *Store) RestoreActivity(ctx context.Context, scope ownership.Scope, id string) (Activity, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		activity, err := getActivity(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if activity.PurgedAt != 0 {
			return fmt.Errorf("%w: activity %s was purged and cannot be restored", ErrConflict, id)
		}
		if activity.DeletedAt == 0 {
			return nil
		}
		if err := writeRevision(ctx, tx, activity, RevisionRestore); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE activities SET deleted_at = 0, updated_at = $1 WHERE seq = $2`, now(), activity.seq)
		return err
	})
	if err != nil {
		return Activity{}, err
	}
	return s.GetActivity(ctx, scope, id, true)
}

// PurgeActivity removes what an activity said, for something that must not
// be kept: it blanks the title, url, words, details and location in the row
// and in every revision, empties its search document, destroys its
// attachments' files in file-store, and marks it deleted and purged. The row,
// its sightings and its dedupe key stay, so a citation still resolves and no
// later import brings it back. Purging again changes nothing.
func (s *Store) PurgeActivity(ctx context.Context, scope ownership.Scope, id string) (Activity, error) {
	var filesToPurge []string
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		activity, err := getActivity(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if activity.PurgedAt != 0 {
			return nil
		}
		if err := writeRevision(ctx, tx, blankActivity(activity), RevisionPurge); err != nil {
			return err
		}
		stamp := now()
		deletedAt := activity.DeletedAt
		if deletedAt == 0 {
			deletedAt = stamp
		}
		if _, err := tx.ExecContext(ctx, `UPDATE activities SET title = '', url = '', action_text = '', details = '{}', latitude = NULL, longitude = NULL,
			search_document = ''::tsvector, deleted_at = $1, purged_at = $2, updated_at = $3 WHERE seq = $4`, deletedAt, stamp, stamp, activity.seq); err != nil {
			return err
		}
		if err := blankRevisions(ctx, tx, activity.seq); err != nil {
			return err
		}
		filesToPurge, err = purgeAttachments(ctx, tx, activity.seq, stamp)
		return err
	})
	if err != nil {
		return Activity{}, err
	}
	// The rows no longer name the files, so their bytes go after the commit.
	// A file that cannot be destroyed is reported, never passed over.
	for _, fileID := range filesToPurge {
		if err := s.files.Purge(ctx, fileID); err != nil {
			return Activity{}, fmt.Errorf("activity %s is purged but its attachment file %s could not be destroyed in file-store: %w", id, fileID, err)
		}
	}
	return s.GetActivity(ctx, scope, id, true)
}

// blankActivity is an activity with everything it said removed.
func blankActivity(activity scannedActivity) scannedActivity {
	activity.Title, activity.URL, activity.ActionText = "", "", ""
	activity.Details = Details{}
	activity.Latitude, activity.Longitude = nil, nil
	attachments := make([]Attachment, len(activity.Attachments))
	copy(attachments, activity.Attachments)
	for index := range attachments {
		attachments[index].FileName = ""
	}
	activity.Attachments = attachments
	return activity
}

func blankRevisions(ctx context.Context, tx *sql.Tx, activitySeq int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, snapshot FROM revisions WHERE activity_seq = $1`, activitySeq)
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
		encoded, err := json.Marshal(blankActivity(scannedActivity{Activity: activity}).Activity)
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
		if _, err := tx.ExecContext(ctx, `UPDATE revisions SET snapshot = $1 WHERE id = $2`, update.snapshot, update.id); err != nil {
			return err
		}
	}
	return nil
}

// Revisions lists an activity's revisions, newest first.
func (s *Store) Revisions(ctx context.Context, scope ownership.Scope, id string) ([]Revision, error) {
	activity, err := getActivity(ctx, s.db, scope, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner_principal_id, reason, revised_at, snapshot FROM revisions WHERE activity_seq = $1 ORDER BY id DESC`, activity.seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		revision := Revision{ActivityID: activity.ID}
		var snapshot string
		if err := rows.Scan(&revision.ID, &revision.OwnerPrincipalID, &revision.Reason, &revision.RevisedAt, &snapshot); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(snapshot), &revision.Activity); err != nil {
			return nil, fmt.Errorf("revision %d is not JSON: %w", revision.ID, err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}
