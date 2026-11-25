package photostore

// Wire types for faces and for who is in a photo. ./generate-ts.sh renders
// this file with photo.go to ts/model.ts.

// FaceBox is where a face is on the thumbnail, in fractions of its width and
// height; a face cut off at an edge may run a little past 0 or 1.
type FaceBox struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

// ScannedFace is one face in POST /face-scans.
type ScannedFace struct {
	Box            FaceBox `json:"box"`
	DetectionScore float64 `json:"detection_score"`
	// EmbeddingFloat32Base64 is the model's description of the face:
	// little-endian float32s, base64. The store normalizes it to length 1.
	EmbeddingFloat32Base64 string `json:"embedding_float32_base64"`
}

// FaceScan is the body of POST /face-scans: what one model found on one
// photo, an empty Faces when it found none. A photo is scanned once per
// model; a second scan is a 409 unless Replaces is set, which retires the
// faces of the scan before it and keeps the new ones under new ids.
type FaceScan struct {
	PhotoID  string        `json:"photo_id"`
	Model    string        `json:"model"`
	Faces    []ScannedFace `json:"faces"`
	Replaces bool          `json:"replaces"`
}

// Face is a face found on a photo. EmbeddingFloat32Base64 is filled only when
// GET /faces is asked for it with include_embeddings=true.
type Face struct {
	// ID is face_000001.
	ID                     string  `json:"id"`
	PhotoID                string  `json:"photo_id"`
	Model                  string  `json:"model"`
	Box                    FaceBox `json:"box"`
	DetectionScore         float64 `json:"detection_score"`
	EmbeddingFloat32Base64 string  `json:"embedding_float32_base64,omitempty"`
	CreatedAt              int64   `json:"created_at"`
}

// PhotoPerson says a person in people-store is in a photo.
type PhotoPerson struct {
	// ID is photo_person_000001.
	ID      string `json:"id"`
	PhotoID string `json:"photo_id"`
	// PersonID is people-store's id, person_000001.
	PersonID string `json:"person_id"`
	// FaceID is the face that is the person, "" when not known.
	FaceID string `json:"face_id"`
	// Status is one of GET /photo-person-statuses: suggested, confirmed or
	// rejected. A rejected link stays rejected whatever a matcher proposes.
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	// Evidence says why, in markdown with [text](ref:<id>) chips.
	Evidence   string `json:"evidence"`
	ProposedBy string `json:"proposed_by"`
	// DecidedBy is who confirmed or rejected it: a person, or the matcher
	// that confirmed it when its checks agreed. "" while suggested.
	DecidedBy string `json:"decided_by"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// PhotoPersonProposal is the body of POST /photo-people. Status is suggested
// or confirmed; confirmed needs DecidedBy. A proposal for a pair that is
// already linked updates it only upward: suggested may become confirmed, and
// a rejected or confirmed link is kept as it is.
type PhotoPersonProposal struct {
	PhotoID    string  `json:"photo_id"`
	PersonID   string  `json:"person_id"`
	FaceID     string  `json:"face_id"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
	ProposedBy string  `json:"proposed_by"`
	DecidedBy  string  `json:"decided_by"`
}

// PhotoPersonDecision is the body of PATCH /photo-people/{id}: a status
// change needs DecidedBy.
type PhotoPersonDecision struct {
	Status    *string `json:"status,omitempty"`
	DecidedBy *string `json:"decided_by,omitempty"`
	FaceID    *string `json:"face_id,omitempty"`
	Evidence  *string `json:"evidence,omitempty"`
}

// PersonPhotoCount is one row of GET /people-counts: how many live photos a
// person is confirmed or suggested in.
type PersonPhotoCount struct {
	PersonID  string `json:"person_id"`
	Confirmed int    `json:"confirmed"`
	Suggested int    `json:"suggested"`
}

// Statuses a photo-person link may hold.
const (
	PhotoPersonSuggested = "suggested"
	PhotoPersonConfirmed = "confirmed"
	PhotoPersonRejected  = "rejected"
)

// PhotoPersonStatuses is every status, in GET /photo-person-statuses.
var PhotoPersonStatuses = []string{PhotoPersonSuggested, PhotoPersonConfirmed, PhotoPersonRejected}
