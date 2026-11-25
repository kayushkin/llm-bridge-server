package photostore

// Wire types for face groups: faces a grouper thinks are one person, waiting
// for a person to say who. ./generate-ts.sh renders this file to ts/model.ts.

// Statuses a face group may hold.
const (
	// FaceGroupOpen waits for a person to say who it is.
	FaceGroupOpen = "open"
	// FaceGroupNamed was named: its faces were confirmed as the person.
	FaceGroupNamed = "named"
	// FaceGroupNotKnown is someone the owner does not know or does not want
	// named, such as a stranger in the background.
	FaceGroupNotKnown = "not_known"
	// FaceGroupMixed holds more than one person and was left as it is.
	FaceGroupMixed = "mixed"
)

// FaceGroupStatuses is every status, in GET /face-group-statuses.
var FaceGroupStatuses = []string{FaceGroupOpen, FaceGroupNamed, FaceGroupNotKnown, FaceGroupMixed}

// FaceGroup is a set of faces a grouper found to be one person.
type FaceGroup struct {
	// ID is face_group_000001.
	ID string `json:"id"`
	// Model is the face model whose faces it groups.
	Model string `json:"model"`
	// Status is one of GET /face-group-statuses.
	Status string `json:"status"`
	// PersonID is people-store's person_ id the group was named as; "" until
	// it is named.
	PersonID   string `json:"person_id"`
	MadeBy     string `json:"made_by"`
	DecidedBy  string `json:"decided_by"`
	DecidedAt  int64  `json:"decided_at"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	FaceCount  int    `json:"face_count"`
	PhotoCount int    `json:"photo_count"`
	// FirstTakenAt and LastTakenAt span when its photos were taken, epoch
	// seconds; 0 when none has a date.
	FirstTakenAt int64 `json:"first_taken_at"`
	LastTakenAt  int64 `json:"last_taken_at"`
	// SampleFaceIDs are up to 12 of its faces, one per photo, spread evenly
	// from the most typical to the least.
	SampleFaceIDs []string `json:"sample_face_ids"`
	// Guesses are who models and context think it is, newest first.
	Guesses []FaceGroupGuess `json:"guesses"`
	// Members is every face in it, filled only by GET /face-groups/{id}.
	Members []FaceGroupMember `json:"members,omitempty"`
}

// FaceGroupMember is one face in a group.
type FaceGroupMember struct {
	FaceID  string `json:"face_id"`
	PhotoID string `json:"photo_id"`
	// Similarity is how close the face is to the group's average face, the
	// cosine similarity of their embeddings.
	Similarity float64 `json:"similarity"`
	TakenAt    int64   `json:"taken_at"`
	Folder     string  `json:"folder"`
}

// FaceGroupGuess is one guess of who a group is.
type FaceGroupGuess struct {
	// ID is face_group_guess_000001.
	ID      string `json:"id"`
	GroupID string `json:"group_id"`
	// PersonID is people-store's person_ id; "" means the guesser thinks it
	// is nobody in people-store.
	PersonID   string  `json:"person_id"`
	Confidence float64 `json:"confidence"`
	// Evidence says why, in markdown with [text](ref:<id>) chips.
	Evidence   string `json:"evidence"`
	ProposedBy string `json:"proposed_by"`
	CreatedAt  int64  `json:"created_at"`
}

// NewFaceGroupMember is one face in POST /face-groups or POST
// /face-groups/{id}/members.
type NewFaceGroupMember struct {
	FaceID     string  `json:"face_id"`
	Similarity float64 `json:"similarity"`
}

// NewFaceGroup is the body of POST /face-groups.
type NewFaceGroup struct {
	Model   string               `json:"model"`
	MadeBy  string               `json:"made_by"`
	Members []NewFaceGroupMember `json:"members"`
}

// FaceGroupMembersAddition is the body of POST /face-groups/{id}/members:
// faces a grouper found to belong to an open group.
type FaceGroupMembersAddition struct {
	Members []NewFaceGroupMember `json:"members"`
}

// FaceGroupGuessProposal is the body of POST /face-groups/{id}/guesses.
type FaceGroupGuessProposal struct {
	PersonID   string  `json:"person_id"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
	ProposedBy string  `json:"proposed_by"`
}

// FaceGroupDecision is the body of POST /face-groups/{id}/decision: a
// person saying who a group is. Status named needs PersonID. Faces in
// ExcludedFaceIDs are taken out of the group first, as not the same person.
// A named group can be named again as someone else, with no faces left out:
// a correction, or two records of one person made one.
type FaceGroupDecision struct {
	Status          string   `json:"status"`
	PersonID        string   `json:"person_id"`
	DecidedBy       string   `json:"decided_by"`
	ExcludedFaceIDs []string `json:"excluded_face_ids"`
}

// FaceGroupDecisionResult answers a decision: the group as it now stands
// and what naming it did to the links of its photos.
type FaceGroupDecisionResult struct {
	Group FaceGroup `json:"group"`
	// Confirmed counts links created or moved up to confirmed.
	Confirmed int `json:"confirmed"`
	// AlreadyConfirmed counts photos the person was confirmed on already.
	AlreadyConfirmed int `json:"already_confirmed"`
	// KeptRejected counts photos where a person had rejected this person,
	// which a group does not undo.
	KeptRejected int `json:"kept_rejected"`
	// OthersRejected counts suggestions of someone else on the group's
	// faces, rejected because each face is one person.
	OthersRejected int `json:"others_rejected"`
	// Excluded counts faces taken out of the group.
	Excluded int `json:"excluded"`
	// PreviousPersonRejected counts, when a named group is named as someone
	// else, the links to the person it was named as on its faces, now
	// rejected.
	PreviousPersonRejected int `json:"previous_person_rejected"`
}
