package filestore

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// FileIDPrefix is the type part of a file's public id: file_000001.
const FileIDPrefix = "file"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them. The bytes are not in it: they are files
// under the data directory New is given.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "files", Role: "files_module", Migrations: migrations}, nil
}

var (
	// ErrNotFound: no live file has that id.
	ErrNotFound = errors.New("not found")
	// ErrFileTooLarge: the upload passed the limit. Nothing was kept.
	ErrFileTooLarge = errors.New("file is larger than this store accepts")
	// ErrEmptyFile: the upload had no bytes.
	ErrEmptyFile = errors.New("file is empty")
)

// Store is the records and the blob directory together. They are one thing:
// a row without its blob is a file that cannot be read, so every path that
// adds or removes either holds blobMutex.
//
// The records are in the module's Postgres schema; the bytes are on disk
// under dataDir/blobs, named by their SHA-256. One process holds a data
// directory: blobMutex is this process's, so two servers sharing a directory
// could purge a blob the other is naming.
type Store struct {
	db      *sql.DB
	dataDir string
	// blobMutex makes "is this blob still named by a row?" and the unlink that
	// follows one step against an upload of the same bytes finishing in
	// between, which would leave a fresh row naming a blob just removed.
	blobMutex sync.Mutex
}

// New wraps a pool opened on the module (postgresstorage.Open), migrated, and
// the directory the bytes are kept in, which it makes if it is missing.
func New(db *sql.DB, dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("file-store: no data directory to keep the bytes in")
	}
	for _, dir := range []string{dataDir, filepath.Join(dataDir, "blobs"), filepath.Join(dataDir, "blobs", "incoming")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// An upload that died mid-copy left a partial file nobody names.
	leftovers, err := filepath.Glob(filepath.Join(dataDir, "blobs", "incoming", "*"))
	if err != nil {
		return nil, err
	}
	for _, leftover := range leftovers {
		if err := os.Remove(leftover); err != nil {
			return nil, fmt.Errorf("remove the partial upload %s: %w", leftover, err)
		}
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

func (s *Store) DataDir() string { return s.dataDir }

func formatID(seq int64) string { return postgresstorage.FormatRecordID(FileIDPrefix, seq) }

// blobPath is where the bytes with this hash live. Two levels of fan-out keep
// any one directory small.
func (s *Store) blobPath(sha string) string {
	return filepath.Join(s.dataDir, "blobs", sha[0:2], sha[2:4], sha)
}

// NewFile is what an upload says about itself; the store works out the rest.
type NewFile struct {
	ContentType           string
	Filename              string
	OwnerService          string
	OwnerRef              string
	UploadedByPrincipalID string
}

// Put reads content to its end, keeps it, and records the file. It reads at
// most maximumBytes+1: one byte past the limit is enough to know, and the
// partial copy is removed.
func (s *Store) Put(content io.Reader, maximumBytes int64, file NewFile) (*File, error) {
	incoming, err := os.CreateTemp(filepath.Join(s.dataDir, "blobs", "incoming"), "upload-*")
	if err != nil {
		return nil, fmt.Errorf("create the incoming file: %w", err)
	}
	incomingPath := incoming.Name()
	kept := false
	defer func() {
		incoming.Close()
		if !kept {
			os.Remove(incomingPath)
		}
	}()

	hasher := sha256.New()
	head := &headCapture{limit: 512}
	size, err := io.Copy(io.MultiWriter(incoming, hasher, head), io.LimitReader(content, maximumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the upload: %w", err)
	}
	if size > maximumBytes {
		return nil, fmt.Errorf("%w: the limit is %d bytes", ErrFileTooLarge, maximumBytes)
	}
	if size == 0 {
		return nil, ErrEmptyFile
	}
	// The bytes must be on disk before a row says they are.
	if err := incoming.Sync(); err != nil {
		return nil, fmt.Errorf("flush the upload to disk: %w", err)
	}
	if err := incoming.Close(); err != nil {
		return nil, fmt.Errorf("close the upload: %w", err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	s.blobMutex.Lock()
	defer s.blobMutex.Unlock()
	destination := s.blobPath(sha)
	if _, statErr := os.Stat(destination); errors.Is(statErr, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return nil, fmt.Errorf("create the blob directory: %w", err)
		}
		if err := os.Rename(incomingPath, destination); err != nil {
			return nil, fmt.Errorf("keep the blob: %w", err)
		}
		kept = true
	} else if statErr != nil {
		return nil, fmt.Errorf("look for an existing blob: %w", statErr)
	}
	// Otherwise these bytes are already here, and the copy just made is dropped.

	stored := &File{
		SHA256: sha, SizeBytes: size,
		ContentType: file.ContentType, DetectedContentType: http.DetectContentType(head.bytes),
		Filename: file.Filename, OwnerService: file.OwnerService, OwnerRef: file.OwnerRef,
		UploadedByPrincipalID: file.UploadedByPrincipalID, CreatedAt: time.Now().Unix(),
	}
	// The identity column gives each number out once, even to an insert that
	// then fails: a gap in the numbers is harmless, and a reused id is not.
	var seq int64
	if err := s.db.QueryRow(`INSERT INTO files
		(sha256, size_bytes, content_type, detected_content_type, filename, owner_service, owner_ref, uploaded_by_principal_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING seq`,
		stored.SHA256, stored.SizeBytes, stored.ContentType, stored.DetectedContentType, stored.Filename,
		stored.OwnerService, stored.OwnerRef, stored.UploadedByPrincipalID, stored.CreatedAt).Scan(&seq); err != nil {
		// The blob may now be named by no row. It is harmless, it is removed the
		// next time a row with its hash is purged, and removing it here could
		// take the bytes of an older row that names the same hash.
		return nil, fmt.Errorf("record the file: %w", err)
	}
	stored.ID = formatID(seq)
	return stored, nil
}

// headCapture keeps the first bytes written to it, for content sniffing.
type headCapture struct {
	bytes []byte
	limit int
}

func (h *headCapture) Write(p []byte) (int, error) {
	if room := h.limit - len(h.bytes); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		h.bytes = append(h.bytes, p[:room]...)
	}
	return len(p), nil
}

const fileColumns = `seq, sha256, size_bytes, content_type, detected_content_type, filename, owner_service, owner_ref, uploaded_by_principal_id, created_at, deleted_at`

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	var file File
	var seq int64
	err := row.Scan(&seq, &file.SHA256, &file.SizeBytes, &file.ContentType, &file.DetectedContentType, &file.Filename,
		&file.OwnerService, &file.OwnerRef, &file.UploadedByPrincipalID, &file.CreatedAt, &file.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	file.ID = formatID(seq)
	return &file, nil
}

// seqOf is the number in a file id. An id that is not the exact form a number
// makes names no file.
func seqOf(id string) (int64, error) {
	seq, err := postgresstorage.ParseRecordID(FileIDPrefix, id)
	if err != nil || formatID(seq) != id {
		return 0, ErrNotFound
	}
	return seq, nil
}

// Get reads one file. A deleted file is not found unless includeDeleted.
func (s *Store) Get(id string, includeDeleted bool) (*File, error) {
	seq, err := seqOf(id)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + fileColumns + ` FROM files WHERE seq = $1`
	if !includeDeleted {
		query += ` AND deleted_at = 0`
	}
	return scanFile(s.db.QueryRow(query, seq))
}

// OpenContent opens a live file's bytes. The caller closes it.
func (s *Store) OpenContent(file *File) (*os.File, error) {
	content, err := os.Open(s.blobPath(file.SHA256))
	if err != nil {
		// A row whose blob is gone is this store's own corruption, not a
		// missing file, and is reported as what it is.
		return nil, fmt.Errorf("file %s names blob %s, which cannot be opened: %w", file.ID, file.SHA256, err)
	}
	return content, nil
}

// Filter narrows a listing. Empty fields keep everything.
type Filter struct {
	OwnerService   string
	OwnerRef       string
	SHA256         string
	IncludeDeleted bool
	Limit          int
	Offset         int
}

// List answers files oldest first.
func (s *Store) List(filter Filter) ([]*File, error) {
	query := `SELECT ` + fileColumns + ` FROM files WHERE true`
	args := []any{}
	for _, condition := range []struct{ column, value string }{
		{"owner_service", filter.OwnerService}, {"owner_ref", filter.OwnerRef}, {"sha256", filter.SHA256},
	} {
		if condition.value != "" {
			args = append(args, condition.value)
			query += fmt.Sprintf(` AND %s = $%d`, condition.column, len(args))
		}
	}
	if !filter.IncludeDeleted {
		query += ` AND deleted_at = 0`
	}
	args = append(args, filter.Limit, filter.Offset)
	query += fmt.Sprintf(` ORDER BY seq ASC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []*File{}
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// Delete takes a file away reversibly: the row and the bytes stay.
func (s *Store) Delete(id string) error {
	return s.stampDeletedAt(id, 0, time.Now().Unix())
}

// Restore brings a deleted file back exactly as it was.
func (s *Store) Restore(id string) error {
	return s.stampDeletedAt(id, -1, 0)
}

// stampDeletedAt moves deleted_at between 0 and a time. from is the state the
// row must be in: 0 for live, -1 for "deleted at any time".
func (s *Store) stampDeletedAt(id string, from, to int64) error {
	condition := `deleted_at = 0`
	if from != 0 {
		condition = `deleted_at != 0`
	}
	seq, err := seqOf(id)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE files SET deleted_at = $1 WHERE seq = $2 AND `+condition, to, seq)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// Purge destroys a file's row, live or deleted, and its bytes with it when no
// other row names them. It is the only thing here that cannot be undone.
func (s *Store) Purge(id string) error {
	s.blobMutex.Lock()
	defer s.blobMutex.Unlock()
	file, err := s.Get(id, true)
	if err != nil {
		return err
	}
	seq, err := seqOf(file.ID)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM files WHERE seq = $1`, seq); err != nil {
		return err
	}
	var stillNamed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE sha256 = $1`, file.SHA256).Scan(&stillNamed); err != nil {
		return err
	}
	if stillNamed > 0 {
		return nil
	}
	if err := os.Remove(s.blobPath(file.SHA256)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("file %s is purged, and its blob %s could not be removed: %w", id, file.SHA256, err)
	}
	return nil
}

// Health counts what is stored.
func (s *Store) Health() (*Health, error) {
	health := &Health{Status: "ok"}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE deleted_at = 0`).Scan(&health.Files); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM (SELECT sha256, MAX(size_bytes) AS size_bytes FROM files GROUP BY sha256) AS blobs`).
		Scan(&health.Blobs, &health.BlobBytes); err != nil {
		return nil, err
	}
	return health, nil
}
