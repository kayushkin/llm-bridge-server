package model

import (
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Board classification records. The board's taxonomy and policy, the
// decision each classifier answer becomes, the reviews people add, and the
// labels a card carries as a result. The shapes a classifier reads or sends
// are llm-bridge's (msg.BoardClassification, msg.BoardClassificationPolicy,
// msg.BoardClassificationDecisionPublication); the ones only this store and
// its readers use are here.

// TaxonomyUpdateRequest is the body of PUT /api/boards/{id}/classification/taxonomy.
// Taxonomy is the whole taxonomy, archived entries included: an axis or value
// that has an id keeps it, one without gets a new one, and an id the board's
// current taxonomy has may not be left out — archive it instead. Null clears
// the board's taxonomy; the field may not be left out.
type TaxonomyUpdateRequest struct {
	Taxonomy *msg.ClassificationTaxonomy `json:"taxonomy" tstype:"LLMBridgeClassificationTaxonomy | null,required"`
}

// TaxonomyChangePreview says what an accepted taxonomy change leaves behind.
// Nothing is scheduled from it: reclassifying is a separate request.
type TaxonomyChangePreview struct {
	// SemanticChange is whether a classifier would now be shown something
	// different (the digest moved). A change to the taxonomy's own name is not.
	SemanticChange bool `json:"semantic_change"`
	// DecisionsUnderOtherTaxonomies counts this board's decisions whose
	// taxonomy digest is not the new one.
	DecisionsUnderOtherTaxonomies int `json:"decisions_under_other_taxonomies"`
	// LabelsOnRetiredEntries counts cards' labels on this board that name an
	// axis or value the new taxonomy archives. They stay as they are.
	LabelsOnRetiredEntries int `json:"labels_on_retired_entries"`
}

// TaxonomyUpdateResult is what an accepted taxonomy PUT answers.
type TaxonomyUpdateResult struct {
	Classification msg.BoardClassification `json:"classification"`
	Preview        TaxonomyChangePreview   `json:"preview"`
}

// PolicyUpdateRequest is the body of PUT /api/boards/{id}/classification/policy:
// the whole policy, keyed by axis id. An axis left out is unconfigured, which
// is off.
type PolicyUpdateRequest struct {
	AxisPolicies map[string]msg.ClassificationAxisPolicy `json:"axis_policies"`
}

// ClassificationDecision is one classifier answer for one card, as published.
// It never changes after it is written; reviews are separate records.
type ClassificationDecision struct {
	ID             string `json:"id"`
	BoardID        string `json:"board_id"`
	OrganizationID string `json:"organization_id"`
	CardID         string `json:"card_id"`
	// PublicationKey is msg's PublicationKey of what was published: the same
	// operation, card, source, taxonomy, policy and prompt give the same key.
	PublicationKey        string `json:"publication_key"`
	OperationID           string `json:"operation_id"`
	CheckpointID          string `json:"checkpoint_id"`
	AttemptNumber         int    `json:"attempt_number"`
	InitiatingPrincipalID string `json:"initiating_principal_id"`
	SourceDigest          string `json:"source_digest"`
	SourceRevision        string `json:"source_revision,omitempty"`
	// Source is the exact text the classifier was shown. Only the single
	// decision read carries it; a list never does, and a decision whose card
	// was purged has none, with SourceRemovedAt saying when it went.
	Source          *msg.ClassificationSourceSnapshot `json:"source,omitempty"`
	SourceRemovedAt *time.Time                        `json:"source_removed_at,omitempty"`
	// TaxonomyRevision names the board's taxonomy revision the classifier was
	// given; Taxonomy is that revision, on the single decision read only.
	TaxonomyRevision int64                            `json:"taxonomy_revision"`
	TaxonomyDigest   string                           `json:"taxonomy_digest"`
	Taxonomy         *msg.ClassificationTaxonomy      `json:"taxonomy,omitempty"`
	PolicyRevision   int64                            `json:"policy_revision"`
	PromptRevision   string                           `json:"prompt_revision"`
	Model            msg.ClassificationModelEvidence  `json:"model"`
	Origin           msg.ClassificationDecisionOrigin `json:"origin"`
	// Selections maps axis id to value ids, as the classifier chose them.
	Selections map[string][]string `json:"selections"`
	Rationale  string              `json:"rationale,omitempty"`
	// ModelReportedConfidence is the model's own number for the whole card,
	// not per axis and not a measured accuracy. Absent when it gave none.
	ModelReportedConfidence *float64                `json:"model_reported_confidence,omitempty"`
	NeedsReview             bool                    `json:"needs_review"`
	ReviewReasons           []msg.OperationEvidence `json:"review_reasons"`
	CompletedAt             time.Time               `json:"completed_at"`
	PublishedAt             time.Time               `json:"published_at"`
	// ReviewCount is how many reviews name this decision.
	ReviewCount int `json:"review_count"`
}

// ClassificationDecisionPage is one page of a board's decisions, newest first.
type ClassificationDecisionPage struct {
	Decisions []ClassificationDecision `json:"decisions"`
	// Total counts every decision matching the filters, not only this page.
	Total int `json:"total"`
	// NextCursor continues the same query; empty on the last page. It is good
	// only for the board it came from.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ClassificationReviewState filters decisions by what people have done.
type ClassificationReviewState string

const (
	ClassificationReviewStateUnreviewed ClassificationReviewState = "unreviewed"
	ClassificationReviewStateReviewed   ClassificationReviewState = "reviewed"
	// ClassificationReviewStateNeedsReview is a decision the classifier
	// marked for review that nobody has reviewed yet.
	ClassificationReviewStateNeedsReview ClassificationReviewState = "needs_review"
)

var ClassificationReviewStates = []ClassificationReviewState{ClassificationReviewStateUnreviewed, ClassificationReviewStateReviewed, ClassificationReviewStateNeedsReview}

// ClassificationReviewAction is what a reviewer did with a decision.
type ClassificationReviewAction string

const (
	// Accept: the classifier's values on the reviewed axes become the card's labels.
	ClassificationReviewActionAccept ClassificationReviewAction = "accept"
	// Correct: the reviewer's values become the card's labels.
	ClassificationReviewActionCorrect ClassificationReviewAction = "correct"
	// Reject: the classifier's values are wrong and the reviewer gives none;
	// the card's labels stay as they were.
	ClassificationReviewActionReject ClassificationReviewAction = "reject"
)

var ClassificationReviewActions = []ClassificationReviewAction{ClassificationReviewActionAccept, ClassificationReviewActionCorrect, ClassificationReviewActionReject}

// ClassificationReviewRequest is the body of
// POST /api/boards/{id}/classification/decisions/{decision_id}/reviews.
type ClassificationReviewRequest struct {
	// IdempotencyKey is the reviewer's own key for this review. Sending the
	// same key and body again returns the first review; the same key with a
	// different body is a conflict.
	IdempotencyKey string                     `json:"idempotency_key"`
	Action         ClassificationReviewAction `json:"action"`
	// AxisIDs are the axes the reviewer looked at. Only these are reviewed;
	// accepting one axis says nothing about the others.
	AxisIDs []string `json:"axis_ids"`
	// Corrections maps each reviewed axis to the reviewer's value ids, for
	// correct only, and must name exactly AxisIDs.
	Corrections map[string][]string `json:"corrections,omitempty"`
	Explanation string              `json:"explanation,omitempty"`
	// ExpectedLabelsRevision is the card's labels revision the reviewer saw
	// (CardClassificationLabels.revision; 0 when the card had none). Anything
	// else is a conflict, and nothing is written.
	ExpectedLabelsRevision int64 `json:"expected_labels_revision"`
	// SupersedesReviewIDs must name the review behind each label this review
	// would replace, so that one person's correction is only ever replaced by
	// someone who saw it.
	SupersedesReviewIDs []string `json:"supersedes_review_ids,omitempty"`
}

// ClassificationReview is one person's review of one decision. Reviews are
// only ever added.
type ClassificationReview struct {
	ID                  string                     `json:"id"`
	DecisionID          string                     `json:"decision_id"`
	BoardID             string                     `json:"board_id"`
	CardID              string                     `json:"card_id"`
	ReviewerPrincipalID string                     `json:"reviewer_principal_id"`
	Action              ClassificationReviewAction `json:"action"`
	AxisIDs             []string                   `json:"axis_ids"`
	Corrections         map[string][]string        `json:"corrections,omitempty"`
	Explanation         string                     `json:"explanation,omitempty"`
	SupersedesReviewIDs []string                   `json:"supersedes_review_ids"`
	// ExpectedLabelsRevision is what the reviewer saw; ResultingLabelsRevision
	// is the card's labels revision after the review.
	ExpectedLabelsRevision  int64     `json:"expected_labels_revision"`
	ResultingLabelsRevision int64     `json:"resulting_labels_revision"`
	TaxonomyRevision        int64     `json:"taxonomy_revision"`
	PolicyRevision          int64     `json:"policy_revision"`
	IdempotencyKey          string    `json:"idempotency_key"`
	CreatedAt               time.Time `json:"created_at"`
}

// ClassificationLabel is the value a card carries on one axis of one board,
// and the decision and review it came from.
type ClassificationLabel struct {
	AxisID           string    `json:"axis_id"`
	ValueIDs         []string  `json:"value_ids"`
	OriginDecisionID string    `json:"origin_decision_id"`
	OriginReviewID   string    `json:"origin_review_id"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CardClassificationLabels is every label a card carries on one board. Only
// a person's review sets a label; a newer classifier answer is a decision to
// review, never a change here. Revision counts changes from 0.
type CardClassificationLabels struct {
	BoardID  string                `json:"board_id"`
	CardID   string                `json:"card_id"`
	Revision int64                 `json:"revision"`
	Labels   []ClassificationLabel `json:"labels"`
}

// ClassificationDecisionDetail is the single decision read: the decision with
// its source and taxonomy, every review of it, and the card's labels now.
type ClassificationDecisionDetail struct {
	Decision ClassificationDecision   `json:"decision"`
	Reviews  []ClassificationReview   `json:"reviews"`
	Labels   CardClassificationLabels `json:"labels"`
}

// ClassificationReviewResult is what an accepted review answers.
type ClassificationReviewResult struct {
	Review ClassificationReview     `json:"review"`
	Labels CardClassificationLabels `json:"labels"`
}
