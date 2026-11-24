package filestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneColumns are the columns of each table the standalone file-store
// kept in its SQLite file.
var standaloneColumns = map[string][]string{
	"files": {"id", "seq", "sha256", "size_bytes", "content_type", "detected_content_type", "filename", "owner_service", "owner_ref",
		"uploaded_by_principal_id", "created_at", "deleted_at"},
	"file_sequence":    {"only_row", "last_seq"},
	"service_settings": {"key", "value"},
}

var sha256Shape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BlobCarry is how an import puts a standalone store's bytes under this
// store's data directory.
type BlobCarry string

const (
	// CopyBlobs writes a copy of each blob. The standalone directory is only
	// read.
	CopyBlobs BlobCarry = "copy"
	// LinkBlobs makes a hard link to each blob, which takes no room but needs
	// both directories on one file system. A blob is never written after it
	// is made, and a purge here only unlinks this store's name for it, so the
	// two stores still cannot change each other's bytes.
	LinkBlobs BlobCarry = "link"
)

// ImportCounts says what an import copied.
type ImportCounts struct {
	Files        int64 `json:"files"`
	DeletedFiles int64 `json:"deleted_files"`
	// LastFileNumber is the highest number the standalone store handed out,
	// purged files included; the module numbers on after it.
	LastFileNumber int64 `json:"last_file_number"`
	// Blobs is how many distinct byte strings were carried, and BlobBytes
	// their size. BlobsAlreadyHere were in the data directory already, with
	// the right bytes.
	Blobs            int64 `json:"blobs"`
	BlobBytes        int64 `json:"blob_bytes"`
	BlobsAlreadyHere int64 `json:"blobs_already_here"`
	// StoredSettings are the standalone store's own setting rows
	// (maximum_file_bytes, inline_content_types). This server keeps those
	// settings itself, so the import does not write them; the caller says
	// how to set them.
	StoredSettings map[string]string `json:"stored_settings"`
}

// ImportStandaloneDatabase copies every file record from the SQLite file of a
// standalone file-store into the module behind db, keeping ids, owners and
// times, soft-deleted rows included, and carries every blob a row names from
// standaloneBlobsDirectory (the standalone data directory's blobs/) into the
// store's own data directory, checking each blob's SHA-256 and size against
// the rows that name it.
//
// The rows go in one transaction, which commits only after every blob is in
// place, so a missing or damaged blob leaves the module empty. Blobs carried
// before such a failure stay on disk, named by no row; a later import finds
// them and checks them again. It refuses a module that already holds a row.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, store *Store, sqlitePath, standaloneBlobsDirectory string, carry BlobCarry) (ImportCounts, error) {
	counts := ImportCounts{StoredSettings: map[string]string{}}
	if carry != CopyBlobs && carry != LinkBlobs {
		return counts, fmt.Errorf("blobs are carried by %q or %q, not %q", CopyBlobs, LinkBlobs, carry)
	}
	if info, err := os.Stat(standaloneBlobsDirectory); err != nil || !info.IsDir() {
		return counts, fmt.Errorf("the standalone blobs directory %s: %v", standaloneBlobsDirectory, err)
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	for table, want := range standaloneColumns {
		if err := standalonesqlite.CheckColumns(ctx, source, "file-store", table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	if err := readStandaloneSettings(ctx, source, counts.StoredSettings); err != nil {
		return counts, err
	}
	if err := source.QueryRowContext(ctx, `SELECT MAX(last_seq, (SELECT COALESCE(MAX(seq), 0) FROM files)) FROM file_sequence`).Scan(&counts.LastFileNumber); err != nil {
		return counts, fmt.Errorf("reading the last file number the standalone store handed out: %w", err)
	}

	// Every blob a row names, and the size the rows say it has.
	blobSizes := map[string]int64{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "files", "files"); err != nil {
			return err
		}
		var err error
		counts.Files, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "files", Columns: standaloneColumns["files"], OrderBy: "seq",
			Insert: `INSERT INTO files (seq, sha256, size_bytes, content_type, detected_content_type, filename, owner_service, owner_ref,
				uploaded_by_principal_id, created_at, deleted_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			Convert: func(row []any) ([]any, error) {
				id, err := standalonesqlite.Text("id", row[0])
				if err != nil {
					return nil, err
				}
				seq, err := standalonesqlite.Integer("seq", row[1])
				if err != nil {
					return nil, err
				}
				if id != formatID(seq) {
					return nil, fmt.Errorf("file %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", id, seq, formatID(seq))
				}
				sha, err := standalonesqlite.Text("sha256", row[2])
				if err != nil {
					return nil, err
				}
				if !sha256Shape.MatchString(sha) {
					return nil, fmt.Errorf("sha256 %q is not 64 lower-case hex characters", sha)
				}
				size, err := standalonesqlite.Integer("size_bytes", row[3])
				if err != nil {
					return nil, err
				}
				if known, seen := blobSizes[sha]; seen && known != size {
					return nil, fmt.Errorf("rows name blob %s with sizes %d and %d", sha, known, size)
				}
				blobSizes[sha] = size
				deletedAt, err := standalonesqlite.Integer("deleted_at", row[11])
				if err != nil {
					return nil, err
				}
				if deletedAt != 0 {
					counts.DeletedFiles++
				}
				return row[1:], nil
			},
		})
		if err != nil {
			return err
		}
		for sha, size := range blobSizes {
			alreadyHere, err := store.carryBlob(filepath.Join(standaloneBlobsDirectory, sha[0:2], sha[2:4], sha), sha, size, carry)
			if err != nil {
				return err
			}
			counts.Blobs++
			counts.BlobBytes += size
			if alreadyHere {
				counts.BlobsAlreadyHere++
			}
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "files", "seq", counts.LastFileNumber)
	})
	if err != nil {
		return ImportCounts{StoredSettings: counts.StoredSettings}, err
	}
	return counts, nil
}

func readStandaloneSettings(ctx context.Context, source *sql.DB, into map[string]string) error {
	rows, err := source.QueryContext(ctx, `SELECT key, value FROM service_settings ORDER BY key`)
	if err != nil {
		return fmt.Errorf("reading the standalone store's settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		into[key] = value
	}
	return rows.Err()
}

// carryBlob puts the blob at sourcePath under this store's name for sha,
// unless the right bytes are there already, and checks what it put there. It
// answers whether the blob was there already.
func (s *Store) carryBlob(sourcePath, sha string, size int64, carry BlobCarry) (bool, error) {
	destination := s.blobPath(sha)
	if _, err := os.Stat(destination); err == nil {
		return true, checkBlob(destination, sha, size)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("look for blob %s here: %w", sha, err)
	}
	if err := checkBlob(sourcePath, sha, size); err != nil {
		return false, fmt.Errorf("the standalone store's blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return false, fmt.Errorf("create the blob directory: %w", err)
	}
	switch carry {
	case LinkBlobs:
		if err := os.Link(sourcePath, destination); err != nil {
			return false, fmt.Errorf("link blob %s: %w", sha, err)
		}
	case CopyBlobs:
		if err := copyFileThroughIncoming(sourcePath, destination, filepath.Join(s.dataDir, "blobs", "incoming")); err != nil {
			return false, fmt.Errorf("copy blob %s: %w", sha, err)
		}
	}
	return false, checkBlob(destination, sha, size)
}

// copyFileThroughIncoming copies source to a file in incoming, flushes it,
// and renames it to destination, so destination never holds part of a blob.
func copyFileThroughIncoming(source, destination, incoming string) error {
	reader, err := os.Open(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	writer, err := os.CreateTemp(incoming, "import-*")
	if err != nil {
		return err
	}
	kept := false
	defer func() {
		writer.Close()
		if !kept {
			os.Remove(writer.Name())
		}
	}()
	if _, err := io.Copy(writer, reader); err != nil {
		return err
	}
	if err := writer.Sync(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := os.Rename(writer.Name(), destination); err != nil {
		return err
	}
	kept = true
	return nil
}

// checkBlob refuses a blob whose bytes are not the hash its name says, or not
// the size its rows say.
func checkBlob(path, sha string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("blob %s: %w", sha, err)
	}
	defer file.Close()
	hasher := sha256.New()
	read, err := io.Copy(hasher, file)
	if err != nil {
		return fmt.Errorf("read blob %s: %w", sha, err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != sha {
		return fmt.Errorf("blob %s at %s holds bytes whose SHA-256 is %s", sha, path, got)
	}
	if read != size {
		return fmt.Errorf("blob %s at %s is %d bytes, and its rows say %d", sha, path, read, size)
	}
	return nil
}
