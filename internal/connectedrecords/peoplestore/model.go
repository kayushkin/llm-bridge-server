package peoplestore

import "encoding/json"

// The wire types. They live outside store.go so that tygo (tygo.yaml) renders
// them to ts/model.ts; store.go is excluded there.

// Person is someone in the operator's own life.
type Person struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// Birthday is "" (unknown), "YYYY-MM-DD", or "--MM-DD" when the year is
	// unknown — the vCard 4 form for a date without a year.
	Birthday string   `json:"birthday"`
	HowMet   string   `json:"how_met"`
	Tags     []string `json:"tags"`
	// PrincipalID is principal-store's id for this person, when they have one
	// there (they log in, or a card can be assigned to them). "" when not.
	PrincipalID string `json:"principal_id"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	// DeletedAt is 0 for a live person. A deleted person still answers
	// GET /persons/{id}, so whatever points at them keeps resolving.
	DeletedAt int64 `json:"deleted_at"`
}

// PersonDetail answers GET /persons/{id} and every write to a person: the
// person with their identities and relationships, each [] when there are none.
type PersonDetail struct {
	Person        `tstype:",extends"`
	Identities    []Identity         `json:"identities"`
	Relationships []RelationshipView `json:"relationships"`
}

// PersonSnapshot is a person with their identities, as a revision keeps them.
type PersonSnapshot struct {
	Person     `tstype:",extends"`
	Identities []Identity `json:"identities"`
}

// PersonInput is the body of POST /persons.
type PersonInput struct {
	DisplayName string   `json:"display_name"`
	Birthday    string   `json:"birthday"`
	HowMet      string   `json:"how_met"`
	Tags        []string `json:"tags"`
	PrincipalID string   `json:"principal_id"`
	// Identities are attached in the same transaction; any refusal or
	// conflict refuses the whole write.
	Identities []IdentityLinkInput `json:"identities"`
}

// PersonPatch is the body of PATCH /persons/{id}. A field left out is left
// alone; birthday and principal_id take "" or null to clear. Any other key is
// a 400.
type PersonPatch struct {
	DisplayName *string   `json:"display_name,omitempty"`
	Birthday    *string   `json:"birthday,omitempty"`
	HowMet      *string   `json:"how_met,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	PrincipalID *string   `json:"principal_id,omitempty"`
}

// Identity is one handle that reaches a person. Value is stored normalized.
type Identity struct {
	ID       string `json:"id"`
	PersonID string `json:"person_id"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Label    string `json:"label"`
	// LinkedBy is one of LinkSources: who attached the handle to this person.
	LinkedBy string `json:"linked_by"`
	// Reason is why, in the linker's words.
	Reason string `json:"reason"`
	// LinkedAt is when the link was made, epoch seconds; an import may carry
	// a time from before the row was created here.
	LinkedAt  int64 `json:"linked_at"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// IdentityInput is one handle in a suggestion's payload, and the handle half
// of IdentityLinkInput.
type IdentityInput struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Label string `json:"label"`
}

// IdentityLinkInput is the body of POST /persons/{id}/identities and one entry
// of POST /persons identities: a handle and who links it, and why.
type IdentityLinkInput struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Label string `json:"label"`
	// LinkedBy is required, one of GET /link-sources.
	LinkedBy string `json:"linked_by"`
	// Reason is required from every source but the operator.
	Reason string `json:"reason"`
	// LinkedAt is when the link was made at its source, epoch seconds; 0 means
	// now. A time in the future is a 400.
	LinkedAt int64 `json:"linked_at"`
	// Move takes the handle from the person who has it, writing a refusal
	// against them. Only a source that overrides refusals may move.
	Move bool `json:"move"`
}

// LinkSource is one entry of GET /link-sources.
type LinkSource struct {
	Source string `json:"source"`
	// OverridesRefusals: this source may link a refused pair (deleting the
	// refusal) and move a handle between persons.
	OverridesRefusals bool `json:"overrides_refusals"`
	// ReasonRequired: a link from this source without a reason is a 400.
	ReasonRequired bool   `json:"reason_required"`
	Description    string `json:"description"`
}

// Refusal says a handle is not a person, so no source but the operator may
// attach it to them.
type Refusal struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	PersonID  string `json:"person_id"`
	RefusedBy string `json:"refused_by"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"created_at"`
}

// IdentityResolution answers GET /identities/resolve: the identity and the
// person who owns it.
type IdentityResolution struct {
	Identity Identity `json:"identity"`
	Person   Person   `json:"person"`
}

// IdentityKind is one entry of GET /identity-kinds.
type IdentityKind struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// RelationshipKind is one entry of GET /relationship-kinds. Inverse is the
// kind the same relationship has seen from the other person.
type RelationshipKind struct {
	Kind        string `json:"kind"`
	Inverse     string `json:"inverse"`
	Description string `json:"description"`
}

// Relationship is one stored row: FromPersonID is the Kind of ToPersonID.
type Relationship struct {
	ID           string `json:"id"`
	FromPersonID string `json:"from_person_id"`
	ToPersonID   string `json:"to_person_id"`
	Kind         string `json:"kind"`
	Note         string `json:"note"`
	CreatedAt    int64  `json:"created_at"`
}

// RelationshipView is a relationship seen from one person: PersonID is the
// other person, and Kind is what they are to this one. A row (A, B, parent)
// shows on B as {person_id: A, kind: parent} and on A as {person_id: B, kind:
// child}. PersonDisplayName is the other person's name, for display only.
type RelationshipView struct {
	ID                string `json:"id"`
	PersonID          string `json:"person_id"`
	PersonDisplayName string `json:"person_display_name"`
	PersonDeletedAt   int64  `json:"person_deleted_at"`
	Kind              string `json:"kind"`
	Note              string `json:"note"`
	CreatedAt         int64  `json:"created_at"`
}

// RelationshipInput is the body of POST /persons/{id}/relationships: "person_id
// is this person's kind". {"person_id":"person_000002","kind":"parent"} on
// person_000001 records that person_000002 is person_000001's parent.
type RelationshipInput struct {
	PersonID string `json:"person_id"`
	Kind     string `json:"kind"`
	Note     string `json:"note"`
}

// SuggestionPayload is what an importer proposes: these handles are one
// person, perhaps called DisplayName.
type SuggestionPayload struct {
	DisplayName string          `json:"display_name"`
	Identities  []IdentityInput `json:"identities"`
	// MatchedPersonIDs are persons that already own another handle from the
	// same group at the source: a hint that accepting with one of them as
	// person_id extends that person. Filled by the store, never by the caller.
	MatchedPersonIDs []string `json:"matched_person_ids"`
	// SourceDetail is whatever the importer wants a reviewer to see, kept as
	// the importer sent it.
	SourceDetail json.RawMessage `json:"source_detail,omitempty"`
	// Reason is why the source thinks these handles are one person; required
	// from a reviewer source (agent).
	Reason string `json:"reason,omitempty"`
	// Covered are handles this suggestion proposed that were attached to a
	// person some other way while it waited; they left Identities then.
	Covered []CoveredHandle `json:"covered,omitempty"`
}

// CoveredHandle is a handle that left a waiting suggestion because it was
// attached to PersonID some other way at CoveredAt.
type CoveredHandle struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	PersonID  string `json:"person_id"`
	CoveredAt int64  `json:"covered_at"`
}

// Suggestion is a proposed merge of handles into one person.
type Suggestion struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	Payload   SuggestionPayload `json:"payload"`
	Status    string            `json:"status"`
	PersonID  string            `json:"person_id"`
	CreatedAt int64             `json:"created_at"`
	DecidedAt int64             `json:"decided_at"`
	// Activity is summed at read time over the suggestion's handles from
	// handle_activity; all zero when no source reported any.
	Activity SuggestionActivity `json:"activity"`
}

// SuggestionActivity is how much the operator hears from a suggestion's
// handles: the newest activity of any of them, the messages all of them sent,
// and whether any has a one-to-one chat with the operator.
type SuggestionActivity struct {
	// LastActivityAt is epoch seconds; 0 when none was seen.
	LastActivityAt int64 `json:"last_activity_at"`
	MessageCount   int64 `json:"message_count"`
	DirectChat     bool  `json:"direct_chat"`
}

// HandleActivity is what a source saw of one handle. Kind and Value are
// normalized as identities are.
type HandleActivity struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	// LastActivityAt is epoch seconds; 0 when none was seen.
	LastActivityAt int64 `json:"last_activity_at"`
	// MessageCount is how many messages this handle sent, as the source logged them.
	MessageCount int64 `json:"message_count"`
	// DirectChat is true when the operator has a one-to-one chat with this handle.
	DirectChat bool `json:"direct_chat"`
}

// HandleActivityReport is the body of PUT /handle-activity. Each handle's row
// is replaced whole.
type HandleActivityReport struct {
	Source     string           `json:"source"`
	Activities []HandleActivity `json:"activities"`
}

// HandleActivityResult answers PUT /handle-activity.
type HandleActivityResult struct {
	Updated int `json:"updated"`
}

// SuggestionInput is the body of POST /suggestions.
type SuggestionInput struct {
	Source       string          `json:"source"`
	DisplayName  string          `json:"display_name"`
	Identities   []IdentityInput `json:"identities"`
	SourceDetail json.RawMessage `json:"source_detail,omitempty"`
	Reason       string          `json:"reason"`
}

// SuggestionProposal answers POST /suggestions. Created is false, and
// Suggestion nil, when every handle was already attached to a person or
// already in a suggestion; Skipped lists those handles either way.
type SuggestionProposal struct {
	Created    bool            `json:"created"`
	Suggestion *Suggestion     `json:"suggestion"`
	Skipped    []IdentityInput `json:"skipped"`
}

// AcceptSuggestionInput is the body of POST /suggestions/{id}/accept. With
// PersonID empty, accepting creates a person named after the suggestion.
type AcceptSuggestionInput struct {
	PersonID string `json:"person_id"`
}

// Revision is a person, identities included, as it was just before Action
// changed it.
type Revision struct {
	ID        string         `json:"id"`
	PersonID  string         `json:"person_id"`
	Action    string         `json:"action"`
	Snapshot  PersonSnapshot `json:"snapshot"`
	CreatedAt int64          `json:"created_at"`
}

// Counts answers the counts half of GET /health.
type Counts struct {
	Persons             int `json:"persons"`
	DeletedPersons      int `json:"deleted_persons"`
	Identities          int `json:"identities"`
	Relationships       int `json:"relationships"`
	ProposedSuggestions int `json:"proposed_suggestions"`
	Refusals            int `json:"refusals"`
}

// Handle names one handle by kind and value, as identities are keyed.
type Handle struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// PendingReview is a handle the operator talks with that nobody has reviewed
// or claimed: its activity, and the person it is attached to ("" for none).
type PendingReview struct {
	Kind           string `json:"kind"`
	Value          string `json:"value"`
	PersonID       string `json:"person_id"`
	LastActivityAt int64  `json:"last_activity_at"`
	MessageCount   int64  `json:"message_count"`
	DirectChat     bool   `json:"direct_chat"`
}

// ReviewQueue answers GET /reviews/pending.
type ReviewQueue struct {
	// Total is every pending handle; Pending is the first `limit` of them.
	Total int `json:"total"`
	// ActiveClaims is how many handles are claimed right now.
	ActiveClaims int             `json:"active_claims"`
	Pending      []PendingReview `json:"pending"`
}

// ReviewClaimInput is the body of POST /reviews/claim.
type ReviewClaimInput struct {
	Limit     int    `json:"limit"`
	ClaimedBy string `json:"claimed_by"`
}

// ReviewClaim answers POST /reviews/claim: the handles now claimed, and how
// many still wait.
type ReviewClaim struct {
	Claimed   []PendingReview `json:"claimed"`
	Remaining int             `json:"remaining"`
}

// ReviewInput is the body of POST /reviews.
type ReviewInput struct {
	Handles    []Handle `json:"handles"`
	ReviewedBy string   `json:"reviewed_by"`
	// ReviewedAt is epoch seconds; 0 means now. A time in the future is a 400.
	ReviewedAt int64 `json:"reviewed_at"`
}

// ReviewResult answers POST /reviews.
type ReviewResult struct {
	Reviewed int `json:"reviewed"`
}

// AcceptedSuggestion answers POST /suggestions/{id}/accept: the suggestion,
// now accepted, and the person it created or extended.
type AcceptedSuggestion struct {
	Suggestion Suggestion   `json:"suggestion"`
	Person     PersonDetail `json:"person"`
}
