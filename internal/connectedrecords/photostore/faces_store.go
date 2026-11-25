package photostore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var personIDPattern = regexp.MustCompile(`^person_[0-9]{6,}$`)

// faceBoxSlack is how far past the thumbnail's edge a box may run: a face
// cut off by the frame is still found, with part of its box outside.
const faceBoxSlack = 0.5

func faceID(seq int64) string { return postgresstorage.FormatRecordID(FaceIDPrefix, seq) }

func photoPersonID(seq int64) string { return postgresstorage.FormatRecordID(PhotoPersonIDPrefix, seq) }

// decodeEmbedding reads a base64 float32 embedding and normalizes it to
// length 1, so a dot product of two is their cosine similarity.
func decodeEmbedding(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: embedding_float32_base64 is not base64: %v", ErrInvalid, err)
	}
	if len(raw) == 0 || len(raw)%4 != 0 {
		return nil, fmt.Errorf("%w: an embedding must be a whole number of float32s, got %d bytes", ErrInvalid, len(raw))
	}
	values := make([]float32, len(raw)/4)
	var squares float64
	for index := range values {
		values[index] = math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4:]))
		if math.IsNaN(float64(values[index])) || math.IsInf(float64(values[index]), 0) {
			return nil, fmt.Errorf("%w: an embedding holds a value that is not a number", ErrInvalid)
		}
		squares += float64(values[index]) * float64(values[index])
	}
	if squares == 0 {
		return nil, fmt.Errorf("%w: an embedding of zeros describes no face", ErrInvalid)
	}
	length := math.Sqrt(squares)
	normalized := make([]byte, len(raw))
	for index, value := range values {
		binary.LittleEndian.PutUint32(normalized[index*4:], math.Float32bits(float32(float64(value)/length)))
	}
	return normalized, nil
}

func validFaceBox(box FaceBox) bool {
	inRange := func(value float64) bool { return value >= -faceBoxSlack && value <= 1+faceBoxSlack }
	return inRange(box.Left) && inRange(box.Top) && inRange(box.Right) && inRange(box.Bottom) &&
		box.Right > box.Left && box.Bottom > box.Top
}

// photoOwner reads the owner of a photo in scope, or ErrNotFound.
func photoOwner(ctx context.Context, q queryer, scope ownership.Scope, photoSeq int64, id string, forUpdate bool) (owner string, deletedAt, purgedAt int64, err error) {
	if err := checkScope(scope); err != nil {
		return "", 0, 0, err
	}
	args := &arguments{}
	query := `SELECT owner_principal_id, deleted_at, purged_at FROM photos WHERE seq = ` + args.add(photoSeq) + ` AND ` + args.scope(scope, "owner_principal_id")
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err = q.QueryRowContext(ctx, query, args.values...).Scan(&owner, &deletedAt, &purgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, fmt.Errorf("%w: no photo %s", ErrNotFound, id)
	}
	return owner, deletedAt, purgedAt, err
}

// RecordFaceScan answers POST /face-scans: it keeps what one model found on
// one photo in scope, all or nothing; the scan and its faces take the photo's
// owner. A photo already scanned by the model is a conflict, so a face id,
// once given, keeps meaning the same face.
func (s *Store) RecordFaceScan(ctx context.Context, scope ownership.Scope, scan FaceScan) ([]Face, error) {
	if strings.TrimSpace(scan.Model) == "" {
		return nil, fmt.Errorf("%w: model is required: name the face model that found the faces", ErrInvalid)
	}
	embeddings := make([][]byte, len(scan.Faces))
	for index, face := range scan.Faces {
		if !validFaceBox(face.Box) {
			return nil, fmt.Errorf("%w: face %d has a box outside the thumbnail or turned inside out", ErrInvalid, index)
		}
		embedding, err := decodeEmbedding(face.EmbeddingFloat32Base64)
		if err != nil {
			return nil, fmt.Errorf("face %d: %w", index, err)
		}
		embeddings[index] = embedding
	}
	photoSeq, err := recordNumberOf(PhotoIDPrefix, "photo", scan.PhotoID)
	if err != nil {
		return nil, err
	}
	faces := make([]Face, 0, len(scan.Faces))
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		owner, _, purgedAt, err := photoOwner(ctx, tx, scope, photoSeq, scan.PhotoID, true)
		if err != nil {
			return err
		}
		if purgedAt != 0 {
			return fmt.Errorf("%w: %s was purged; its faces are not kept", ErrConflict, scan.PhotoID)
		}
		var scanned int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM face_scans WHERE photo_seq = $1 AND model = $2`, photoSeq, scan.Model).Scan(&scanned); err != nil {
			return err
		}
		created := now()
		if scanned > 0 && !scan.Replaces {
			return fmt.Errorf("%w: %s was already scanned by %s; send replaces to scan it again", ErrConflict, scan.PhotoID, scan.Model)
		}
		if scanned > 0 {
			if _, err := retireFacesOfPhoto(ctx, tx, photoSeq, scan.Model, "a new scan by the same model replaced it", created); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO face_scans (owner_principal_id, photo_seq, model, face_count, scanned_at) VALUES ($1, $2, $3, $4, $5)`,
			owner, photoSeq, scan.Model, len(scan.Faces), created); err != nil {
			return conflictOf(err)
		}
		for index, face := range scan.Faces {
			var seq int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO faces (owner_principal_id, photo_seq, model, box_left, box_top, box_right, box_bottom,
				detection_score, embedding, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING seq`, owner, photoSeq, scan.Model,
				face.Box.Left, face.Box.Top, face.Box.Right, face.Box.Bottom, face.DetectionScore, embeddings[index], created).Scan(&seq); err != nil {
				return err
			}
			faces = append(faces, Face{ID: faceID(seq), OwnerPrincipalID: owner, PhotoID: scan.PhotoID, Model: scan.Model, Box: face.Box,
				DetectionScore: face.DetectionScore, CreatedAt: created})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return faces, nil
}

// retireFacesOfPhoto retires the live faces one model (every model when
// model is "") found on a photo, and forgets that the photo was scanned, so
// it is scanned again. A retired face keeps its row and id but leaves every
// list. A link that named one keeps its status, since whether the person is
// in the photo did not change, but no longer names a face; its old row goes
// to photo_person_revisions. It answers how many faces it retired.
func retireFacesOfPhoto(ctx context.Context, tx *sql.Tx, photoSeq int64, model, reason string, at int64) (int, error) {
	modelClause, args := "", []any{photoSeq}
	if model != "" {
		modelClause, args = " AND model = $2", append(args, model)
	}
	links, err := queryPhotoPeople(ctx, tx, `SELECT `+photoPersonColumns+` FROM photo_people WHERE face_seq IN
		(SELECT seq FROM faces WHERE photo_seq = $1 AND retired_at = 0`+modelClause+`) FOR UPDATE`, args...)
	if err != nil {
		return 0, err
	}
	for _, link := range links {
		if err := keepPhotoPersonRevision(ctx, tx, link, at); err != nil {
			return 0, err
		}
		note := fmt.Sprintf("The face this link named (%s) was retired: %s.", link.FaceID, reason)
		evidence := strings.TrimSpace(link.Evidence + "\n\n" + note)
		if _, err := tx.ExecContext(ctx, `UPDATE photo_people SET face_seq = NULL, evidence = $1, updated_at = $2 WHERE seq = $3`,
			evidence, at, mustRecordNumber(PhotoPersonIDPrefix, link.ID)); err != nil {
			return 0, err
		}
	}
	updateClause := strings.ReplaceAll(modelClause, "$2", "$4")
	retired, err := tx.ExecContext(ctx, `UPDATE faces SET retired_at = $1, retired_reason = $2 WHERE photo_seq = $3 AND retired_at = 0`+updateClause,
		append([]any{at, reason}, args...)...)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM face_scans WHERE photo_seq = $1`+modelClause, args...); err != nil {
		return 0, err
	}
	count, err := retired.RowsAffected()
	return int(count), err
}

// mustRecordNumber reads the number of an id this store formatted itself.
func mustRecordNumber(prefix, id string) int64 {
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		panic(fmt.Sprintf("photostore: an id the store formatted does not parse: %v", err))
	}
	return number
}

// PhotosNotScanned answers GET /face-scans/pending: live, unpurged images in
// scope the model has not scanned yet, oldest id first.
func (s *Store) PhotosNotScanned(ctx context.Context, scope ownership.Scope, model string, limit int) ([]string, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalid)
	}
	if limit <= 0 || limit > maximumLimit {
		limit = maximumLimit
	}
	args := &arguments{}
	rows, err := s.db.QueryContext(ctx, `SELECT seq FROM photos WHERE `+args.scope(scope, "owner_principal_id")+`
		AND deleted_at = 0 AND purged_at = 0 AND media_type LIKE 'image/%'
		AND NOT EXISTS (SELECT 1 FROM face_scans WHERE face_scans.photo_seq = photos.seq AND face_scans.model = `+args.add(model)+`)
		ORDER BY seq LIMIT `+args.add(limit), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		ids = append(ids, photoID(seq))
	}
	return ids, rows.Err()
}

// FaceFilter selects faces for GET /faces, in id order. AfterID pages: the
// next page starts after the last id of the one before.
type FaceFilter struct {
	PhotoID           string
	Model             string
	AfterID           string
	IncludeEmbeddings bool
	Limit             int
}

const maximumFaceLimit = 5000

// Faces answers GET /faces and GET /photos/{id}/faces: live faces on live
// photos in scope.
func (s *Store) Faces(ctx context.Context, scope ownership.Scope, filter FaceFilter) ([]Face, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	args := &arguments{}
	where := []string{args.scope(scope, "faces.owner_principal_id"), "photos.deleted_at = 0", "faces.retired_at = 0"}
	if filter.PhotoID != "" {
		photoSeq, err := postgresstorage.ParseRecordID(PhotoIDPrefix, filter.PhotoID)
		if err != nil {
			// A malformed id names no photo, so no photo's faces.
			return []Face{}, nil
		}
		where = append(where, "faces.photo_seq = "+args.add(photoSeq))
	}
	if filter.Model != "" {
		where = append(where, "faces.model = "+args.add(filter.Model))
	}
	if filter.AfterID != "" {
		afterSeq, err := postgresstorage.ParseRecordID(FaceIDPrefix, filter.AfterID)
		if err != nil {
			return nil, fmt.Errorf("%w: after must be a face id, got %q", ErrInvalid, filter.AfterID)
		}
		where = append(where, "faces.seq > "+args.add(afterSeq))
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumFaceLimit {
		limit = maximumFaceLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT faces.seq, faces.owner_principal_id, faces.photo_seq, faces.model, faces.box_left, faces.box_top,
		faces.box_right, faces.box_bottom, faces.detection_score, faces.embedding, faces.created_at
		FROM faces JOIN photos ON photos.seq = faces.photo_seq
		WHERE `+strings.Join(where, " AND ")+` ORDER BY faces.seq LIMIT `+args.add(limit), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	faces := []Face{}
	for rows.Next() {
		var face Face
		var seq, photoSeq int64
		var embedding []byte
		if err := rows.Scan(&seq, &face.OwnerPrincipalID, &photoSeq, &face.Model, &face.Box.Left, &face.Box.Top, &face.Box.Right, &face.Box.Bottom,
			&face.DetectionScore, &embedding, &face.CreatedAt); err != nil {
			return nil, err
		}
		face.ID, face.PhotoID = faceID(seq), photoID(photoSeq)
		if filter.IncludeEmbeddings {
			face.EmbeddingFloat32Base64 = base64.StdEncoding.EncodeToString(embedding)
		}
		faces = append(faces, face)
	}
	return faces, rows.Err()
}

const photoPersonColumns = `seq, owner_principal_id, photo_seq, person_id, face_seq, status, confidence, evidence, proposed_by, decided_by, created_at, updated_at`

// photoPersonColumnsJoinedToPhotos is photoPersonColumns named by table, for
// a query that joins photos, which has columns of the same names.
const photoPersonColumnsJoinedToPhotos = `photo_people.seq, photo_people.owner_principal_id, photo_people.photo_seq, photo_people.person_id,
	photo_people.face_seq, photo_people.status, photo_people.confidence, photo_people.evidence, photo_people.proposed_by,
	photo_people.decided_by, photo_people.created_at, photo_people.updated_at`

func scanPhotoPerson(row interface{ Scan(...any) error }) (PhotoPerson, error) {
	var link PhotoPerson
	var seq, photoSeq int64
	var faceSeq sql.NullInt64
	err := row.Scan(&seq, &link.OwnerPrincipalID, &photoSeq, &link.PersonID, &faceSeq, &link.Status, &link.Confidence, &link.Evidence,
		&link.ProposedBy, &link.DecidedBy, &link.CreatedAt, &link.UpdatedAt)
	if err != nil {
		return PhotoPerson{}, err
	}
	link.ID, link.PhotoID = photoPersonID(seq), photoID(photoSeq)
	if faceSeq.Valid {
		link.FaceID = faceID(faceSeq.Int64)
	}
	return link, nil
}

// queryPhotoPeople reads every link a query selects, closing its rows before
// it answers, so the caller may write in the same transaction.
func queryPhotoPeople(ctx context.Context, q queryer, query string, args ...any) ([]PhotoPerson, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []PhotoPerson{}
	for rows.Next() {
		link, err := scanPhotoPerson(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// nullableFaceSeq is the face column of a link: NULL for no face.
func nullableFaceSeq(faceID string) any {
	if faceID == "" {
		return nil
	}
	return mustRecordNumber(FaceIDPrefix, faceID)
}

// checkFaceOnPhoto refuses a face that is not one of the owner's, not on the
// photo, or retired.
func checkFaceOnPhoto(ctx context.Context, tx *sql.Tx, ownerPrincipalID, faceIDOnLink string, photoSeq int64) error {
	if faceIDOnLink == "" {
		return nil
	}
	faceSeq, err := postgresstorage.ParseRecordID(FaceIDPrefix, faceIDOnLink)
	if err != nil {
		return fmt.Errorf("%w: no face %s", ErrInvalid, faceIDOnLink)
	}
	var onPhoto, retiredAt int64
	err = tx.QueryRowContext(ctx, `SELECT photo_seq, retired_at FROM faces WHERE seq = $1 AND owner_principal_id = $2`, faceSeq, ownerPrincipalID).
		Scan(&onPhoto, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no face %s", ErrInvalid, faceIDOnLink)
	} else if err != nil {
		return err
	}
	if onPhoto != photoSeq {
		return fmt.Errorf("%w: %s is on %s, not %s", ErrInvalid, faceIDOnLink, photoID(onPhoto), photoID(photoSeq))
	}
	if retiredAt != 0 {
		return fmt.Errorf("%w: %s was retired; name a face of the photo's current scan", ErrInvalid, faceIDOnLink)
	}
	return nil
}

// keepPhotoPersonRevision copies a link, as it stands before a change, into
// photo_person_revisions; the revision takes the link's owner.
func keepPhotoPersonRevision(ctx context.Context, tx *sql.Tx, link PhotoPerson, at int64) error {
	row, err := json.Marshal(link)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_person_revisions (owner_principal_id, photo_person_seq, replaced_at, row_json) VALUES ($1, $2, $3, $4::jsonb)`,
		link.OwnerPrincipalID, mustRecordNumber(PhotoPersonIDPrefix, link.ID), at, string(row))
	return err
}

// ProposePhotoPerson answers POST /photo-people. It creates a link on a photo
// in scope, owned by the photo's owner, or moves an existing suggestion up to
// what the proposal says; it never changes a rejected or confirmed link, and
// answers it as it stands, with created false. The person must be one the
// photo's owner keeps in people-store.
func (s *Store) ProposePhotoPerson(ctx context.Context, scope ownership.Scope, proposal PhotoPersonProposal) (PhotoPerson, bool, error) {
	if !personIDPattern.MatchString(proposal.PersonID) {
		return PhotoPerson{}, false, fmt.Errorf("%w: person_id must be people-store's id (person_000001), got %q", ErrInvalid, proposal.PersonID)
	}
	if proposal.Status != PhotoPersonSuggested && proposal.Status != PhotoPersonConfirmed {
		return PhotoPerson{}, false, fmt.Errorf("%w: a proposal is suggested or confirmed, got %q", ErrInvalid, proposal.Status)
	}
	if strings.TrimSpace(proposal.ProposedBy) == "" {
		return PhotoPerson{}, false, fmt.Errorf("%w: proposed_by is required: who or what thinks so", ErrInvalid)
	}
	if proposal.Status == PhotoPersonConfirmed && strings.TrimSpace(proposal.DecidedBy) == "" {
		return PhotoPerson{}, false, fmt.Errorf("%w: a confirmed link needs decided_by", ErrInvalid)
	}
	if proposal.Confidence < 0 || proposal.Confidence > 1 {
		return PhotoPerson{}, false, fmt.Errorf("%w: confidence is between 0 and 1", ErrInvalid)
	}
	photoSeq, err := recordNumberOf(PhotoIDPrefix, "photo", proposal.PhotoID)
	if err != nil {
		return PhotoPerson{}, false, err
	}
	// A photo's owner never changes, so it is read once, before people-store
	// is asked, and the transaction holds the photo to it.
	owner, _, _, err := photoOwner(ctx, s.db, scope, photoSeq, proposal.PhotoID, false)
	if err != nil {
		return PhotoPerson{}, false, err
	}
	if err := s.personKeptByOwner(ctx, owner, proposal.PersonID); err != nil {
		return PhotoPerson{}, false, err
	}
	var link PhotoPerson
	var created bool
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		if _, _, _, err := photoOwner(ctx, tx, ownership.OwnedBy(owner), photoSeq, proposal.PhotoID, true); err != nil {
			return err
		}
		link, created, err = proposePhotoPersonInTransaction(ctx, tx, owner, photoSeq, proposal, now())
		return err
	})
	if err != nil {
		return PhotoPerson{}, false, conflictOf(err)
	}
	return link, created, nil
}

// proposePhotoPersonInTransaction is ProposePhotoPerson's work inside a
// transaction the caller commits, for a proposal already validated on a
// photo of ownerPrincipalID's.
func proposePhotoPersonInTransaction(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, photoSeq int64, proposal PhotoPersonProposal, at int64) (PhotoPerson, bool, error) {
	if err := checkFaceOnPhoto(ctx, tx, ownerPrincipalID, proposal.FaceID, photoSeq); err != nil {
		return PhotoPerson{}, false, err
	}
	decidedBy := ""
	if proposal.Status == PhotoPersonConfirmed {
		decidedBy = proposal.DecidedBy
	}
	existing, err := scanPhotoPerson(tx.QueryRowContext(ctx, `SELECT `+photoPersonColumns+` FROM photo_people
		WHERE owner_principal_id = $1 AND photo_seq = $2 AND person_id = $3 FOR UPDATE`, ownerPrincipalID, photoSeq, proposal.PersonID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		link := PhotoPerson{OwnerPrincipalID: ownerPrincipalID, PhotoID: photoID(photoSeq), PersonID: proposal.PersonID, FaceID: proposal.FaceID,
			Status: proposal.Status, Confidence: proposal.Confidence, Evidence: proposal.Evidence, ProposedBy: proposal.ProposedBy,
			DecidedBy: decidedBy, CreatedAt: at, UpdatedAt: at}
		var seq int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO photo_people (owner_principal_id, photo_seq, person_id, face_seq, status, confidence, evidence,
			proposed_by, decided_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING seq`,
			ownerPrincipalID, photoSeq, link.PersonID, nullableFaceSeq(link.FaceID), link.Status, link.Confidence, link.Evidence,
			link.ProposedBy, link.DecidedBy, at, at).Scan(&seq); err != nil {
			return PhotoPerson{}, false, err
		}
		link.ID = photoPersonID(seq)
		return link, true, nil
	case err != nil:
		return PhotoPerson{}, false, err
	}
	if existing.Status == PhotoPersonConfirmed && existing.FaceID == "" && proposal.FaceID != "" {
		// A confirmed link that lost its face (a rescan retired it) takes the
		// face a matcher finds for the same person; its status stays.
		if err := keepPhotoPersonRevision(ctx, tx, existing, at); err != nil {
			return PhotoPerson{}, false, err
		}
		existing.FaceID, existing.UpdatedAt = proposal.FaceID, at
		if _, err := tx.ExecContext(ctx, `UPDATE photo_people SET face_seq = $1, updated_at = $2 WHERE seq = $3`,
			nullableFaceSeq(existing.FaceID), at, mustRecordNumber(PhotoPersonIDPrefix, existing.ID)); err != nil {
			return PhotoPerson{}, false, err
		}
		return existing, false, nil
	}
	if existing.Status != PhotoPersonSuggested {
		return existing, false, nil
	}
	if err := keepPhotoPersonRevision(ctx, tx, existing, at); err != nil {
		return PhotoPerson{}, false, err
	}
	updated := existing
	updated.Status, updated.Confidence, updated.Evidence, updated.ProposedBy = proposal.Status, proposal.Confidence, proposal.Evidence, proposal.ProposedBy
	updated.DecidedBy, updated.UpdatedAt = decidedBy, at
	if proposal.FaceID != "" {
		updated.FaceID = proposal.FaceID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE photo_people SET face_seq = $1, status = $2, confidence = $3, evidence = $4, proposed_by = $5,
		decided_by = $6, updated_at = $7 WHERE seq = $8`, nullableFaceSeq(updated.FaceID), updated.Status, updated.Confidence, updated.Evidence,
		updated.ProposedBy, updated.DecidedBy, at, mustRecordNumber(PhotoPersonIDPrefix, updated.ID)); err != nil {
		return PhotoPerson{}, false, err
	}
	return updated, false, nil
}

// DecidePhotoPerson answers PATCH /photo-people/{id}: a person confirming or
// rejecting a link in scope, or pointing it at a face of its photo. Any
// status change needs decided_by; a person may change a link however a
// matcher left it.
func (s *Store) DecidePhotoPerson(ctx context.Context, scope ownership.Scope, id string, decision PhotoPersonDecision) (PhotoPerson, error) {
	if err := checkScope(scope); err != nil {
		return PhotoPerson{}, err
	}
	if decision.Status != nil && !slices.Contains(PhotoPersonStatuses, *decision.Status) {
		return PhotoPerson{}, fmt.Errorf("%w: status is one of %s", ErrInvalid, strings.Join(PhotoPersonStatuses, ", "))
	}
	if decision.Status != nil && (decision.DecidedBy == nil || strings.TrimSpace(*decision.DecidedBy) == "") {
		return PhotoPerson{}, fmt.Errorf("%w: a status change needs decided_by", ErrInvalid)
	}
	seq, err := recordNumberOf(PhotoPersonIDPrefix, "link", id)
	if err != nil {
		return PhotoPerson{}, err
	}
	var link PhotoPerson
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		args := &arguments{}
		link, err = scanPhotoPerson(tx.QueryRowContext(ctx, `SELECT `+photoPersonColumns+` FROM photo_people WHERE seq = `+args.add(seq)+
			` AND `+args.scope(scope, "owner_principal_id")+` FOR UPDATE`, args.values...))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no link %s", ErrNotFound, id)
		} else if err != nil {
			return err
		}
		at := now()
		if err := keepPhotoPersonRevision(ctx, tx, link, at); err != nil {
			return err
		}
		if decision.Status != nil {
			link.Status = *decision.Status
			link.DecidedBy = *decision.DecidedBy
			if link.Status == PhotoPersonSuggested {
				link.DecidedBy = ""
			}
		}
		if decision.FaceID != nil {
			if err := checkFaceOnPhoto(ctx, tx, link.OwnerPrincipalID, *decision.FaceID, mustRecordNumber(PhotoIDPrefix, link.PhotoID)); err != nil {
				return err
			}
			link.FaceID = *decision.FaceID
		}
		if decision.Evidence != nil {
			link.Evidence = *decision.Evidence
		}
		link.UpdatedAt = at
		_, err = tx.ExecContext(ctx, `UPDATE photo_people SET face_seq = $1, status = $2, evidence = $3, decided_by = $4, updated_at = $5 WHERE seq = $6`,
			nullableFaceSeq(link.FaceID), link.Status, link.Evidence, link.DecidedBy, at, seq)
		return err
	})
	if err != nil {
		return PhotoPerson{}, err
	}
	return link, nil
}

// PhotoPersonFilter selects links for GET /photo-people, in id order.
type PhotoPersonFilter struct {
	PhotoID  string
	PersonID string
	Status   string
	Limit    int
	Offset   int
}

// PhotoPeople answers GET /photo-people: links in scope on live photos.
func (s *Store) PhotoPeople(ctx context.Context, scope ownership.Scope, filter PhotoPersonFilter) ([]PhotoPerson, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	if filter.Status != "" && !slices.Contains(PhotoPersonStatuses, filter.Status) {
		return nil, fmt.Errorf("%w: status is one of %s", ErrInvalid, strings.Join(PhotoPersonStatuses, ", "))
	}
	args := &arguments{}
	where := []string{args.scope(scope, "photo_people.owner_principal_id"), "photos.deleted_at = 0"}
	if filter.PhotoID != "" {
		photoSeq, err := postgresstorage.ParseRecordID(PhotoIDPrefix, filter.PhotoID)
		if err != nil {
			return []PhotoPerson{}, nil
		}
		where = append(where, "photo_people.photo_seq = "+args.add(photoSeq))
	}
	if filter.PersonID != "" {
		where = append(where, "photo_people.person_id = "+args.add(filter.PersonID))
	}
	if filter.Status != "" {
		where = append(where, "photo_people.status = "+args.add(filter.Status))
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumFaceLimit {
		limit = maximumFaceLimit
	}
	return queryPhotoPeople(ctx, s.db, `SELECT `+photoPersonColumnsJoinedToPhotos+` FROM photo_people JOIN photos ON photos.seq = photo_people.photo_seq
		WHERE `+strings.Join(where, " AND ")+` ORDER BY photo_people.seq LIMIT `+args.add(limit)+` OFFSET `+args.add(max(filter.Offset, 0)), args.values...)
}

// PeopleCounts answers GET /people-counts: per person, how many live photos
// in scope they are confirmed and suggested in, most confirmed first.
func (s *Store) PeopleCounts(ctx context.Context, scope ownership.Scope) ([]PersonPhotoCount, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	args := &arguments{}
	rows, err := s.db.QueryContext(ctx, `SELECT person_id,
		SUM(CASE WHEN status = 'confirmed' THEN 1 ELSE 0 END), SUM(CASE WHEN status = 'suggested' THEN 1 ELSE 0 END)
		FROM photo_people JOIN photos ON photos.seq = photo_people.photo_seq
		WHERE `+args.scope(scope, "photo_people.owner_principal_id")+` AND photos.deleted_at = 0 AND status <> 'rejected'
		GROUP BY person_id ORDER BY 2 DESC, 3 DESC, person_id COLLATE "C"`, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := []PersonPhotoCount{}
	for rows.Next() {
		var count PersonPhotoCount
		if err := rows.Scan(&count.PersonID, &count.Confirmed, &count.Suggested); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

// FaceByID answers one face in scope, without its embedding.
func (s *Store) FaceByID(ctx context.Context, scope ownership.Scope, id string) (Face, error) {
	if err := checkScope(scope); err != nil {
		return Face{}, err
	}
	seq, err := recordNumberOf(FaceIDPrefix, "face", id)
	if err != nil {
		return Face{}, err
	}
	args := &arguments{}
	var face Face
	var photoSeq int64
	err = s.db.QueryRowContext(ctx, `SELECT owner_principal_id, photo_seq, model, box_left, box_top, box_right, box_bottom, detection_score, created_at
		FROM faces WHERE seq = `+args.add(seq)+` AND `+args.scope(scope, "owner_principal_id"), args.values...).
		Scan(&face.OwnerPrincipalID, &photoSeq, &face.Model, &face.Box.Left, &face.Box.Top, &face.Box.Right, &face.Box.Bottom, &face.DetectionScore, &face.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Face{}, fmt.Errorf("%w: no face %s", ErrNotFound, id)
	}
	if err != nil {
		return Face{}, err
	}
	face.ID, face.PhotoID = faceID(seq), photoID(photoSeq)
	return face, nil
}
