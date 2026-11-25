// Package photostore keeps what is known about the user's own photos: where
// the original is, when it was taken and how sure that is, where (GPS), a
// thumbnail, and a description. The originals stay on the machine they were
// found on; only thumbnails come here.
//
// A photo is cited elsewhere by its id (photo_000001), which kanban-store's
// registry claims, so a timeline item or a note can link one as a chip.
package photostore

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	// ErrConflict is a change a purged photo refuses.
	ErrConflict = errors.New("conflict")
)

// Store owns the database and the thumbnails directory.
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
	if err := os.MkdirAll(filepath.Join(dataDir, "thumbnails"), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "photo-store.db")
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
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addPurgedAt(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addFaceRetirement(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := fillSightings(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

// addPurgedAt adds photos.purged_at to a database made before it existed.
func addPurgedAt(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('photos') WHERE name = 'purged_at'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := db.Exec(`ALTER TABLE photos ADD COLUMN purged_at INTEGER NOT NULL DEFAULT 0`)
	return err
}

// addFaceRetirement adds faces.retired_at and faces.retired_reason to a
// database made before a face could be retired.
func addFaceRetirement(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('faces') WHERE name = 'retired_at'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	for _, statement := range []string{
		`ALTER TABLE faces ADD COLUMN retired_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE faces ADD COLUMN retired_reason TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database and thumbnails live in.
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

func formatID(seq int64) string { return fmt.Sprintf("photo_%06d", seq) }

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ThumbnailPath is where a photo's thumbnail is kept, by the original's hash.
func (s *Store) ThumbnailPath(sha256 string) string {
	return filepath.Join(s.dataDir, "thumbnails", sha256+".jpg")
}

const photoColumns = `id, sha256, source_machine, source_path, folder, media_type, taken_at, date_source,
	latitude, longitude, width, height, description, described_by, tags, created_at, updated_at, deleted_at, purged_at`

type queryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func scanPhoto(scan func(...any) error) (*Photo, error) {
	var photo Photo
	var latitude, longitude sql.NullFloat64
	var tags string
	if err := scan(&photo.ID, &photo.Sha256, &photo.SourceMachine, &photo.SourcePath, &photo.Folder, &photo.MediaType,
		&photo.TakenAt, &photo.DateSource, &latitude, &longitude, &photo.Width, &photo.Height, &photo.Description,
		&photo.DescribedBy, &tags, &photo.CreatedAt, &photo.UpdatedAt, &photo.DeletedAt, &photo.PurgedAt); err != nil {
		return nil, err
	}
	if latitude.Valid && longitude.Valid {
		photo.Latitude, photo.Longitude = &latitude.Float64, &longitude.Float64
	}
	if err := json.Unmarshal([]byte(tags), &photo.Tags); err != nil {
		return nil, fmt.Errorf("photo %s has tags that are not a JSON array: %w", photo.ID, err)
	}
	return &photo, nil
}

func getRow(q queryer, id string) (*Photo, error) {
	photo, err := scanPhoto(q.QueryRow(`SELECT `+photoColumns+` FROM photos WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no photo %s", ErrNotFound, id)
	}
	return photo, err
}

// Get reads one photo. A deleted photo is not found unless asked for.
func (s *Store) Get(id string, includeDeleted bool) (*Photo, error) {
	photo, err := getRow(s.db, id)
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

// snapshot copies the whole current row into photo_revisions before a change,
// in the change's own transaction.
func snapshot(tx *sql.Tx, id, reason string, at int64) error {
	photo, err := getRow(tx, id)
	if err != nil {
		return err
	}
	row, err := json.Marshal(photo)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO photo_revisions (photo_id, reason, replaced_at, row_json) VALUES (?,?,?,?)`, id, reason, at, string(row))
	return err
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

// Import upserts photos by the original's sha256. A new hash is created. A
// known one has its facts (where it is, its date, GPS, size, thumbnail)
// replaced after a revision when any differ, and is skipped when none do; its
// description and tags are never touched by an import. The rows are one
// transaction: one bad photo writes nothing. Thumbnails are written after the
// commit, each to a temporary file renamed into place.
func (s *Store) Import(request ImportRequest) (*ImportResult, error) {
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
	// A thumbnail that differs from the one kept is a different picture to
	// a face model: the faces found on the old one are retired below.
	thumbnailChanged := map[string]bool{}
	for _, photo := range request.Photos {
		kept, err := os.ReadFile(s.ThumbnailPath(photo.Sha256))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the kept thumbnail of %s: %w", photo.Sha256, err)
		}
		thumbnailChanged[photo.Sha256] = !bytes.Equal(kept, photo.ThumbnailJPEG)
	}
	result := &ImportResult{IDs: make([]string, 0, len(request.Photos))}
	purged := map[string]bool{}
	err := s.inTransaction(func(tx *sql.Tx) error {
		at := now()
		for _, photo := range request.Photos {
			var existing string
			err := tx.QueryRow(`SELECT id FROM photos WHERE sha256 = ?`, photo.Sha256).Scan(&existing)
			if errors.Is(err, sql.ErrNoRows) {
				var maxSeq sql.NullInt64
				if err := tx.QueryRow(`SELECT MAX(seq) FROM photos`).Scan(&maxSeq); err != nil {
					return err
				}
				seq := maxSeq.Int64 + 1
				id := formatID(seq)
				if _, err := tx.Exec(`INSERT INTO photos (id, seq, sha256, source_machine, source_path, folder, media_type,
					taken_at, date_source, latitude, longitude, width, height, created_at, updated_at)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					id, seq, photo.Sha256, photo.SourceMachine, photo.SourcePath, photo.Folder, photo.MediaType,
					photo.TakenAt, photo.DateSource, nullableFloat(photo.Latitude), nullableFloat(photo.Longitude),
					photo.Width, photo.Height, at, at); err != nil {
					return err
				}
				if err := addSighting(tx, id, photo, at); err != nil {
					return err
				}
				result.Created++
				result.IDs = append(result.IDs, id)
				continue
			}
			if err != nil {
				return err
			}
			current, err := getRow(tx, existing)
			if err != nil {
				return err
			}
			result.IDs = append(result.IDs, existing)
			if err := addSighting(tx, existing, photo, at); err != nil {
				return err
			}
			if current.PurgedAt != 0 {
				purged[photo.Sha256] = true
				result.Skipped++
				continue
			}
			if thumbnailChanged[photo.Sha256] {
				retired, err := retireFacesOfPhoto(tx, existing, "", "an import replaced the thumbnail it was found on", at)
				if err != nil {
					return err
				}
				result.FacesRetired += retired
			}
			if current.SourceMachine == photo.SourceMachine && current.SourcePath == photo.SourcePath &&
				current.Folder == photo.Folder && current.MediaType == photo.MediaType && current.TakenAt == photo.TakenAt &&
				current.DateSource == photo.DateSource && sameFloat(current.Latitude, photo.Latitude) &&
				sameFloat(current.Longitude, photo.Longitude) && current.Width == photo.Width && current.Height == photo.Height {
				result.Skipped++
				continue
			}
			if err := snapshot(tx, existing, "import", at); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE photos SET source_machine=?, source_path=?, folder=?, media_type=?, taken_at=?,
				date_source=?, latitude=?, longitude=?, width=?, height=?, updated_at=? WHERE id=?`,
				photo.SourceMachine, photo.SourcePath, photo.Folder, photo.MediaType, photo.TakenAt, photo.DateSource,
				nullableFloat(photo.Latitude), nullableFloat(photo.Longitude), photo.Width, photo.Height, at, existing); err != nil {
				return err
			}
			result.Updated++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, photo := range request.Photos {
		if purged[photo.Sha256] {
			continue
		}
		if err := writeFileAtomically(s.ThumbnailPath(photo.Sha256), photo.ThumbnailJPEG); err != nil {
			return nil, fmt.Errorf("thumbnail of %s: %w", photo.Sha256, err)
		}
	}
	return result, nil
}

// fillSightings gives an empty photo_sightings every place the store has
// recorded a photo: each photo's current place, and every earlier one its
// revisions kept.
func fillSightings(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM photo_sightings`).Scan(&count); err != nil || count > 0 {
		return err
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO photo_sightings (photo_id, source_machine, source_path, folder, seen_at)
		SELECT photo_id, json_extract(row_json, '$.source_machine'), json_extract(row_json, '$.source_path'),
			COALESCE(json_extract(row_json, '$.folder'), ''), MIN(replaced_at)
		FROM photo_revisions
		WHERE json_extract(row_json, '$.source_path') IS NOT NULL AND json_extract(row_json, '$.source_path') != ''
		GROUP BY photo_id, json_extract(row_json, '$.source_machine'), json_extract(row_json, '$.source_path');
		INSERT OR IGNORE INTO photo_sightings (photo_id, source_machine, source_path, folder, seen_at)
		SELECT id, source_machine, source_path, folder, updated_at FROM photos WHERE source_path != '';`)
	return err
}

// addSighting records that an import found a photo at a place.
func addSighting(tx *sql.Tx, photoID string, photo ImportPhoto, at int64) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO photo_sightings (photo_id, source_machine, source_path, folder, seen_at) VALUES (?,?,?,?,?)`,
		photoID, photo.SourceMachine, photo.SourcePath, photo.Folder, at)
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

// Sightings lists the places live photos were found on one machine (every
// machine when it is ""), ordered by photo and place, for paging.
func (s *Store) Sightings(sourceMachine string, limit, offset int) ([]Sighting, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.Query(`SELECT s.photo_id, s.source_machine, s.source_path, s.folder, s.seen_at, p.media_type, p.tags
		FROM photo_sightings s JOIN photos p ON p.id = s.photo_id
		WHERE p.deleted_at = 0 AND (? = '' OR s.source_machine = ?)
		ORDER BY s.photo_id, s.source_machine, s.source_path LIMIT ? OFFSET ?`, sourceMachine, sourceMachine, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sightings := []Sighting{}
	for rows.Next() {
		var sighting Sighting
		var tags string
		if err := rows.Scan(&sighting.PhotoID, &sighting.SourceMachine, &sighting.SourcePath, &sighting.Folder, &sighting.SeenAt, &sighting.MediaType, &tags); err != nil {
			return nil, err
		}
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

func writeFileAtomically(path string, content []byte) error {
	temporary := fmt.Sprintf("%s.partial-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// TagCounts answers GET /tags: every tag on a live photo, with how many live
// photos carry it, the most used first and ties by name.
func (s *Store) TagCounts() ([]TagCount, error) {
	rows, err := s.db.Query(`SELECT tag.value, COUNT(*) FROM photos, json_each(photos.tags) AS tag
		WHERE photos.deleted_at = 0 GROUP BY tag.value ORDER BY COUNT(*) DESC, tag.value`)
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
	Query          string // words that must all appear in the description or tags
	Tag            string
	Person         string // a person_ id confirmed in the photo
	Described      string // "true", "false" or ""
	HasGPS         bool
	Sha256         string // the original file's hash: at most one photo
	IncludeDeleted bool
	ExcludeTags    []string // tags none of the photos may carry
	Limit          int
	Offset         int
}

const (
	defaultLimit = 100
	maximumLimit = 1000
)

// List answers GET /photos.
// whereClause turns a filter into the SQL that selects its photos: "" or
// " WHERE …", and its arguments. Paging is not part of it.
func (filter Filter) whereClause() (string, []any, error) {
	where := []string{}
	args := []any{}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	if filter.From > 0 {
		where = append(where, "taken_at >= ?")
		args = append(args, filter.From)
	}
	if filter.To > 0 {
		where = append(where, "taken_at < ? AND taken_at > 0")
		args = append(args, filter.To)
	}
	if filter.Folder != "" {
		where = append(where, "folder = ?")
		args = append(args, filter.Folder)
	}
	for _, word := range strings.Fields(filter.Query) {
		where = append(where, "(description LIKE ? ESCAPE '\\' OR tags LIKE ? ESCAPE '\\')")
		pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(word) + "%"
		args = append(args, pattern, pattern)
	}
	if filter.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(photos.tags) WHERE value = ?)")
		args = append(args, filter.Tag)
	}
	if filter.Person != "" {
		where = append(where, "EXISTS (SELECT 1 FROM photo_people WHERE photo_people.photo_id = photos.id AND photo_people.person_id = ? AND photo_people.status = 'confirmed')")
		args = append(args, filter.Person)
	}
	switch filter.Described {
	case "true":
		where = append(where, "description != ''")
	case "false":
		where = append(where, "description = ''")
	case "":
	default:
		return "", nil, fmt.Errorf("%w: described must be true or false", ErrInvalid)
	}
	if filter.HasGPS {
		where = append(where, "latitude IS NOT NULL")
	}
	if filter.Sha256 != "" {
		where = append(where, "sha256 = ?")
		args = append(args, strings.ToLower(filter.Sha256))
	}
	for _, tag := range filter.ExcludeTags {
		where = append(where, "NOT EXISTS (SELECT 1 FROM json_each(photos.tags) WHERE value = ?)")
		args = append(args, tag)
	}
	if len(where) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(where, " AND "), args, nil
}

func (s *Store) List(filter Filter) ([]Photo, error) {
	whereSQL, args, err := filter.whereClause()
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + photoColumns + ` FROM photos` + whereSQL
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maximumLimit)
	query += ` ORDER BY taken_at, seq LIMIT ? OFFSET ?`
	args = append(args, limit, max(filter.Offset, 0))
	rows, err := s.db.Query(query, args...)
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

// Patch changes the fields the patch names on a live photo.
func (s *Store) Patch(id string, patch PhotoPatch) (*Photo, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		photo, err := getRow(tx, id)
		if err != nil {
			return err
		}
		if photo.DeletedAt != 0 {
			return fmt.Errorf("%w: photo %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(tx, id, "update", at); err != nil {
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
		_, err = tx.Exec(`UPDATE photos SET description=?, described_by=?, tags=?, taken_at=?, date_source=?, updated_at=? WHERE id=?`,
			photo.Description, photo.DescribedBy, encodeStrings(photo.Tags), photo.TakenAt, photo.DateSource, at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// Delete stamps deleted_at; again on a deleted photo changes nothing. The
// thumbnail stays, so a restore brings the photo back whole.
func (s *Store) Delete(id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		photo, err := getRow(tx, id)
		if err != nil {
			return err
		}
		if photo.DeletedAt != 0 {
			return nil
		}
		at := now()
		if err := snapshot(tx, id, "delete", at); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE photos SET deleted_at=?, updated_at=? WHERE id=?`, at, at, id)
		return err
	})
}

// Restore brings a deleted photo back as it was; a purged one cannot come
// back.
func (s *Store) Restore(id string) (*Photo, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		photo, err := getRow(tx, id)
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
		if err := snapshot(tx, id, "restore", at); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE photos SET deleted_at=0, updated_at=? WHERE id=?`, at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// Purge removes a photo for good, the one exception to nothing being
// hard-deleted, for a picture that must not be kept (an ID, a card, a
// document): it deletes the thumbnail file, blanks the description in the row
// and in every revision, marks the photo deleted and purged, and makes every
// later import skip it. The row stays, so a citation of the id still says
// what happened. Purging again changes nothing.
func (s *Store) Purge(id string) error {
	var sha string
	err := s.inTransaction(func(tx *sql.Tx) error {
		photo, err := getRow(tx, id)
		if err != nil {
			return err
		}
		sha = photo.Sha256
		if photo.PurgedAt != 0 {
			return nil
		}
		at := now()
		deletedAt := photo.DeletedAt
		if deletedAt == 0 {
			deletedAt = at
		}
		if _, err := tx.Exec(`UPDATE photos SET description='', described_by='', deleted_at=?, purged_at=?, updated_at=? WHERE id=?`,
			deletedAt, at, at, id); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE photo_revisions SET row_json = json_set(row_json, '$.description', '', '$.described_by', '')
			WHERE photo_id = ?`, id)
		return err
	})
	if err != nil {
		return err
	}
	if err := os.Remove(s.ThumbnailPath(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("photo %s is purged but its thumbnail is still on disk: %w", id, err)
	}
	return nil
}

// Revisions is a photo's history, newest first.
func (s *Store) Revisions(id string, limit, offset int) ([]Revision, error) {
	if _, err := getRow(s.db, id); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := s.db.Query(`SELECT revision_id, reason, replaced_at, row_json FROM photo_revisions WHERE photo_id = ?
		ORDER BY revision_id DESC LIMIT ? OFFSET ?`, id, min(limit, maximumLimit), max(offset, 0))
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
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Counts reports what GET /health shows.
func (s *Store) Counts() (Counts, error) {
	var counts Counts
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM photos WHERE deleted_at = 0),
		(SELECT COUNT(*) FROM photos WHERE deleted_at = 0 AND description != ''),
		(SELECT COUNT(*) FROM photos WHERE deleted_at = 0 AND latitude IS NOT NULL),
		(SELECT COUNT(*) FROM photos WHERE deleted_at != 0),
		(SELECT COUNT(*) FROM photo_revisions)`).
		Scan(&counts.Photos, &counts.Described, &counts.WithGPS, &counts.Deleted, &counts.Revisions)
	return counts, err
}
