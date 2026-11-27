// Package photos keeps what is known about one owner's photos: where the
// original is, when it was taken and how sure that is, where (GPS), a
// thumbnail, and a description. The originals stay on the machine they were
// found on; only thumbnails come here, and their bytes are kept by file-store
// (ThumbnailFiles).
//
// Every record has an owner, the principal who keeps it. A request reaches
// only its caller's own records unless the caller sees every owner (an
// administrator, or the server's own service).
//
// A photo is cited elsewhere by its id (photo_000001), which reference-store's
// list of types claims, so a timeline item or a note can link one as a chip.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
package photos

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	// ErrConflict is a change the record's state refuses (a purged photo, a
	// decided group, a second scan), or a uniqueness rule another write won.
	ErrConflict = errors.New("conflict")
	// ErrGone is a purged photo's thumbnail or face: removed for good.
	ErrGone = errors.New("gone")
	// ErrUpstream is a module this store asks (people-store) failing to
	// answer.
	ErrUpstream = errors.New("upstream failed")
)

// The type parts of the public ids: photo_000001, face_000001,
// photo_person_000001, face_group_000001, face_group_guess_000001.
const (
	PhotoIDPrefix          = "photo"
	FaceIDPrefix           = "face"
	PhotoPersonIDPrefix    = "photo_person"
	FaceGroupIDPrefix      = "face_group"
	FaceGroupGuessIDPrefix = "face_group_guess"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "photos", Role: "photos_module", Migrations: migrations}, nil
}

// ThumbnailFiles keeps thumbnails' bytes: file-store, in this process. Each
// photo's thumbnail is one file there, named by the photo it belongs to.
type ThumbnailFiles interface {
	// Keep stores jpeg as photoID's thumbnail, owned by ownerPrincipalID,
	// and answers the new file's id (file_000001).
	Keep(ctx context.Context, ownerPrincipalID, photoID string, jpeg []byte) (string, error)
	// KeepCopyOf stores the file at path as photoID's thumbnail; the importer
	// uses it to carry a standalone store's thumbnails without reading them
	// into memory (file-store may hard-link them).
	KeepCopyOf(ctx context.Context, ownerPrincipalID, photoID, path string) (string, error)
	// Open answers a kept thumbnail's bytes.
	Open(ctx context.Context, fileID string) (io.ReadCloser, error)
	// Destroy removes a kept thumbnail for good.
	Destroy(ctx context.Context, fileID string) error
}

// People is people-store, asked about one owner's records.
type People interface {
	// PersonExists says whether ownerPrincipalID's records hold the person,
	// not deleted.
	PersonExists(ctx context.Context, ownerPrincipalID, personID string) (bool, error)
}

// Store keeps the records in the module's database.
type Store struct {
	db         *sql.DB
	thumbnails ThumbnailFiles
	people     People
}

// New returns the store over db, the module's pool. thumbnails keeps the
// thumbnails' bytes; people says which persons an owner keeps. None may be
// nil.
func New(db *sql.DB, thumbnails ThumbnailFiles, people People) *Store {
	if db == nil || thumbnails == nil || people == nil {
		panic("photostore.New: a database, a keeper of thumbnail files and people-store are all required")
	}
	return &Store{db: db, thumbnails: thumbnails, people: people}
}

func now() int64 { return time.Now().Unix() }

// recordNumberOf reads the number of a public id of the type prefix names. A
// malformed id names no record, so it is not found, as a missing one is.
func recordNumberOf(prefix, kind, id string) (int64, error) {
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: no %s %s", ErrNotFound, kind, id)
	}
	return number, nil
}

func photoID(seq int64) string { return postgresstorage.FormatRecordID(PhotoIDPrefix, seq) }

// queryer is what both *sql.DB and *sql.Tx offer, so a read can run inside a
// write's transaction or outside one.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// arguments numbers the placeholders of a query as its arguments are added.
type arguments struct{ values []any }

// add appends value and answers its placeholder.
func (a *arguments) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// scope answers the condition that holds column to scope, adding its argument.
func (a *arguments) scope(scope ownership.Scope, column string) string {
	condition, values := scope.Condition(column, len(a.values)+1)
	a.values = append(a.values, values...)
	return condition
}

func checkScope(scope ownership.Scope) error {
	if err := scope.Check(); err != nil {
		return fmt.Errorf("photostore: %w", err)
	}
	return nil
}

// inTransaction runs work in one transaction: committed when it returns nil,
// rolled back otherwise.
func (s *Store) inTransaction(ctx context.Context, work func(tx *sql.Tx) error) error {
	return postgresstorage.InTransaction(ctx, s.db, work)
}

// isUniqueViolation reports a uniqueness rule the database enforced.
func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

// conflictOf turns a uniqueness violation, a race the store's own checks
// lost, into ErrConflict.
func conflictOf(err error) error {
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: another write took it first; send it again: %v", ErrConflict, err)
	}
	return err
}

// personKeptByOwner refuses a person the owner's people-store records do not
// hold, and a people-store that cannot say.
func (s *Store) personKeptByOwner(ctx context.Context, ownerPrincipalID, personID string) error {
	exists, err := s.people.PersonExists(ctx, ownerPrincipalID, personID)
	if err != nil {
		return fmt.Errorf("%w: asking people-store whether %s keeps %s: %v", ErrUpstream, ownerPrincipalID, personID, err)
	}
	if !exists {
		return fmt.Errorf("%w: person_id %s is not a person in the records of %s, who owns this", ErrInvalid, personID, ownerPrincipalID)
	}
	return nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const photoColumns = `seq, owner_principal_id, sha256, source_machine, source_path, folder, media_type, taken_at, date_source,
	latitude, longitude, width, height, description, described_by, tags::text, created_at, updated_at, deleted_at, purged_at`

func scanPhoto(scan func(...any) error) (*Photo, error) {
	var photo Photo
	var seq int64
	var latitude, longitude sql.NullFloat64
	var tags string
	if err := scan(&seq, &photo.OwnerPrincipalID, &photo.Sha256, &photo.SourceMachine, &photo.SourcePath, &photo.Folder, &photo.MediaType,
		&photo.TakenAt, &photo.DateSource, &latitude, &longitude, &photo.Width, &photo.Height, &photo.Description,
		&photo.DescribedBy, &tags, &photo.CreatedAt, &photo.UpdatedAt, &photo.DeletedAt, &photo.PurgedAt); err != nil {
		return nil, err
	}
	photo.ID = photoID(seq)
	if latitude.Valid && longitude.Valid {
		photo.Latitude, photo.Longitude = &latitude.Float64, &longitude.Float64
	}
	if err := json.Unmarshal([]byte(tags), &photo.Tags); err != nil {
		return nil, fmt.Errorf("photo %s has tags that are not a JSON array: %w", photo.ID, err)
	}
	return &photo, nil
}

// getRow reads one photo in scope, deleted or not; forUpdate locks its row
// for the rest of the transaction.
func getRow(ctx context.Context, q queryer, scope ownership.Scope, id string, forUpdate bool) (*Photo, int64, error) {
	if err := checkScope(scope); err != nil {
		return nil, 0, err
	}
	seq, err := recordNumberOf(PhotoIDPrefix, "photo", id)
	if err != nil {
		return nil, 0, err
	}
	args := &arguments{}
	query := `SELECT ` + photoColumns + ` FROM photos WHERE seq = ` + args.add(seq) + ` AND ` + args.scope(scope, "owner_principal_id")
	if forUpdate {
		query += ` FOR UPDATE`
	}
	photo, err := scanPhoto(q.QueryRowContext(ctx, query, args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: no photo %s", ErrNotFound, id)
	}
	return photo, seq, err
}

// Get reads one photo in scope. A deleted photo is not found unless asked for.
func (s *Store) Get(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (*Photo, error) {
	photo, _, err := getRow(ctx, s.db, scope, id, false)
	if err != nil {
		return nil, err
	}
	if photo.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: photo %s is deleted", ErrNotFound, id)
	}
	return photo, nil
}

func encodeStrings(values []string) string {
	if values == nil {
		values = []string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func normalizeTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, fmt.Errorf("%w: a tag is empty", ErrInvalid)
		}
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out, nil
}

// snapshot copies the whole photo, as it stands before a change, into
// photo_revisions, in the change's own transaction. The revision takes the
// photo's owner.
func snapshot(ctx context.Context, tx *sql.Tx, photo *Photo, seq int64, reason string, at int64) error {
	row, err := json.Marshal(photo)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_revisions (owner_principal_id, photo_seq, reason, replaced_at, row_json)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, photo.OwnerPrincipalID, seq, reason, at, string(row))
	return err
}

func validateImport(index int, photo ImportPhoto) error {
	where := fmt.Sprintf("photo %d", index)
	switch {
	case !sha256Pattern.MatchString(photo.Sha256):
		return fmt.Errorf("%w: %s: sha256 must be 64 lowercase hex characters", ErrInvalid, where)
	case strings.TrimSpace(photo.SourceMachine) == "" || strings.TrimSpace(photo.SourcePath) == "":
		return fmt.Errorf("%w: %s: source_machine and source_path are required", ErrInvalid, where)
	case strings.TrimSpace(photo.MediaType) == "":
		return fmt.Errorf("%w: %s: media_type is required", ErrInvalid, where)
	case !knownDateSource(photo.DateSource):
		return fmt.Errorf("%w: %s: unknown date_source %q: use one of %s", ErrInvalid, where, photo.DateSource, strings.Join(DateSources, ", "))
	case photo.DateSource == DateUnknown && photo.TakenAt != 0, photo.DateSource != DateUnknown && photo.TakenAt <= 0:
		return fmt.Errorf("%w: %s: taken_at must be set exactly when date_source is not unknown", ErrInvalid, where)
	case (photo.Latitude == nil) != (photo.Longitude == nil):
		return fmt.Errorf("%w: %s: latitude and longitude come together", ErrInvalid, where)
	case photo.Latitude != nil && (*photo.Latitude < -90 || *photo.Latitude > 90 || *photo.Longitude < -180 || *photo.Longitude > 180):
		return fmt.Errorf("%w: %s: latitude or longitude out of range", ErrInvalid, where)
	case len(photo.ThumbnailJPEG) < 3 || photo.ThumbnailJPEG[0] != 0xFF || photo.ThumbnailJPEG[1] != 0xD8:
		return fmt.Errorf("%w: %s: thumbnail_jpeg must be a JPEG", ErrInvalid, where)
	}
	return nil
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// readThumbnail answers the bytes of a kept thumbnail.
func (s *Store) readThumbnail(ctx context.Context, fileID string) ([]byte, error) {
	reader, err := s.thumbnails.Open(ctx, fileID)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// importPlan is what an import found about one photo before its transaction,
// and the thumbnail file it kept for it.
type importPlan struct {
	// seq is the photo's number: the known photo's, or the one reserved for
	// a new photo.
	seq int64
	// known says the owner already held a photo of this sha256.
	known bool
	// purged and keptFileID are the known photo's state when it was read.
	purged     bool
	keptFileID string
	// thumbnailChanged says a known photo's thumbnail arrived with other
	// bytes than the kept ones.
	thumbnailChanged bool
	// newFileID is the file kept for a new photo, or for a known photo whose
	// thumbnail changed; "" when nothing was kept.
	newFileID string
}

// Import upserts photos by the original's sha256 among the owner's own
// photos. A new hash is created, owned by ownerPrincipalID. A known one has
// its facts (where it is, its date, GPS, size) replaced after a revision when
// any differ, and is skipped when none do; its description and tags are never
// touched by an import. A known photo whose thumbnail arrives with other
// bytes gets a new thumbnail file and has its faces retired, so it is scanned
// again; the same bytes write nothing.
//
// The rows are one transaction: one bad photo writes nothing. Thumbnail files
// are kept before it and cannot share it, so when it fails every file this
// import kept is destroyed, and a thumbnail file the import replaced is
// destroyed only after it commits.
func (s *Store) Import(ctx context.Context, ownerPrincipalID string, request ImportRequest) (*ImportResult, error) {
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(request.Photos) == 0 {
		return nil, fmt.Errorf("%w: photos is empty", ErrInvalid)
	}
	seen := map[string]bool{}
	for index, photo := range request.Photos {
		if err := validateImport(index, photo); err != nil {
			return nil, err
		}
		if seen[photo.Sha256] {
			return nil, fmt.Errorf("%w: photo %d: sha256 %s is in the request twice", ErrInvalid, index, photo.Sha256)
		}
		seen[photo.Sha256] = true
	}

	plans := make([]importPlan, len(request.Photos))
	for index, photo := range request.Photos {
		plan := &plans[index]
		var purgedAt int64
		err := s.db.QueryRowContext(ctx, `SELECT seq, purged_at, thumbnail_file_id FROM photos WHERE owner_principal_id = $1 AND sha256 = $2`,
			ownerPrincipalID, photo.Sha256).Scan(&plan.seq, &purgedAt, &plan.keptFileID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		plan.known, plan.purged = true, purgedAt != 0
		if plan.purged {
			continue
		}
		if plan.keptFileID == "" {
			return nil, fmt.Errorf("photo %s is not purged and has no thumbnail file", photoID(plan.seq))
		}
		// A thumbnail that differs from the one kept is a different picture
		// to a face model: the faces found on the old one are retired below.
		kept, err := s.readThumbnail(ctx, plan.keptFileID)
		if err != nil {
			return nil, fmt.Errorf("reading the kept thumbnail of %s: %w", photoID(plan.seq), err)
		}
		plan.thumbnailChanged = !bytes.Equal(kept, photo.ThumbnailJPEG)
	}

	// Cleanup runs even when ctx is cancelled: a kept file nothing points at
	// must not be left behind.
	cleanupContext := context.WithoutCancel(ctx)
	keptFiles := []string{}
	failWith := func(cause error) error {
		failures := []error{cause}
		for _, fileID := range keptFiles {
			if err := s.thumbnails.Destroy(cleanupContext, fileID); err != nil {
				failures = append(failures, fmt.Errorf("destroying thumbnail file %s, which this failed import kept: %w", fileID, err))
			}
		}
		return errors.Join(failures...)
	}
	for index, photo := range request.Photos {
		plan := &plans[index]
		switch {
		case !plan.known:
			if err := s.db.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('photos', 'seq'))`).Scan(&plan.seq); err != nil {
				return nil, failWith(fmt.Errorf("reserving a photo number: %w", err))
			}
		case !plan.thumbnailChanged:
			continue
		}
		fileID, err := s.thumbnails.Keep(ctx, ownerPrincipalID, photoID(plan.seq), photo.ThumbnailJPEG)
		if err != nil {
			return nil, failWith(fmt.Errorf("keeping the thumbnail of %s: %w", photoID(plan.seq), err))
		}
		keptFiles = append(keptFiles, fileID)
		plan.newFileID = fileID
	}

	result := &ImportResult{IDs: make([]string, 0, len(request.Photos))}
	replacedFiles := []string{}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		at := now()
		for index, photo := range request.Photos {
			plan := plans[index]
			id := photoID(plan.seq)
			if !plan.known {
				if _, err := tx.ExecContext(ctx, `INSERT INTO photos (seq, owner_principal_id, sha256, source_machine, source_path, folder, media_type,
					taken_at, date_source, latitude, longitude, width, height, created_at, updated_at, thumbnail_file_id)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
					plan.seq, ownerPrincipalID, photo.Sha256, photo.SourceMachine, photo.SourcePath, photo.Folder, photo.MediaType,
					photo.TakenAt, photo.DateSource, nullableFloat(photo.Latitude), nullableFloat(photo.Longitude),
					photo.Width, photo.Height, at, at, plan.newFileID); err != nil {
					return conflictOf(err)
				}
				if err := addSighting(ctx, tx, ownerPrincipalID, plan.seq, photo, at); err != nil {
					return err
				}
				result.Created++
				result.IDs = append(result.IDs, id)
				continue
			}
			current, _, err := getRow(ctx, tx, ownership.OwnedBy(ownerPrincipalID), id, true)
			if err != nil {
				return err
			}
			var fileID string
			if err := tx.QueryRowContext(ctx, `SELECT thumbnail_file_id FROM photos WHERE seq = $1`, plan.seq).Scan(&fileID); err != nil {
				return err
			}
			if (current.PurgedAt != 0) != plan.purged || (!plan.purged && fileID != plan.keptFileID) {
				return fmt.Errorf("%w: photo %s changed while this import ran; send it again", ErrConflict, id)
			}
			result.IDs = append(result.IDs, id)
			if err := addSighting(ctx, tx, ownerPrincipalID, plan.seq, photo, at); err != nil {
				return err
			}
			if plan.purged {
				result.Skipped++
				continue
			}
			if plan.thumbnailChanged {
				retired, err := retireFacesOfPhoto(ctx, tx, plan.seq, "", "an import replaced the thumbnail it was found on", at)
				if err != nil {
					return err
				}
				result.FacesRetired += retired
				if _, err := tx.ExecContext(ctx, `UPDATE photos SET thumbnail_file_id = $1 WHERE seq = $2`, plan.newFileID, plan.seq); err != nil {
					return err
				}
				replacedFiles = append(replacedFiles, plan.keptFileID)
			}
			if current.SourceMachine == photo.SourceMachine && current.SourcePath == photo.SourcePath &&
				current.Folder == photo.Folder && current.MediaType == photo.MediaType && current.TakenAt == photo.TakenAt &&
				current.DateSource == photo.DateSource && sameFloat(current.Latitude, photo.Latitude) &&
				sameFloat(current.Longitude, photo.Longitude) && current.Width == photo.Width && current.Height == photo.Height {
				result.Skipped++
				continue
			}
			if err := snapshot(ctx, tx, current, plan.seq, "import", at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE photos SET source_machine = $1, source_path = $2, folder = $3, media_type = $4, taken_at = $5,
				date_source = $6, latitude = $7, longitude = $8, width = $9, height = $10, updated_at = $11 WHERE seq = $12`,
				photo.SourceMachine, photo.SourcePath, photo.Folder, photo.MediaType, photo.TakenAt, photo.DateSource,
				nullableFloat(photo.Latitude), nullableFloat(photo.Longitude), photo.Width, photo.Height, at, plan.seq); err != nil {
				return err
			}
			result.Updated++
		}
		return nil
	})
	if err != nil {
		return nil, failWith(err)
	}
	failures := []error{}
	for _, fileID := range replacedFiles {
		if err := s.thumbnails.Destroy(cleanupContext, fileID); err != nil {
			failures = append(failures, fmt.Errorf("the import was written, but thumbnail file %s, which it replaced, could not be destroyed: %w", fileID, err))
		}
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return result, nil
}

// addSighting records that an import found a photo at a place; the sighting
// takes the photo's owner.
func addSighting(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, photoSeq int64, photo ImportPhoto, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_sightings (owner_principal_id, photo_seq, source_machine, source_path, folder, seen_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (photo_seq, source_machine, source_path) DO NOTHING`,
		ownerPrincipalID, photoSeq, photo.SourceMachine, photo.SourcePath, photo.Folder, at)
	return err
}

// Sighting is one place a photo was found, with what a reader of places
// needs to know about the photo.
type Sighting struct {
	PhotoID       string   `json:"photo_id"`
	SourceMachine string   `json:"source_machine"`
	SourcePath    string   `json:"source_path"`
	Folder        string   `json:"folder"`
	SeenAt        int64    `json:"seen_at"`
	MediaType     string   `json:"media_type"`
	Tags          []string `json:"tags"`
}

// Sightings lists the places live photos in scope were found on one machine
// (every machine when it is ""), ordered by photo and place, for paging.
func (s *Store) Sightings(ctx context.Context, scope ownership.Scope, sourceMachine string, limit, offset int) ([]Sighting, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	args := &arguments{}
	where := []string{"p.deleted_at = 0", args.scope(scope, "s.owner_principal_id")}
	if sourceMachine != "" {
		where = append(where, "s.source_machine = "+args.add(sourceMachine))
	}
	query := `SELECT s.photo_seq, s.source_machine, s.source_path, s.folder, s.seen_at, p.media_type, p.tags::text
		FROM photo_sightings s JOIN photos p ON p.seq = s.photo_seq
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY s.photo_seq, s.source_machine COLLATE "C", s.source_path COLLATE "C" LIMIT ` + args.add(limit) + ` OFFSET ` + args.add(max(offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sightings := []Sighting{}
	for rows.Next() {
		var sighting Sighting
		var seq int64
		var tags string
		if err := rows.Scan(&seq, &sighting.SourceMachine, &sighting.SourcePath, &sighting.Folder, &sighting.SeenAt, &sighting.MediaType, &tags); err != nil {
			return nil, err
		}
		sighting.PhotoID = photoID(seq)
		if err := json.Unmarshal([]byte(tags), &sighting.Tags); err != nil {
			return nil, fmt.Errorf("photo %s tags: %w", sighting.PhotoID, err)
		}
		sightings = append(sightings, sighting)
	}
	return sightings, rows.Err()
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TagCounts answers GET /tags: every tag on a live photo in scope, with how
// many such photos carry it, the most used first and ties by name.
func (s *Store) TagCounts(ctx context.Context, scope ownership.Scope) ([]TagCount, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	args := &arguments{}
	rows, err := s.db.QueryContext(ctx, `SELECT tag.value, COUNT(*) FROM photos, jsonb_array_elements_text(photos.tags) AS tag(value)
		WHERE photos.deleted_at = 0 AND `+args.scope(scope, "photos.owner_principal_id")+`
		GROUP BY tag.value ORDER BY COUNT(*) DESC, tag.value COLLATE "C"`, args.values...)
	if err != nil {
		return nil, fmt.Errorf("count tags: %w", err)
	}
	defer rows.Close()
	counts := []TagCount{}
	for rows.Next() {
		var count TagCount
		if err := rows.Scan(&count.Tag, &count.Photos); err != nil {
			return nil, fmt.Errorf("count tags: %w", err)
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

// Filter selects photos for GET /photos. The zero value lists live photos,
// oldest taken first.
type Filter struct {
	From           int64 // inclusive, on taken_at
	To             int64 // exclusive
	Folder         string
	Query          string // words that must all appear in the description or a tag
	Tag            string
	Person         string // a person_ id confirmed in the photo
	Described      string // "true", "false" or ""
	HasGPS         bool
	Sha256         string // the original file's hash: at most one photo per owner
	IncludeDeleted bool
	ExcludeTags    []string // tags none of the photos may carry
	Limit          int
	Offset         int
}

const (
	defaultLimit = 100
	maximumLimit = 1000
)

// likePatternEscaper makes a word match itself in a LIKE pattern.
var likePatternEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// whereClause turns a filter and a scope into the condition that selects
// their photos, adding its arguments to args. Paging is not part of it.
func (filter Filter) whereClause(scope ownership.Scope, args *arguments) (string, error) {
	where := []string{args.scope(scope, "owner_principal_id")}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	if filter.From > 0 {
		where = append(where, "taken_at >= "+args.add(filter.From))
	}
	if filter.To > 0 {
		where = append(where, "taken_at < "+args.add(filter.To)+" AND taken_at > 0")
	}
	if filter.Folder != "" {
		where = append(where, "folder = "+args.add(filter.Folder))
	}
	for _, word := range strings.Fields(filter.Query) {
		pattern := args.add("%" + likePatternEscaper.Replace(word) + "%")
		where = append(where, "(description ILIKE "+pattern+` ESCAPE '\' OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(photos.tags) AS tag(value) WHERE tag.value ILIKE `+pattern+` ESCAPE '\'))`)
	}
	if filter.Tag != "" {
		where = append(where, "tags @> jsonb_build_array("+args.add(filter.Tag)+"::text)")
	}
	if filter.Person != "" {
		where = append(where, "EXISTS (SELECT 1 FROM photo_people WHERE photo_people.photo_seq = photos.seq AND photo_people.person_id = "+
			args.add(filter.Person)+" AND photo_people.status = 'confirmed')")
	}
	switch filter.Described {
	case "true":
		where = append(where, "description <> ''")
	case "false":
		where = append(where, "description = ''")
	case "":
	default:
		return "", fmt.Errorf("%w: described must be true or false", ErrInvalid)
	}
	if filter.HasGPS {
		where = append(where, "latitude IS NOT NULL")
	}
	if filter.Sha256 != "" {
		where = append(where, "sha256 = "+args.add(strings.ToLower(filter.Sha256)))
	}
	for _, tag := range filter.ExcludeTags {
		where = append(where, "NOT tags @> jsonb_build_array("+args.add(tag)+"::text)")
	}
	return strings.Join(where, " AND "), nil
}

// List answers GET /photos: the photos in scope the filter keeps.
func (s *Store) List(ctx context.Context, scope ownership.Scope, filter Filter) ([]Photo, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	args := &arguments{}
	where, err := filter.whereClause(scope, args)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maximumLimit)
	query := `SELECT ` + photoColumns + ` FROM photos WHERE ` + where +
		` ORDER BY taken_at, seq LIMIT ` + args.add(limit) + ` OFFSET ` + args.add(max(filter.Offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	photos := []Photo{}
	for rows.Next() {
		photo, err := scanPhoto(rows.Scan)
		if err != nil {
			return nil, err
		}
		photos = append(photos, *photo)
	}
	return photos, rows.Err()
}

// Patch changes the fields the patch names on a live photo in scope.
func (s *Store) Patch(ctx context.Context, scope ownership.Scope, id string, patch PhotoPatch) (*Photo, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		photo, seq, err := getRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if photo.DeletedAt != 0 {
			return fmt.Errorf("%w: photo %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(ctx, tx, photo, seq, "update", at); err != nil {
			return err
		}
		if patch.Description != nil {
			photo.Description = strings.TrimSpace(*patch.Description)
		}
		if patch.DescribedBy != nil {
			photo.DescribedBy = strings.TrimSpace(*patch.DescribedBy)
		}
		if photo.Description != "" && photo.DescribedBy == "" {
			return fmt.Errorf("%w: a description needs described_by: who or what wrote it", ErrInvalid)
		}
		if patch.Tags != nil {
			tags, err := normalizeTags(*patch.Tags)
			if err != nil {
				return err
			}
			photo.Tags = tags
		}
		if patch.TakenAt != nil {
			photo.TakenAt = *patch.TakenAt
		}
		if patch.DateSource != nil {
			photo.DateSource = *patch.DateSource
		}
		if !knownDateSource(photo.DateSource) {
			return fmt.Errorf("%w: unknown date_source %q: use one of %s", ErrInvalid, photo.DateSource, strings.Join(DateSources, ", "))
		}
		if (photo.DateSource == DateUnknown) != (photo.TakenAt == 0) || photo.TakenAt < 0 {
			return fmt.Errorf("%w: taken_at must be set exactly when date_source is not unknown", ErrInvalid)
		}
		_, err = tx.ExecContext(ctx, `UPDATE photos SET description = $1, described_by = $2, tags = $3::jsonb, taken_at = $4, date_source = $5, updated_at = $6
			WHERE seq = $7`, photo.Description, photo.DescribedBy, encodeStrings(photo.Tags), photo.TakenAt, photo.DateSource, at, seq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, scope, id, false)
}

// Delete stamps deleted_at; again on a deleted photo changes nothing. The
// thumbnail stays, so a restore brings the photo back whole.
func (s *Store) Delete(ctx context.Context, scope ownership.Scope, id string) error {
	return s.inTransaction(ctx, func(tx *sql.Tx) error {
		photo, seq, err := getRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if photo.DeletedAt != 0 {
			return nil
		}
		at := now()
		if err := snapshot(ctx, tx, photo, seq, "delete", at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE photos SET deleted_at = $1, updated_at = $1 WHERE seq = $2`, at, seq)
		return err
	})
}

// Restore brings a deleted photo back as it was; a purged one cannot come
// back.
func (s *Store) Restore(ctx context.Context, scope ownership.Scope, id string) (*Photo, error) {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		photo, seq, err := getRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if photo.PurgedAt != 0 {
			return fmt.Errorf("%w: photo %s is purged and cannot be restored", ErrConflict, id)
		}
		if photo.DeletedAt == 0 {
			return nil
		}
		at := now()
		if err := snapshot(ctx, tx, photo, seq, "restore", at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE photos SET deleted_at = 0, updated_at = $1 WHERE seq = $2`, at, seq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, scope, id, false)
}

// Purge removes a photo for good, the one exception to nothing being
// hard-deleted, for a picture that must not be kept (an ID, a card, a
// document): it destroys the thumbnail file, blanks the description in the
// row and in every revision, marks the photo deleted and purged, and makes
// every later import of its sha256 by its owner skip it. The row stays, so a
// citation of the id still says what happened. Purging again changes nothing.
//
// The file is destroyed inside the transaction, before it commits: if it
// cannot be destroyed nothing changes and the purge can be sent again, where
// destroying after the commit could leave the bytes of a picture that must
// not be kept with no row pointing at them.
func (s *Store) Purge(ctx context.Context, scope ownership.Scope, id string) error {
	return s.inTransaction(ctx, func(tx *sql.Tx) error {
		photo, seq, err := getRow(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		if photo.PurgedAt != 0 {
			return nil
		}
		var fileID string
		if err := tx.QueryRowContext(ctx, `SELECT thumbnail_file_id FROM photos WHERE seq = $1`, seq).Scan(&fileID); err != nil {
			return err
		}
		at := now()
		deletedAt := photo.DeletedAt
		if deletedAt == 0 {
			deletedAt = at
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photos SET description = '', described_by = '', deleted_at = $1, purged_at = $2, updated_at = $2,
			thumbnail_file_id = '' WHERE seq = $3`, deletedAt, at, seq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_revisions
			SET row_json = jsonb_set(jsonb_set(row_json, '{description}', '""'), '{described_by}', '""') WHERE photo_seq = $1`, seq); err != nil {
			return err
		}
		if fileID != "" {
			if err := s.thumbnails.Destroy(ctx, fileID); err != nil {
				return fmt.Errorf("photo %s was not purged: destroying its thumbnail file %s: %w", id, fileID, err)
			}
		}
		return nil
	})
}

// OpenThumbnail answers a photo's thumbnail bytes, for a deleted photo too,
// so a citation of it keeps drawing. A purged photo's is ErrGone.
func (s *Store) OpenThumbnail(ctx context.Context, scope ownership.Scope, id string) (io.ReadCloser, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	seq, err := recordNumberOf(PhotoIDPrefix, "photo", id)
	if err != nil {
		return nil, err
	}
	args := &arguments{}
	var purgedAt int64
	var fileID string
	err = s.db.QueryRowContext(ctx, `SELECT purged_at, thumbnail_file_id FROM photos WHERE seq = `+args.add(seq)+` AND `+
		args.scope(scope, "owner_principal_id"), args.values...).Scan(&purgedAt, &fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no photo %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if purgedAt != 0 {
		return nil, fmt.Errorf("%w: photo %s was purged: its thumbnail is gone for good", ErrGone, id)
	}
	if fileID == "" {
		return nil, fmt.Errorf("photo %s is not purged and has no thumbnail file", id)
	}
	reader, err := s.thumbnails.Open(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("photo %s: opening its thumbnail file %s: %w", id, fileID, err)
	}
	return reader, nil
}

// Revisions is a photo's history, newest first.
func (s *Store) Revisions(ctx context.Context, scope ownership.Scope, id string, limit, offset int) ([]Revision, error) {
	photo, seq, err := getRow(ctx, s.db, scope, id, false)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision_id, reason, replaced_at, row_json::text FROM photo_revisions WHERE photo_seq = $1
		ORDER BY revision_id DESC LIMIT $2 OFFSET $3`, seq, min(limit, maximumLimit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var row string
		if err := rows.Scan(&revision.RevisionID, &revision.Reason, &revision.ReplacedAt, &row); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(row), &revision.Photo); err != nil {
			return nil, fmt.Errorf("revision %d of %s: %w", revision.RevisionID, id, err)
		}
		// A revision a standalone store wrote names no owner; the photo's
		// owner never changes, so it is the photo's.
		revision.Photo.OwnerPrincipalID = photo.OwnerPrincipalID
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Counts reports what GET /health shows, over the photos in scope.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (Counts, error) {
	if err := checkScope(scope); err != nil {
		return Counts{}, err
	}
	args := &arguments{}
	photosInScope := args.scope(scope, "owner_principal_id")
	var counts Counts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM photos WHERE deleted_at = 0 AND `+photosInScope+`),
		(SELECT COUNT(*) FROM photos WHERE deleted_at = 0 AND description <> '' AND `+photosInScope+`),
		(SELECT COUNT(*) FROM photos WHERE deleted_at = 0 AND latitude IS NOT NULL AND `+photosInScope+`),
		(SELECT COUNT(*) FROM photos WHERE deleted_at <> 0 AND `+photosInScope+`),
		(SELECT COUNT(*) FROM photo_revisions WHERE `+photosInScope+`)`, args.values...).
		Scan(&counts.Photos, &counts.Described, &counts.WithGPS, &counts.Deleted, &counts.Revisions)
	return counts, err
}
