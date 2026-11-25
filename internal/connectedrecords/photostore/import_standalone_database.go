package photostore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneStoreName names the standalone store in the import's errors.
const standaloneStoreName = "photo-store"

// ImportCounts says how many rows of each table an import copied, how many
// sightings it filled in, and how many thumbnails it carried into file-store.
type ImportCounts struct {
	Photos               int64 `json:"photos"`
	PhotoRevisions       int64 `json:"photo_revisions"`
	Sightings            int64 `json:"sightings"`
	SightingsFilled      int64 `json:"sightings_filled"`
	FaceScans            int64 `json:"face_scans"`
	Faces                int64 `json:"faces"`
	PhotoPeople          int64 `json:"photo_people"`
	PhotoPersonRevisions int64 `json:"photo_person_revisions"`
	FaceGroups           int64 `json:"face_groups"`
	FaceGroupMembers     int64 `json:"face_group_members"`
	FaceGroupGuesses     int64 `json:"face_group_guesses"`
	Thumbnails           int64 `json:"thumbnails"`
}

// Summary is the counts in one line, and nothing of the records.
func (counts ImportCounts) Summary() string {
	return fmt.Sprintf("photos=%d photo_revisions=%d sightings=%d sightings_filled=%d face_scans=%d faces=%d photo_people=%d "+
		"photo_person_revisions=%d face_groups=%d face_group_members=%d face_group_guesses=%d thumbnails=%d",
		counts.Photos, counts.PhotoRevisions, counts.Sightings, counts.SightingsFilled, counts.FaceScans, counts.Faces, counts.PhotoPeople,
		counts.PhotoPersonRevisions, counts.FaceGroups, counts.FaceGroupMembers, counts.FaceGroupGuesses, counts.Thumbnails)
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

// standaloneJSON reads a TEXT column holding JSON, for a jsonb column.
func standaloneJSON(column string, value any) (string, error) {
	return standalonesqlite.Text(column, value)
}

// standaloneTables are the tables of the standalone file in the order they
// are copied (each after what it points at), with the live schema's columns:
// schema.sql's and those its migrate functions added. thumbnailFileIDs holds
// the file each unpurged photo's thumbnail was carried into, by photo number.
func standaloneTables(owner string, thumbnailFileIDs map[int64]string) []standalonesqlite.Table {
	return []standalonesqlite.Table{
		{
			Name: "photos",
			Columns: []string{"seq", "id", "sha256", "source_machine", "source_path", "folder", "media_type", "taken_at", "date_source",
				"latitude", "longitude", "width", "height", "description", "described_by", "tags", "created_at", "updated_at", "deleted_at", "purged_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO photos (seq, owner_principal_id, sha256, source_machine, source_path, folder, media_type, taken_at, date_source,
				latitude, longitude, width, height, description, described_by, tags, created_at, updated_at, deleted_at, purged_at, thumbnail_file_id)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb, $17, $18, $19, $20, $21)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(PhotoIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				tags, err := standaloneJSON("tags", row[15])
				if err != nil {
					return nil, err
				}
				purgedAt, err := standalonesqlite.Integer("purged_at", row[19])
				if err != nil {
					return nil, err
				}
				fileID := thumbnailFileIDs[seq]
				if purgedAt == 0 && fileID == "" {
					return nil, fmt.Errorf("photo %s is not purged and its thumbnail was not carried", photoID(seq))
				}
				values := append([]any{seq, owner}, row[2:15]...)
				return append(values, tags, row[16], row[17], row[18], purgedAt, fileID), nil
			},
		},
		{
			Name:    "photo_revisions",
			Columns: []string{"revision_id", "photo_id", "reason", "replaced_at", "row_json"},
			OrderBy: "revision_id",
			Insert: `INSERT INTO photo_revisions (revision_id, owner_principal_id, photo_seq, reason, replaced_at, row_json)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
			Convert: func(row []any) ([]any, error) {
				photoSeq, err := standaloneReference("photo_id", PhotoIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				rowJSON, err := standaloneJSON("row_json", row[4])
				if err != nil {
					return nil, err
				}
				return []any{row[0], owner, photoSeq, row[2], row[3], rowJSON}, nil
			},
		},
		{
			Name:    "photo_sightings",
			Columns: []string{"photo_id", "source_machine", "source_path", "folder", "seen_at"},
			OrderBy: "photo_id, source_machine, source_path",
			Insert: `INSERT INTO photo_sightings (owner_principal_id, photo_seq, source_machine, source_path, folder, seen_at)
				VALUES ($1, $2, $3, $4, $5, $6)`,
			Convert: func(row []any) ([]any, error) {
				photoSeq, err := standaloneReference("photo_id", PhotoIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				return append([]any{owner, photoSeq}, row[1:]...), nil
			},
		},
		{
			Name:    "face_scans",
			Columns: []string{"photo_id", "model", "face_count", "scanned_at"},
			OrderBy: "photo_id, model",
			Insert:  `INSERT INTO face_scans (owner_principal_id, photo_seq, model, face_count, scanned_at) VALUES ($1, $2, $3, $4, $5)`,
			Convert: func(row []any) ([]any, error) {
				photoSeq, err := standaloneReference("photo_id", PhotoIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				return append([]any{owner, photoSeq}, row[1:]...), nil
			},
		},
		{
			Name: "faces",
			Columns: []string{"seq", "id", "photo_id", "model", "box_left", "box_top", "box_right", "box_bottom", "detection_score",
				"embedding", "created_at", "retired_at", "retired_reason"},
			OrderBy: "seq",
			Insert: `INSERT INTO faces (seq, owner_principal_id, photo_seq, model, box_left, box_top, box_right, box_bottom, detection_score,
				embedding, created_at, retired_at, retired_reason)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(FaceIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				photoSeq, err := standaloneReference("photo_id", PhotoIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				if _, isBytes := row[9].([]byte); !isBytes {
					return nil, fmt.Errorf("embedding is %T, not a blob", row[9])
				}
				return append([]any{seq, owner, photoSeq}, row[3:]...), nil
			},
		},
		{
			Name: "photo_people",
			Columns: []string{"seq", "id", "photo_id", "person_id", "face_id", "status", "confidence", "evidence", "proposed_by",
				"decided_by", "created_at", "updated_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO photo_people (seq, owner_principal_id, photo_seq, person_id, face_seq, status, confidence, evidence, proposed_by,
				decided_by, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(PhotoPersonIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				photoSeq, err := standaloneReference("photo_id", PhotoIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				var faceSeq any
				if row[4] != nil {
					number, err := standaloneReference("face_id", FaceIDPrefix, row[4])
					if err != nil {
						return nil, err
					}
					faceSeq = number
				}
				values := []any{seq, owner, photoSeq, row[3], faceSeq}
				return append(values, row[5:]...), nil
			},
		},
		{
			Name:    "photo_person_revisions",
			Columns: []string{"revision_id", "photo_person_id", "replaced_at", "row_json"},
			OrderBy: "revision_id",
			Insert: `INSERT INTO photo_person_revisions (revision_id, owner_principal_id, photo_person_seq, replaced_at, row_json)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5::jsonb)`,
			Convert: func(row []any) ([]any, error) {
				linkSeq, err := standaloneReference("photo_person_id", PhotoPersonIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				rowJSON, err := standaloneJSON("row_json", row[3])
				if err != nil {
					return nil, err
				}
				return []any{row[0], owner, linkSeq, row[2], rowJSON}, nil
			},
		},
		{
			Name:    "face_groups",
			Columns: []string{"seq", "id", "model", "status", "person_id", "made_by", "decided_by", "decided_at", "created_at", "updated_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO face_groups (seq, owner_principal_id, model, status, person_id, made_by, decided_by, decided_at, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(FaceGroupIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner}, row[2:]...), nil
			},
		},
		{
			Name:    "face_group_members",
			Columns: []string{"group_id", "face_id", "similarity", "added_at", "released_at", "released_by"},
			OrderBy: "group_id, face_id",
			Insert: `INSERT INTO face_group_members (owner_principal_id, group_seq, face_seq, similarity, added_at, released_at, released_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			Convert: func(row []any) ([]any, error) {
				groupSeq, err := standaloneReference("group_id", FaceGroupIDPrefix, row[0])
				if err != nil {
					return nil, err
				}
				faceSeq, err := standaloneReference("face_id", FaceIDPrefix, row[1])
				if err != nil {
					return nil, err
				}
				return append([]any{owner, groupSeq, faceSeq}, row[2:]...), nil
			},
		},
		{
			Name:    "face_group_guesses",
			Columns: []string{"seq", "id", "group_id", "person_id", "confidence", "evidence", "proposed_by", "created_at"},
			OrderBy: "seq",
			Insert: `INSERT INTO face_group_guesses (seq, owner_principal_id, group_seq, person_id, confidence, evidence, proposed_by, created_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			Convert: func(row []any) ([]any, error) {
				seq, err := standaloneRecordNumber(FaceGroupGuessIDPrefix, row[0], row[1])
				if err != nil {
					return nil, err
				}
				groupSeq, err := standaloneReference("group_id", FaceGroupIDPrefix, row[2])
				if err != nil {
					return nil, err
				}
				return append([]any{seq, owner, groupSeq}, row[3:]...), nil
			},
		},
	}
}

// numberedTables are the tables whose numbering continues after the import,
// with their identity column.
var numberedTables = []struct{ table, column string }{
	{"photos", "seq"}, {"photo_revisions", "revision_id"}, {"faces", "seq"}, {"photo_people", "seq"},
	{"photo_person_revisions", "revision_id"}, {"face_groups", "seq"}, {"face_group_guesses", "seq"},
}

// standaloneThumbnail is one unpurged photo of the standalone file, and where
// its thumbnail is.
type standaloneThumbnail struct {
	seq  int64
	path string
}

// standaloneThumbnails lists the thumbnail every unpurged photo of the file
// must have in thumbnailsDirectory (<sha256>.jpg), and refuses the file when
// one is missing, naming the photo.
func standaloneThumbnails(ctx context.Context, source *sql.DB, thumbnailsDirectory string) ([]standaloneThumbnail, error) {
	rows, err := source.QueryContext(ctx, `SELECT seq, id, sha256 FROM photos WHERE purged_at = 0 ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("reading photos: %w", err)
	}
	defer rows.Close()
	thumbnails := []standaloneThumbnail{}
	for rows.Next() {
		var seqValue, idValue, shaValue any
		if err := rows.Scan(&seqValue, &idValue, &shaValue); err != nil {
			return nil, err
		}
		seq, err := standaloneRecordNumber(PhotoIDPrefix, seqValue, idValue)
		if err != nil {
			return nil, fmt.Errorf("photos: %w", err)
		}
		sha, err := standalonesqlite.Text("sha256", shaValue)
		if err != nil {
			return nil, fmt.Errorf("photo %s: %w", photoID(seq), err)
		}
		if !sha256Pattern.MatchString(sha) {
			return nil, fmt.Errorf("photo %s has a sha256 that is not 64 lowercase hex characters", photoID(seq))
		}
		path := filepath.Join(thumbnailsDirectory, sha+".jpg")
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("photo %s is not purged and its thumbnail is missing: %w", photoID(seq), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("photo %s is not purged and its thumbnail %s is not a regular file", photoID(seq), path)
		}
		thumbnails = append(thumbnails, standaloneThumbnail{seq: seq, path: path})
	}
	return thumbnails, rows.Err()
}

// fillSightings gives an empty photo_sightings every place the copied rows
// recorded a photo: the places its revisions kept, each first seen when the
// earliest of them was replaced, and each photo's current place. The
// standalone store filled the table the same way when it first opened a file
// made before sightings existed.
func fillSightings(ctx context.Context, tx *sql.Tx) (int64, error) {
	fromRevisions, err := tx.ExecContext(ctx, `
		INSERT INTO photo_sightings (owner_principal_id, photo_seq, source_machine, source_path, folder, seen_at)
		SELECT owner_principal_id, photo_seq, row_json->>'source_machine', row_json->>'source_path',
			(array_agg(COALESCE(row_json->>'folder', '') ORDER BY replaced_at, revision_id))[1], MIN(replaced_at)
		FROM photo_revisions
		WHERE row_json->>'source_machine' IS NOT NULL AND COALESCE(row_json->>'source_path', '') <> ''
		GROUP BY owner_principal_id, photo_seq, row_json->>'source_machine', row_json->>'source_path'
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("filling sightings from revisions: %w", err)
	}
	fromPhotos, err := tx.ExecContext(ctx, `
		INSERT INTO photo_sightings (owner_principal_id, photo_seq, source_machine, source_path, folder, seen_at)
		SELECT owner_principal_id, seq, source_machine, source_path, folder, updated_at FROM photos WHERE source_path <> ''
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("filling sightings from photos: %w", err)
	}
	first, err := fromRevisions.RowsAffected()
	if err != nil {
		return 0, err
	}
	second, err := fromPhotos.RowsAffected()
	if err != nil {
		return 0, err
	}
	return first + second, nil
}

// ImportStandaloneDatabase copies every row of a standalone photo-store's
// SQLite file into the module behind db, as it is — numbers, ids, times and
// text — with every row owned by ownerPrincipalID, and carries the thumbnail
// of every photo that is not purged from thumbnailsDirectory
// (<sha256>.jpg, the standalone store's data directory's thumbnails/) into
// thumbnails.
//
// The thumbnails are carried first, because file-store is another module and
// cannot share the rows' transaction; the rows are then copied in one
// transaction, so they are copied whole or not at all, and on any failure
// every thumbnail file already carried is destroyed again.
//
// It refuses an owner that is not a principal-store id, a module that already
// holds any row (so it can never mix two stores or run twice), an unpurged
// photo whose thumbnail is missing, and a file it cannot copy whole: a table
// with a column it does not know, an id that is not the form its number
// makes, or a row pointing at one the file does not hold. When the file holds
// no sightings it fills them from the revisions and the photos, as the
// standalone store did. Afterwards each numbered table numbers on from the
// highest number copied, so no id that chats, notes and links already carry
// is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, thumbnails ThumbnailFiles, sqlitePath, thumbnailsDirectory, ownerPrincipalID string) (ImportCounts, error) {
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return ImportCounts{}, err
	}
	if thumbnails == nil {
		return ImportCounts{}, errors.New("photostore: the import needs a keeper of thumbnail files")
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return ImportCounts{}, err
	}
	defer source.Close()
	tables := standaloneTables(ownerPrincipalID, nil)
	tableNames := make([]string, 0, len(tables))
	for _, table := range tables {
		if err := standalonesqlite.CheckColumns(ctx, source, standaloneStoreName, table.Name, table.Columns); err != nil {
			return ImportCounts{}, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		tableNames = append(tableNames, table.Name)
	}
	// Refused before any thumbnail is carried, and again inside the copy.
	if err := postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		return standalonesqlite.RefuseRowsInModule(ctx, tx, "photos", tableNames...)
	}); err != nil {
		return ImportCounts{}, err
	}
	toCarry, err := standaloneThumbnails(ctx, source, thumbnailsDirectory)
	if err != nil {
		return ImportCounts{}, fmt.Errorf("importing %s: %w", sqlitePath, err)
	}

	cleanupContext := context.WithoutCancel(ctx)
	thumbnailFileIDs := map[int64]string{}
	failWith := func(cause error) error {
		failures := []error{fmt.Errorf("importing %s: %w", sqlitePath, cause)}
		for seq, fileID := range thumbnailFileIDs {
			if err := thumbnails.Destroy(cleanupContext, fileID); err != nil {
				failures = append(failures, fmt.Errorf("destroying thumbnail file %s, carried for %s by this failed import: %w", fileID, photoID(seq), err))
			}
		}
		return errors.Join(failures...)
	}
	for _, thumbnail := range toCarry {
		fileID, err := thumbnails.KeepCopyOf(ctx, ownerPrincipalID, photoID(thumbnail.seq), thumbnail.path)
		if err != nil {
			return ImportCounts{}, failWith(fmt.Errorf("carrying the thumbnail of %s: %w", photoID(thumbnail.seq), err))
		}
		thumbnailFileIDs[thumbnail.seq] = fileID
	}

	tables = standaloneTables(ownerPrincipalID, thumbnailFileIDs)
	copied := map[string]int64{}
	var sightingsFilled int64
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "photos", tableNames...); err != nil {
			return err
		}
		for _, table := range tables {
			count, err := standalonesqlite.Copy(ctx, source, tx, table)
			if err != nil {
				return err
			}
			copied[table.Name] = count
		}
		if copied["photo_sightings"] == 0 {
			filled, err := fillSightings(ctx, tx)
			if err != nil {
				return err
			}
			sightingsFilled = filled
		}
		for _, numbered := range numberedTables {
			var last int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX("+numbered.column+"), 0) FROM "+numbered.table).Scan(&last); err != nil {
				return fmt.Errorf("reading the highest number in %s: %w", numbered.table, err)
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbered.table, numbered.column, last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, failWith(err)
	}
	return ImportCounts{
		Photos:               copied["photos"],
		PhotoRevisions:       copied["photo_revisions"],
		Sightings:            copied["photo_sightings"],
		SightingsFilled:      sightingsFilled,
		FaceScans:            copied["face_scans"],
		Faces:                copied["faces"],
		PhotoPeople:          copied["photo_people"],
		PhotoPersonRevisions: copied["photo_person_revisions"],
		FaceGroups:           copied["face_groups"],
		FaceGroupMembers:     copied["face_group_members"],
		FaceGroupGuesses:     copied["face_group_guesses"],
		Thumbnails:           int64(len(thumbnailFileIDs)),
	}, nil
}
