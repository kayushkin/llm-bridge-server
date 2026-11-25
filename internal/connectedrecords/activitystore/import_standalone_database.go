package activitystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the import's errors.
const standaloneStoreName = "activity-store"

// standaloneAttachmentFolder is the folder of the standalone data directory
// that held attachment files; a row's stored_path begins with it.
const standaloneAttachmentFolder = "attachments"

// StandaloneImportCounts says how many rows of each table
// ImportStandaloneDatabase copied, and how many attachment files it handed
// file-store. (ImportCounts is what an import batch did, on the wire.)
type StandaloneImportCounts struct {
	Imports          int64 `json:"imports"`
	Activities       int64 `json:"activities"`
	Sightings        int64 `json:"sightings"`
	Attachments      int64 `json:"attachments"`
	AttachmentFiles  int64 `json:"attachment_files"`
	Revisions        int64 `json:"revisions"`
	PurgedActivities int64 `json:"purged_activities"`
}

// Summary is the counts in one line, and nothing of the records.
func (counts StandaloneImportCounts) Summary() string {
	return fmt.Sprintf("imports=%d activities=%d (purged=%d) sightings=%d attachments=%d attachment_files=%d revisions=%d",
		counts.Imports, counts.Activities, counts.PurgedActivities, counts.Sightings, counts.Attachments, counts.AttachmentFiles, counts.Revisions)
}

// standaloneRecordNumber reads a standalone row's number and checks its id is
// the one that number makes: the import keeps ids by their number and will
// not change one.
func standaloneRecordNumber(prefix string, seqValue, idValue any) (int64, error) {
	seq, err := standalonesqlite.Integer("seq", seqValue)
	if err != nil {
		return 0, err
	}
	id, err := standalonesqlite.Text("id", idValue)
	if err != nil {
		return 0, err
	}
	if want := postgresstorage.FormatRecordID(prefix, seq); id != want {
		return 0, fmt.Errorf("id %q has number %d, whose id is %s", id, seq, want)
	}
	return seq, nil
}

// standaloneReference reads a column that names another row by its public id.
func standaloneReference(column, prefix string, value any) (int64, error) {
	id, err := standalonesqlite.Text(column, value)
	if err != nil {
		return 0, err
	}
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", column, err)
	}
	return number, nil
}

// standaloneAttachmentPath is where the standalone store kept an attachment
// row's file: storedPath is relative to its data directory and begins with
// its attachments folder, which attachmentsDirectory is.
func standaloneAttachmentPath(attachmentsDirectory, storedPath string) (string, error) {
	relative, found := strings.CutPrefix(filepath.ToSlash(storedPath), standaloneAttachmentFolder+"/")
	if !found || relative == "" {
		return "", fmt.Errorf("stored_path %q is not under the %s folder", storedPath, standaloneAttachmentFolder)
	}
	cleaned := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("stored_path %q leaves the %s folder", storedPath, standaloneAttachmentFolder)
	}
	return filepath.Join(attachmentsDirectory, cleaned), nil
}

// attachmentCarrier hands each live attachment's bytes to file-store during
// the copy, after checking them against the row, and remembers what it kept
// so a failed import destroys them again.
type attachmentCarrier struct {
	ctx                  context.Context
	files                AttachmentFiles
	attachmentsDirectory string
	kept                 []string
}

func (carrier *attachmentCarrier) carry(activitySeq int64, fileName, contentType, wantSHA256 string, wantSize int64, storedPath string) (string, error) {
	path, err := standaloneAttachmentPath(carrier.attachmentsDirectory, storedPath)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading its file: %w", err)
	}
	digest := sha256.Sum256(content)
	if got := hex.EncodeToString(digest[:]); got != wantSHA256 {
		return "", fmt.Errorf("its file has sha256 %s, and the row says %s", got, wantSHA256)
	}
	if int64(len(content)) != wantSize {
		return "", fmt.Errorf("its file is %d bytes, and the row says %d", len(content), wantSize)
	}
	fileID, err := carrier.files.Put(carrier.ctx, bytes.NewReader(content), AttachmentFile{
		ActivityID: postgresstorage.FormatRecordID(IDPrefixActivity, activitySeq), FileName: fileName, ContentType: contentType})
	if err != nil {
		return "", fmt.Errorf("keeping its file in file-store: %w", err)
	}
	carrier.kept = append(carrier.kept, fileID)
	return fileID, nil
}

// standaloneTables are the tables of the standalone file in the order they
// are copied (each after what it points at), with the live schema's columns.
// The FTS5 table is not copied: each activity's search document is made
// again from its title, url and details, and a purged one's is empty, as its
// FTS5 row was deleted.
func standaloneTables(owner string, carrier *attachmentCarrier, purged *int64) []standalonesqlite.Table {
	return []standalonesqlite.Table{
		{
			Name:    "imports",
			Columns: []string{"seq", "id", "kind", "sha256", "source_machine", "source_path", "imported_at", "finished_at", "counts"},
			OrderBy: "seq",
			Insert: `INSERT INTO imports (seq, owner_principal_id, kind, sha256, source_machine, source_path, imported_at, finished_at, counts)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(IDPrefixImport, row[0], row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner}, row[2:]...), nil
			},
		},
		{
			Name: "activities",
			Columns: []string{"seq", "id", "product", "action", "action_text", "title", "url", "occurred_at", "latitude", "longitude",
				"details", "source", "source_ref", "import_id", "dedupe_key", "created_at", "updated_at", "deleted_at", "purged_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO activities (seq, owner_principal_id, product, action, action_text, title, url, occurred_at, latitude, longitude,
				details, source, source_ref, import_seq, dedupe_key, created_at, updated_at, deleted_at, purged_at, search_document)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
				CASE WHEN CAST($19 AS bigint) <> 0 THEN ''::tsvector ELSE ` + searchDocumentSQL("$6", "$7", "$20") + ` END)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(IDPrefixActivity, row[0], row[1])
				if err != nil {
					return nil, err
				}
				var importSeq any
				if row[13] != nil {
					number, err := standaloneReference("import_id", IDPrefixImport, row[13])
					if err != nil {
						return nil, err
					}
					importSeq = number
				}
				detailsText, err := standalonesqlite.Text("details", row[10])
				if err != nil {
					return nil, err
				}
				var details Details
				if err := json.Unmarshal([]byte(detailsText), &details); err != nil {
					return nil, fmt.Errorf("details are not JSON: %w", err)
				}
				purgedAt, err := standalonesqlite.Integer("purged_at", row[18])
				if err != nil {
					return nil, err
				}
				if purgedAt != 0 {
					*purged++
				}
				values := append([]any{seq, owner}, row[2:13]...)
				values = append(values, importSeq)
				values = append(values, row[14:19]...)
				return append(values, searchableDetails(details)), nil
			},
		},
		{
			// A sighting has no id; rowid keeps the order they were seen in.
			Name:    "sightings",
			Columns: []string{"activity_id", "source", "source_ref", "product", "action", "import_id", "occurred_at", "seen_at"},
			OrderBy: "rowid",
			Insert: `INSERT INTO sightings (owner_principal_id, activity_seq, source, source_ref, product, action, import_seq, occurred_at, seen_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			Convert: func(row []any) ([]any, error) {
				activitySeq, err := standaloneReference("activity_id", IDPrefixActivity, row[0])
				if err != nil {
					return nil, err
				}
				importSeq, err := standaloneReference("import_id", IDPrefixImport, row[5])
				if err != nil {
					return nil, err
				}
				values := append([]any{owner, activitySeq}, row[1:5]...)
				return append(values, importSeq, row[6], row[7]), nil
			},
		},
		{
			Name: "attachments",
			Columns: []string{"seq", "id", "activity_id", "file_name", "content_type", "sha256", "size_bytes", "stored_path",
				"created_at", "purged_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO attachments (seq, owner_principal_id, activity_seq, file_name, content_type, sha256, size_bytes, file_id,
				created_at, purged_at) OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(IDPrefixAttachment, row[0], row[1])
				if err != nil {
					return nil, err
				}
				activitySeq, err := standaloneReference("activity_id", IDPrefixActivity, row[2])
				if err != nil {
					return nil, err
				}
				purgedAt, err := standalonesqlite.Integer("purged_at", row[9])
				if err != nil {
					return nil, err
				}
				fileID := ""
				if purgedAt == 0 {
					fileName, err := standalonesqlite.Text("file_name", row[3])
					if err != nil {
						return nil, err
					}
					contentType, err := standalonesqlite.Text("content_type", row[4])
					if err != nil {
						return nil, err
					}
					hash, err := standalonesqlite.Text("sha256", row[5])
					if err != nil {
						return nil, err
					}
					size, err := standalonesqlite.Integer("size_bytes", row[6])
					if err != nil {
						return nil, err
					}
					storedPath, err := standalonesqlite.Text("stored_path", row[7])
					if err != nil {
						return nil, err
					}
					if fileID, err = carrier.carry(activitySeq, fileName, contentType, hash, size, storedPath); err != nil {
						return nil, err
					}
				}
				values := append([]any{seq, owner, activitySeq}, row[3:7]...)
				return append(values, fileID, row[8], row[9]), nil
			},
		},
		{
			Name:    "revisions",
			Columns: []string{"id", "activity_id", "reason", "revised_at", "snapshot"},
			OrderBy: "id",
			Insert: `INSERT INTO revisions (id, owner_principal_id, activity_seq, reason, revised_at, snapshot)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				id, err := standalonesqlite.Integer("id", row[0])
				if err != nil {
					return nil, err
				}
				activitySeq, err := standaloneReference("activity_id", IDPrefixActivity, row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{id, owner, activitySeq}, row[2:]...), nil
			},
		},
	}
}

// numberedTables are the tables whose numbers become public ids or are
// answered as ids, with their identity column, numbered on after the import.
var numberedTables = [][2]string{{"imports", "seq"}, {"activities", "seq"}, {"attachments", "seq"}, {"revisions", "id"}}

// ImportStandaloneDatabase copies every row of a standalone activity-store's
// SQLite file into the module behind db, as it is — numbers, ids, times and
// text — with every row owned by ownerPrincipalID, and hands each live
// attachment's file, read from attachmentsDirectory (the standalone data
// directory's attachments folder), to files. The rows go in one transaction,
// so it copies everything or nothing; when it fails, the files it handed
// file-store are destroyed again.
//
// It refuses an owner that is not a principal-store id, a module that already
// holds any row (so it can never mix two stores or run twice), and a file it
// cannot copy whole: a table with a column it does not know, an id that is
// not the form its number makes, a row pointing at one the file does not
// hold, or an attachment whose file is missing or does not match its row's
// sha256 and size. A copy holding only some of the activities, with their
// sightings, attachments and revisions, imports the same way. Afterwards each
// numbered table numbers on from the highest number copied, so no id that
// chats, notes and links already carry is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, files AttachmentFiles, sqlitePath, attachmentsDirectory, ownerPrincipalID string) (StandaloneImportCounts, error) {
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return StandaloneImportCounts{}, err
	}
	if info, err := os.Stat(attachmentsDirectory); err != nil {
		return StandaloneImportCounts{}, fmt.Errorf("the standalone attachments directory: %w", err)
	} else if !info.IsDir() {
		return StandaloneImportCounts{}, fmt.Errorf("the standalone attachments directory %s is not a directory", attachmentsDirectory)
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return StandaloneImportCounts{}, err
	}
	defer source.Close()
	carrier := &attachmentCarrier{ctx: ctx, files: files, attachmentsDirectory: attachmentsDirectory}
	var purged int64
	tables := standaloneTables(ownerPrincipalID, carrier, &purged)
	tableNames := make([]string, 0, len(tables))
	for _, table := range tables {
		if err := standalonesqlite.CheckColumns(ctx, source, standaloneStoreName, table.Name, table.Columns); err != nil {
			return StandaloneImportCounts{}, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		tableNames = append(tableNames, table.Name)
	}
	copied := map[string]int64{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "activity", tableNames...); err != nil {
			return err
		}
		for _, table := range tables {
			count, err := standalonesqlite.Copy(ctx, source, tx, table)
			if err != nil {
				return err
			}
			copied[table.Name] = count
		}
		for _, numbered := range numberedTables {
			var last int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX("+numbered[1]+"), 0) FROM "+numbered[0]).Scan(&last); err != nil {
				return fmt.Errorf("reading the highest number in %s: %w", numbered[0], err)
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbered[0], numbered[1], last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		cleanup := (&Store{files: files}).purgeFiles(ctx, carrier.kept)
		return StandaloneImportCounts{}, errors.Join(fmt.Errorf("importing %s: %w", sqlitePath, err), cleanup)
	}
	return StandaloneImportCounts{
		Imports:          copied["imports"],
		Activities:       copied["activities"],
		Sightings:        copied["sightings"],
		Attachments:      copied["attachments"],
		AttachmentFiles:  int64(len(carrier.kept)),
		Revisions:        copied["revisions"],
		PurgedActivities: purged,
	}, nil
}
