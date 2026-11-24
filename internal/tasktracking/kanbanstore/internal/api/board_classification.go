package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// Board classification routes, under /api/boards/{id}/classification. The
// store's rules live in internal/db/classification.go; this file reads
// requests, holds a publication's initiating principal to the board, and
// turns refusals into statuses. Who may call which route is in
// authorizeRequest.

const (
	taxonomyEntityTagPrefix = "taxonomy-"
	policyEntityTagPrefix   = "policy-"
	defaultDecisionPage     = 50
)

func (a *API) boardClassificationRoutes(w http.ResponseWriter, r *http.Request, boardID string, rest []string) {
	switch {
	case len(rest) == 0:
		a.boardClassificationSettings(w, r, boardID)
	case len(rest) == 1 && rest[0] == "taxonomy":
		a.boardClassificationTaxonomy(w, r, boardID)
	case len(rest) == 1 && rest[0] == "policy":
		a.boardClassificationPolicy(w, r, boardID)
	case len(rest) == 1 && rest[0] == "decisions":
		switch r.Method {
		case http.MethodGet:
			a.listClassificationDecisions(w, r, boardID)
		case http.MethodPost:
			a.publishClassificationDecision(w, r, boardID)
		default:
			writeError(w, 405, "method not allowed")
		}
	case len(rest) == 2 && rest[0] == "decisions":
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed")
			return
		}
		detail, err := a.store.ClassificationDecisionDetail(boardID, rest[1])
		if err != nil {
			writeClassificationError(w, err)
			return
		}
		writeJSON(w, 200, detail)
	case len(rest) == 3 && rest[0] == "decisions" && rest[2] == "reviews":
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed")
			return
		}
		a.reviewClassificationDecision(w, r, boardID, rest[1])
	default:
		writeError(w, 404, "not found")
	}
}

// writeClassificationError answers a refusal with its status, a stable code
// and, when there is one, the state the caller should have sent against.
func writeClassificationError(w http.ResponseWriter, err error) {
	var refusal *db.ClassificationRefusal
	switch {
	case errors.As(err, &refusal):
		status := map[db.ClassificationRefusalKind]int{
			db.ClassificationRefusalInvalid:  http.StatusBadRequest,
			db.ClassificationRefusalStale:    http.StatusPreconditionFailed,
			db.ClassificationRefusalConflict: http.StatusConflict,
			db.ClassificationRefusalNotFound: http.StatusNotFound,
		}[refusal.Kind]
		body := map[string]any{"error": refusal.Message, "code": refusal.Code}
		if refusal.Current != nil {
			body["current"] = refusal.Current
		}
		writeJSON(w, status, body)
	case errors.Is(err, db.ErrNotFound):
		writeError(w, 404, "not found")
	case errors.Is(err, db.ErrBadCursor):
		writeJSON(w, 400, map[string]string{"error": err.Error(), "code": "cursor_invalid"})
	default:
		writeError(w, 500, err.Error())
	}
}

func writeClassificationInvalid(w http.ResponseWriter, format string, arguments ...any) {
	writeJSON(w, 400, map[string]string{"error": fmt.Sprintf(format, arguments...), "code": "request_invalid"})
}

// callerClassificationActions is what the caller may do, by the level the
// gate found. The service token and an administrator may do everything.
func callerClassificationActions(r *http.Request, boardID string) []msg.BoardClassificationAction {
	level := BoardAccessAdminister
	if access := principalBoardAccessFrom(r); access != nil {
		level = access.LevelOn(boardID)
	}
	actions := []msg.BoardClassificationAction{}
	if level >= BoardAccessView {
		actions = append(actions, msg.BoardClassificationActionRead)
	}
	if level >= BoardAccessEdit {
		actions = append(actions, msg.BoardClassificationActionClassify, msg.BoardClassificationActionReview)
	}
	if level >= BoardAccessAdminister {
		actions = append(actions, msg.BoardClassificationActionManage)
	}
	return actions
}

// principalOf is who a person-made write is by: the gate's principal, or an
// administrator's own header. Empty for the service token.
func principalOf(r *http.Request) string {
	if calledWithServiceToken(r) {
		return ""
	}
	if access := principalBoardAccessFrom(r); access != nil {
		return access.PrincipalID
	}
	return r.Header.Get(PrincipalIDHeader)
}

// revisionFromIfMatch reads If-Match: "<prefix><revision>". A missing header
// is its own answer (428): a write that says nothing about what it saw could
// overwrite anything.
func revisionFromIfMatch(w http.ResponseWriter, r *http.Request, prefix string) (int64, bool) {
	header := r.Header.Get("If-Match")
	if header == "" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"code": "if_match_required",
			"error": fmt.Sprintf(`send If-Match: "%s<revision>", the revision you read; "%s0" for a board that has none`, prefix, prefix)})
		return 0, false
	}
	unquoted := strings.TrimSuffix(strings.TrimPrefix(header, `"`), `"`)
	revision, err := strconv.ParseInt(strings.TrimPrefix(unquoted, prefix), 10, 64)
	if !strings.HasPrefix(unquoted, prefix) || err != nil || revision < 0 {
		writeClassificationInvalid(w, `If-Match %s is not "%s<revision>"`, header, prefix)
		return 0, false
	}
	return revision, true
}

func entityTag(prefix string, revision int64) string {
	return `"` + prefix + strconv.FormatInt(revision, 10) + `"`
}

func (a *API) boardClassificationSettings(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	classification, err := a.store.BoardClassification(boardID)
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	classification.CallerActions = callerClassificationActions(r, boardID)
	writeJSON(w, 200, classification)
}

func (a *API) boardClassificationTaxonomy(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != http.MethodPut {
		writeError(w, 405, "method not allowed")
		return
	}
	expected, ok := revisionFromIfMatch(w, r, taxonomyEntityTagPrefix)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		writeClassificationInvalid(w, "invalid JSON: %v", err)
		return
	}
	raw, present := fields["taxonomy"]
	if !present || len(fields) != 1 {
		writeClassificationInvalid(w, `the body is {"taxonomy": {…}}, or {"taxonomy": null} to clear it`)
		return
	}
	var taxonomy *msg.ClassificationTaxonomy
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&taxonomy); err != nil {
		writeClassificationInvalid(w, "taxonomy: %v", err)
		return
	}
	result, err := a.store.SetBoardTaxonomy(boardID, expected, taxonomy, principalOf(r))
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	result.Classification.CallerActions = callerClassificationActions(r, boardID)
	w.Header().Set("ETag", entityTag(taxonomyEntityTagPrefix, result.Classification.TaxonomyRevision))
	writeJSON(w, 200, result)
}

func (a *API) boardClassificationPolicy(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != http.MethodPut {
		writeError(w, 405, "method not allowed")
		return
	}
	expected, ok := revisionFromIfMatch(w, r, policyEntityTagPrefix)
	if !ok {
		return
	}
	var request model.PolicyUpdateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeClassificationInvalid(w, "invalid JSON: %v", err)
		return
	}
	classification, err := a.store.SetBoardClassificationPolicy(boardID, expected, request.AxisPolicies, principalOf(r))
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	classification.CallerActions = callerClassificationActions(r, boardID)
	w.Header().Set("ETag", entityTag(policyEntityTagPrefix, classification.Policy.Revision))
	writeJSON(w, 200, classification)
}

func (a *API) listClassificationDecisions(w http.ResponseWriter, r *http.Request, boardID string) {
	query := r.URL.Query()
	limit := defaultDecisionPage
	if text := query.Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > db.MaximumClassificationDecisionPage {
			writeClassificationInvalid(w, "limit must be a whole number from 1 to %d", db.MaximumClassificationDecisionPage)
			return
		}
		limit = parsed
	}
	filter := db.ClassificationDecisionFilter{
		CardID: query.Get("card_id"), ReviewState: model.ClassificationReviewState(query.Get("review_state")),
		AxisID: query.Get("axis_id"), ValueID: query.Get("value_id"),
		SourceDigest: query.Get("source_digest"), OperationID: query.Get("operation_id"),
	}
	if filter.ReviewState != "" {
		known := false
		for _, state := range model.ClassificationReviewStates {
			known = known || state == filter.ReviewState
		}
		if !known {
			writeClassificationInvalid(w, "review_state %q is not one of %v", filter.ReviewState, model.ClassificationReviewStates)
			return
		}
	}
	if filter.ValueID != "" && filter.AxisID == "" {
		writeClassificationInvalid(w, "value_id needs axis_id")
		return
	}
	page, err := a.store.ListClassificationDecisions(boardID, filter, query.Get("cursor"), limit)
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// publishClassificationDecision records a classifier's answer for one card.
// The service token says an internal service sent it; the initiating
// principal says for whom, and must still be able to classify the board now,
// not only when the run was admitted.
func (a *API) publishClassificationDecision(w http.ResponseWriter, r *http.Request, boardID string) {
	if !calledWithServiceToken(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "service_token_required",
			"error": "decisions are published by the classifier with " + ServiceTokenHeader})
		return
	}
	var publication msg.BoardClassificationDecisionPublication
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&publication); err != nil {
		writeClassificationInvalid(w, "invalid JSON: %v", err)
		return
	}
	if publication.BoardID != boardID {
		writeClassificationInvalid(w, "board_id %q is not the board in the path, %s", publication.BoardID, boardID)
		return
	}
	if _, err := a.store.GetBoard(boardID); err != nil {
		writeClassificationError(w, err)
		return
	}
	access, administrator, refusal := a.boardAccessOfPrincipal(publication.InitiatingPrincipalID)
	if refusal != nil && refusal.status == http.StatusBadGateway {
		writeError(w, refusal.status, refusal.message)
		return
	}
	if refusal != nil || (!administrator && access.LevelOn(boardID) < BoardAccessEdit) {
		reason := "does not hold can_edit on the board"
		if refusal != nil {
			reason = refusal.message
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "initiating_principal_may_not_classify",
			"error": fmt.Sprintf("initiating principal %s may not classify board %s now: %s", publication.InitiatingPrincipalID, boardID, reason)})
		return
	}
	decision, created, err := a.store.PublishClassificationDecision(publication)
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, decision)
}

// reviewClassificationDecision records a person's review. The service token
// names no person, so it cannot review.
func (a *API) reviewClassificationDecision(w http.ResponseWriter, r *http.Request, boardID, decisionID string) {
	reviewer := principalOf(r)
	if reviewer == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "reviewer_required",
			"error": "a review is a person's: send " + PrincipalIDHeader + " through the gateway, not the service token"})
		return
	}
	var request model.ClassificationReviewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeClassificationInvalid(w, "invalid JSON: %v", err)
		return
	}
	result, created, err := a.store.ReviewClassificationDecision(boardID, decisionID, reviewer, request)
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, result)
}

// cardClassificationLabels answers GET /api/boards/{id}/cards/{card_id}/classification.
func (a *API) cardClassificationLabels(w http.ResponseWriter, r *http.Request, boardID, cardID string) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	labels, err := a.store.CardClassificationLabels(boardID, cardID)
	if err != nil {
		writeClassificationError(w, err)
		return
	}
	writeJSON(w, 200, labels)
}
