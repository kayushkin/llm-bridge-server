package photostore

import (
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
)

var personIDPattern = regexp.MustCompile(`^person_[0-9]{6,}$`)

// faceBoxSlack is how far past the thumbnail's edge a box may run: a face
// cut off by the frame is still found, with part of its box outside.
const faceBoxSlack = 0.5

func formatFaceID(seq int64) string { return fmt.Sprintf("face_%06d", seq) }

func formatPhotoPersonID(seq int64) string { return fmt.Sprintf("photo_person_%06d", seq) }

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

// RecordFaceScan answers POST /face-scans: it keeps what one model found on
// one photo, all or nothing. A photo already scanned by the model is a
// conflict, so a face id, once given, keeps meaning the same face.
func (s *Store) RecordFaceScan(scan FaceScan) ([]Face, error) {
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
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var purgedAt int64
	if err := tx.QueryRow(`SELECT purged_at FROM photos WHERE id = ?`, scan.PhotoID).Scan(&purgedAt); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no photo %s", ErrNotFound, scan.PhotoID)
	} else if err != nil {
		return nil, err
	}
	if purgedAt != 0 {
		return nil, fmt.Errorf("%w: %s was purged; its faces are not kept", ErrConflict, scan.PhotoID)
	}
	var scanned int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM face_scans WHERE photo_id = ? AND model = ?`, scan.PhotoID, scan.Model).Scan(&scanned); err != nil {
		return nil, err
	}
	created := now()
	if scanned > 0 && !scan.Replaces {
		return nil, fmt.Errorf("%w: %s was already scanned by %s; send replaces to scan it again", ErrConflict, scan.PhotoID, scan.Model)
	}
	if scanned > 0 {
		if _, err := retireFacesOfPhoto(tx, scan.PhotoID, scan.Model, "a new scan by the same model replaced it", created); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO face_scans (photo_id, model, face_count, scanned_at) VALUES (?, ?, ?, ?)`,
		scan.PhotoID, scan.Model, len(scan.Faces), created); err != nil {
		return nil, err
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM faces`).Scan(&seq); err != nil {
		return nil, err
	}
	faces := make([]Face, 0, len(scan.Faces))
	for index, face := range scan.Faces {
		seq++
		recorded := Face{ID: formatFaceID(seq), PhotoID: scan.PhotoID, Model: scan.Model, Box: face.Box,
			DetectionScore: face.DetectionScore, CreatedAt: created}
		if _, err := tx.Exec(`INSERT INTO faces (id, seq, photo_id, model, box_left, box_top, box_right, box_bottom, detection_score, embedding, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recorded.ID, seq, scan.PhotoID, scan.Model,
			face.Box.Left, face.Box.Top, face.Box.Right, face.Box.Bottom, face.DetectionScore, embeddings[index], created); err != nil {
			return nil, err
		}
		faces = append(faces, recorded)
	}
	return faces, tx.Commit()
}

// retireFacesOfPhoto retires the live faces one model (every model when
// model is "") found on a photo, and forgets that the photo was scanned, so
// it is scanned again. A retired face keeps its row and id but leaves every
// list. A link that named one keeps its status, since whether the person is
// in the photo did not change, but no longer names a face; its old row goes
// to photo_person_revisions. It answers how many faces it retired.
func retireFacesOfPhoto(tx *sql.Tx, photoID, model, reason string, at int64) (int, error) {
	modelClause, args := "", []any{photoID}
	if model != "" {
		modelClause, args = " AND model = ?", append(args, model)
	}
	rows, err := tx.Query(`SELECT `+photoPersonColumns+` FROM photo_people WHERE face_id IN
		(SELECT id FROM faces WHERE photo_id = ? AND retired_at = 0`+modelClause+`)`, args...)
	if err != nil {
		return 0, err
	}
	links := []PhotoPerson{}
	for rows.Next() {
		link, err := scanPhotoPerson(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		links = append(links, link)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, link := range links {
		if err := keepPhotoPersonRevision(tx, link, at); err != nil {
			return 0, err
		}
		note := fmt.Sprintf("The face this link named (%s) was retired: %s.", link.FaceID, reason)
		evidence := strings.TrimSpace(link.Evidence + "\n\n" + note)
		if _, err := tx.Exec(`UPDATE photo_people SET face_id = NULL, evidence = ?, updated_at = ? WHERE id = ?`, evidence, at, link.ID); err != nil {
			return 0, err
		}
	}
	retired, err := tx.Exec(`UPDATE faces SET retired_at = ?, retired_reason = ? WHERE photo_id = ? AND retired_at = 0`+modelClause,
		append([]any{at, reason}, args...)...)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM face_scans WHERE photo_id = ?`+modelClause, args...); err != nil {
		return 0, err
	}
	count, err := retired.RowsAffected()
	return int(count), err
}

// PhotosNotScanned answers GET /face-scans/pending: live, unpurged images the
// model has not scanned yet, oldest id first.
func (s *Store) PhotosNotScanned(model string, limit int) ([]string, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalid)
	}
	if limit <= 0 || limit > maximumLimit {
		limit = maximumLimit
	}
	rows, err := s.db.Query(`SELECT id FROM photos WHERE deleted_at = 0 AND purged_at = 0 AND media_type LIKE 'image/%'
		AND NOT EXISTS (SELECT 1 FROM face_scans WHERE face_scans.photo_id = photos.id AND face_scans.model = ?)
		ORDER BY seq LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
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

// Faces answers GET /faces and GET /photos/{id}/faces.
func (s *Store) Faces(filter FaceFilter) ([]Face, error) {
	where := []string{"photos.deleted_at = 0", "faces.retired_at = 0"}
	args := []any{}
	if filter.PhotoID != "" {
		where = append(where, "faces.photo_id = ?")
		args = append(args, filter.PhotoID)
	}
	if filter.Model != "" {
		where = append(where, "faces.model = ?")
		args = append(args, filter.Model)
	}
	if filter.AfterID != "" {
		var afterSeq int64
		if _, err := fmt.Sscanf(filter.AfterID, "face_%d", &afterSeq); err != nil {
			return nil, fmt.Errorf("%w: after must be a face id, got %q", ErrInvalid, filter.AfterID)
		}
		where = append(where, "faces.seq > ?")
		args = append(args, afterSeq)
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumFaceLimit {
		limit = maximumFaceLimit
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT faces.id, faces.photo_id, faces.model, faces.box_left, faces.box_top, faces.box_right, faces.box_bottom,
		faces.detection_score, faces.embedding, faces.created_at FROM faces JOIN photos ON photos.id = faces.photo_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY faces.seq LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	faces := []Face{}
	for rows.Next() {
		var face Face
		var embedding []byte
		if err := rows.Scan(&face.ID, &face.PhotoID, &face.Model, &face.Box.Left, &face.Box.Top, &face.Box.Right, &face.Box.Bottom,
			&face.DetectionScore, &embedding, &face.CreatedAt); err != nil {
			return nil, err
		}
		if filter.IncludeEmbeddings {
			face.EmbeddingFloat32Base64 = base64.StdEncoding.EncodeToString(embedding)
		}
		faces = append(faces, face)
	}
	return faces, rows.Err()
}

const photoPersonColumns = `id, photo_id, person_id, COALESCE(face_id, ''), status, confidence, evidence, proposed_by, decided_by, created_at, updated_at`

// photoPersonColumnsJoinedToPhotos is photoPersonColumns named by table, for
// a query that joins photos, which has columns of the same names.
const photoPersonColumnsJoinedToPhotos = `photo_people.id, photo_people.photo_id, photo_people.person_id, COALESCE(photo_people.face_id, ''),
	photo_people.status, photo_people.confidence, photo_people.evidence, photo_people.proposed_by, photo_people.decided_by,
	photo_people.created_at, photo_people.updated_at`

func scanPhotoPerson(row interface{ Scan(...any) error }) (PhotoPerson, error) {
	var link PhotoPerson
	err := row.Scan(&link.ID, &link.PhotoID, &link.PersonID, &link.FaceID, &link.Status, &link.Confidence, &link.Evidence,
		&link.ProposedBy, &link.DecidedBy, &link.CreatedAt, &link.UpdatedAt)
	return link, err
}

func nullableFaceID(faceID string) any {
	if faceID == "" {
		return nil
	}
	return faceID
}

// checkFaceOnPhoto refuses a face that is not on the photo, or was retired.
func checkFaceOnPhoto(tx *sql.Tx, faceID, photoID string) error {
	if faceID == "" {
		return nil
	}
	var onPhoto string
	var retiredAt int64
	if err := tx.QueryRow(`SELECT photo_id, retired_at FROM faces WHERE id = ?`, faceID).Scan(&onPhoto, &retiredAt); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no face %s", ErrInvalid, faceID)
	} else if err != nil {
		return err
	}
	if onPhoto != photoID {
		return fmt.Errorf("%w: %s is on %s, not %s", ErrInvalid, faceID, onPhoto, photoID)
	}
	if retiredAt != 0 {
		return fmt.Errorf("%w: %s was retired; name a face of the photo's current scan", ErrInvalid, faceID)
	}
	return nil
}

func keepPhotoPersonRevision(tx *sql.Tx, link PhotoPerson, at int64) error {
	row, err := json.Marshal(link)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO photo_person_revisions (photo_person_id, replaced_at, row_json) VALUES (?, ?, ?)`, link.ID, at, string(row))
	return err
}

// ProposePhotoPerson answers POST /photo-people. It creates a link, or moves
// an existing suggestion up to what the proposal says; it never changes a
// rejected or confirmed link, and answers it as it stands, with created false.
func (s *Store) ProposePhotoPerson(proposal PhotoPersonProposal) (PhotoPerson, bool, error) {
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
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return PhotoPerson{}, false, err
	}
	defer tx.Rollback()
	link, created, err := proposePhotoPersonInTransaction(tx, proposal, now())
	if err != nil {
		return PhotoPerson{}, false, err
	}
	return link, created, tx.Commit()
}

// proposePhotoPersonInTransaction is ProposePhotoPerson's work inside a
// transaction the caller commits, for a proposal already validated.
func proposePhotoPersonInTransaction(tx *sql.Tx, proposal PhotoPersonProposal, at int64) (PhotoPerson, bool, error) {
	var deletedAt int64
	if err := tx.QueryRow(`SELECT deleted_at FROM photos WHERE id = ?`, proposal.PhotoID).Scan(&deletedAt); errors.Is(err, sql.ErrNoRows) {
		return PhotoPerson{}, false, fmt.Errorf("%w: no photo %s", ErrNotFound, proposal.PhotoID)
	} else if err != nil {
		return PhotoPerson{}, false, err
	}
	if err := checkFaceOnPhoto(tx, proposal.FaceID, proposal.PhotoID); err != nil {
		return PhotoPerson{}, false, err
	}
	decidedBy := ""
	if proposal.Status == PhotoPersonConfirmed {
		decidedBy = proposal.DecidedBy
	}
	existing, err := scanPhotoPerson(tx.QueryRow(`SELECT `+photoPersonColumns+` FROM photo_people WHERE photo_id = ? AND person_id = ?`,
		proposal.PhotoID, proposal.PersonID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var seq int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM photo_people`).Scan(&seq); err != nil {
			return PhotoPerson{}, false, err
		}
		link := PhotoPerson{ID: formatPhotoPersonID(seq), PhotoID: proposal.PhotoID, PersonID: proposal.PersonID, FaceID: proposal.FaceID,
			Status: proposal.Status, Confidence: proposal.Confidence, Evidence: proposal.Evidence, ProposedBy: proposal.ProposedBy,
			DecidedBy: decidedBy, CreatedAt: at, UpdatedAt: at}
		if _, err := tx.Exec(`INSERT INTO photo_people (id, seq, photo_id, person_id, face_id, status, confidence, evidence, proposed_by, decided_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, link.ID, seq, link.PhotoID, link.PersonID, nullableFaceID(link.FaceID), link.Status,
			link.Confidence, link.Evidence, link.ProposedBy, link.DecidedBy, at, at); err != nil {
			return PhotoPerson{}, false, err
		}
		return link, true, nil
	case err != nil:
		return PhotoPerson{}, false, err
	}
	if existing.Status == PhotoPersonConfirmed && existing.FaceID == "" && proposal.FaceID != "" {
		// A confirmed link that lost its face (a rescan retired it) takes the
		// face a matcher finds for the same person; its status stays.
		if err := keepPhotoPersonRevision(tx, existing, at); err != nil {
			return PhotoPerson{}, false, err
		}
		existing.FaceID, existing.UpdatedAt = proposal.FaceID, at
		if _, err := tx.Exec(`UPDATE photo_people SET face_id = ?, updated_at = ? WHERE id = ?`, existing.FaceID, at, existing.ID); err != nil {
			return PhotoPerson{}, false, err
		}
		return existing, false, nil
	}
	if existing.Status != PhotoPersonSuggested {
		return existing, false, nil
	}
	if err := keepPhotoPersonRevision(tx, existing, at); err != nil {
		return PhotoPerson{}, false, err
	}
	updated := existing
	updated.Status, updated.Confidence, updated.Evidence, updated.ProposedBy = proposal.Status, proposal.Confidence, proposal.Evidence, proposal.ProposedBy
	updated.DecidedBy, updated.UpdatedAt = decidedBy, at
	if proposal.FaceID != "" {
		updated.FaceID = proposal.FaceID
	}
	if _, err := tx.Exec(`UPDATE photo_people SET face_id = ?, status = ?, confidence = ?, evidence = ?, proposed_by = ?, decided_by = ?, updated_at = ? WHERE id = ?`,
		nullableFaceID(updated.FaceID), updated.Status, updated.Confidence, updated.Evidence, updated.ProposedBy, updated.DecidedBy, at, updated.ID); err != nil {
		return PhotoPerson{}, false, err
	}
	return updated, false, nil
}

// DecidePhotoPerson answers PATCH /photo-people/{id}: a person confirming or
// rejecting a link, or pointing it at a face. Any status change needs
// decided_by; a person may change a link however a matcher left it.
func (s *Store) DecidePhotoPerson(id string, decision PhotoPersonDecision) (PhotoPerson, error) {
	if decision.Status != nil && !slices.Contains(PhotoPersonStatuses, *decision.Status) {
		return PhotoPerson{}, fmt.Errorf("%w: status is one of %s", ErrInvalid, strings.Join(PhotoPersonStatuses, ", "))
	}
	if decision.Status != nil && (decision.DecidedBy == nil || strings.TrimSpace(*decision.DecidedBy) == "") {
		return PhotoPerson{}, fmt.Errorf("%w: a status change needs decided_by", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return PhotoPerson{}, err
	}
	defer tx.Rollback()
	link, err := scanPhotoPerson(tx.QueryRow(`SELECT `+photoPersonColumns+` FROM photo_people WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoPerson{}, fmt.Errorf("%w: no link %s", ErrNotFound, id)
	} else if err != nil {
		return PhotoPerson{}, err
	}
	at := now()
	if err := keepPhotoPersonRevision(tx, link, at); err != nil {
		return PhotoPerson{}, err
	}
	if decision.Status != nil {
		link.Status = *decision.Status
		link.DecidedBy = *decision.DecidedBy
		if link.Status == PhotoPersonSuggested {
			link.DecidedBy = ""
		}
	}
	if decision.FaceID != nil {
		if err := checkFaceOnPhoto(tx, *decision.FaceID, link.PhotoID); err != nil {
			return PhotoPerson{}, err
		}
		link.FaceID = *decision.FaceID
	}
	if decision.Evidence != nil {
		link.Evidence = *decision.Evidence
	}
	link.UpdatedAt = at
	if _, err := tx.Exec(`UPDATE photo_people SET face_id = ?, status = ?, evidence = ?, decided_by = ?, updated_at = ? WHERE id = ?`,
		nullableFaceID(link.FaceID), link.Status, link.Evidence, link.DecidedBy, at, link.ID); err != nil {
		return PhotoPerson{}, err
	}
	return link, tx.Commit()
}

// PhotoPersonFilter selects links for GET /photo-people, in id order.
type PhotoPersonFilter struct {
	PhotoID  string
	PersonID string
	Status   string
	Limit    int
	Offset   int
}

// PhotoPeople answers GET /photo-people: links on live photos.
func (s *Store) PhotoPeople(filter PhotoPersonFilter) ([]PhotoPerson, error) {
	where := []string{"photos.deleted_at = 0"}
	args := []any{}
	for column, value := range map[string]string{"photo_people.photo_id": filter.PhotoID, "photo_people.person_id": filter.PersonID, "photo_people.status": filter.Status} {
		if value != "" {
			where = append(where, column+" = ?")
			args = append(args, value)
		}
	}
	if filter.Status != "" && !slices.Contains(PhotoPersonStatuses, filter.Status) {
		return nil, fmt.Errorf("%w: status is one of %s", ErrInvalid, strings.Join(PhotoPersonStatuses, ", "))
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumFaceLimit {
		limit = maximumFaceLimit
	}
	args = append(args, limit, max(filter.Offset, 0))
	rows, err := s.db.Query(`SELECT `+photoPersonColumnsJoinedToPhotos+` FROM photo_people JOIN photos ON photos.id = photo_people.photo_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY photo_people.seq LIMIT ? OFFSET ?`, args...)
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

// PeopleCounts answers GET /people-counts: per person, how many live photos
// they are confirmed and suggested in, most confirmed first.
func (s *Store) PeopleCounts() ([]PersonPhotoCount, error) {
	rows, err := s.db.Query(`SELECT person_id,
		SUM(CASE WHEN status = 'confirmed' THEN 1 ELSE 0 END), SUM(CASE WHEN status = 'suggested' THEN 1 ELSE 0 END)
		FROM photo_people JOIN photos ON photos.id = photo_people.photo_id
		WHERE photos.deleted_at = 0 AND status != 'rejected'
		GROUP BY person_id ORDER BY 2 DESC, 3 DESC, person_id`)
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

// FaceByID answers one face, without its embedding.
func (s *Store) FaceByID(id string) (Face, error) {
	var face Face
	err := s.db.QueryRow(`SELECT id, photo_id, model, box_left, box_top, box_right, box_bottom, detection_score, created_at FROM faces WHERE id = ?`, id).
		Scan(&face.ID, &face.PhotoID, &face.Model, &face.Box.Left, &face.Box.Top, &face.Box.Right, &face.Box.Bottom, &face.DetectionScore, &face.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Face{}, fmt.Errorf("%w: no face %s", ErrNotFound, id)
	}
	return face, err
}
