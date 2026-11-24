package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/model"
)

// Bulk card commands: one command applied to many cards.
//
// A bulk move must obey everything a single move does — the WIP limit, the
// column's auto_status, the event on the timeline, the message triggers, and
// above all the caller's access to that one card. Writing those rules a second
// time here is how the two would come to differ. So this route re-implements
// none of them: for each card it builds the request the single-card route
// takes and runs it through the same authorizeRequest and the same handlers,
// as the same caller, and reports what that route answered.
//
// It follows that a bulk request is not one transaction. Each card is its own
// command; one that is refused undoes nothing done to the others, and the
// answer says which is which.

// capturedResponse is an http.ResponseWriter that keeps what a handler wrote.
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCapturedResponse() *capturedResponse {
	return &capturedResponse{header: http.Header{}, status: http.StatusOK}
}

func (c *capturedResponse) Header() http.Header         { return c.header }
func (c *capturedResponse) WriteHeader(status int)      { c.status = status }
func (c *capturedResponse) Write(b []byte) (int, error) { return c.body.Write(b) }

func (c *capturedResponse) decoded() any {
	var decoded any
	if err := json.Unmarshal(c.body.Bytes(), &decoded); err != nil {
		// Every handler here answers JSON. One that did not is reported as it
		// was rather than dropped.
		return map[string]string{"unparsed_response": c.body.String()}
	}
	return decoded
}

func (a *API) bulkCardCommands(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, model.BulkCardCommandOptions{Commands: model.BulkCardCommands, MaxCards: model.MaxBulkCardCommandCards})
	case http.MethodPost:
		var request model.BulkCardCommandRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if err := validateBulkCardCommandRequest(&request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		response := model.BulkCardCommandResponse{Command: request.Command, Results: []model.BulkCardCommandResult{}}
		for index, cardID := range request.CardIDs {
			result := a.runBulkCommandOnCard(r, &request, index, cardID)
			if result.Status >= 200 && result.Status < 300 {
				response.Succeeded++
			} else {
				response.Failed++
			}
			response.Results = append(response.Results, result)
		}
		writeJSON(w, 200, response)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func validateBulkCardCommandRequest(request *model.BulkCardCommandRequest) error {
	if len(request.CardIDs) == 0 {
		return fmt.Errorf("card_ids is required")
	}
	if len(request.CardIDs) > model.MaxBulkCardCommandCards {
		return fmt.Errorf("card_ids has %d entries; one request takes at most %d", len(request.CardIDs), model.MaxBulkCardCommandCards)
	}
	seen := map[string]bool{}
	for _, cardID := range request.CardIDs {
		if cardID == "" || strings.Contains(cardID, "/") {
			return fmt.Errorf("card_ids has an entry %q that is not a card id", cardID)
		}
		if seen[cardID] {
			return fmt.Errorf("card_ids names %s twice", cardID)
		}
		seen[cardID] = true
	}

	// Exactly the field the command reads, and no other.
	set := map[string]bool{
		"move": request.Move != nil, "principal_id": request.PrincipalID != "",
		"reason": request.Reason != "", "tags": len(request.Tags) > 0,
	}
	reads := map[model.BulkCardCommand]struct {
		field    string
		required bool
	}{
		model.BulkCommandMove:       {"move", true},
		model.BulkCommandAssign:     {"principal_id", true},
		model.BulkCommandUnassign:   {"principal_id", true},
		model.BulkCommandHold:       {"reason", false},
		model.BulkCommandUnhold:     {"", false},
		model.BulkCommandAddTags:    {"tags", true},
		model.BulkCommandRemoveTags: {"tags", true},
	}
	read, known := reads[request.Command]
	if !known {
		return fmt.Errorf("command %q is not one of %v (GET /api/bulk-card-commands)", request.Command, model.BulkCardCommands)
	}
	if read.required && !set[read.field] {
		return fmt.Errorf("command %q needs %s", request.Command, read.field)
	}
	for field, isSet := range set {
		if isSet && field != read.field {
			return fmt.Errorf("command %q does not read %s; send only what the command reads, so nothing is half-obeyed", request.Command, field)
		}
	}
	if request.Move != nil {
		if err := request.Move.Validate(); err != nil {
			return err
		}
	}
	if request.PrincipalID != "" && !principalIDShape.MatchString(request.PrincipalID) {
		return fmt.Errorf("principal_id %q is not a principal-store id (principal_000001)", boundedtext.Value(request.PrincipalID))
	}
	for _, tag := range request.Tags {
		if strings.TrimSpace(tag) == "" || tag != strings.TrimSpace(tag) {
			return fmt.Errorf("tags has an empty or untrimmed entry %q", tag)
		}
	}
	return nil
}

// runAsSingleCardRequest runs one single-card request as the caller of the
// bulk request: the same access rules, then the same handler.
func (a *API) runAsSingleCardRequest(bulk *http.Request, method, path string, headers map[string]string, body any) *capturedResponse {
	captured := newCapturedResponse()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			writeError(captured, 500, err.Error())
			return captured
		}
	}
	target := path
	if actor := bulk.URL.Query().Get("actor"); actor != "" {
		target += "?actor=" + url.QueryEscape(actor)
	}
	request, err := http.NewRequestWithContext(bulk.Context(), method, target, bytes.NewReader(encoded))
	if err != nil {
		writeError(captured, 500, err.Error())
		return captured
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	// A nil access is the service token or an administrator, whom no rule binds.
	if access := principalBoardAccessFrom(bulk); access != nil {
		refusal, err := a.authorizeRequest(request, access)
		if err != nil {
			writeError(captured, 500, err.Error())
			return captured
		}
		if refusal != nil {
			writeError(captured, refusal.status, refusal.message)
			return captured
		}
	}
	a.routes().ServeHTTP(captured, request)
	return captured
}

func (a *API) runBulkCommandOnCard(bulk *http.Request, request *model.BulkCardCommandRequest, index int, cardID string) model.BulkCardCommandResult {
	cardPath := "/api/cards/" + url.PathEscape(cardID)
	var answer *capturedResponse
	switch request.Command {
	case model.BulkCommandMove:
		move := *request.Move
		move.Position += float64(index)
		answer = a.runAsSingleCardRequest(bulk, "POST", cardPath+"/move", nil, move)
	case model.BulkCommandAssign:
		answer = a.runAsSingleCardRequest(bulk, "PUT", cardPath+"/assignments/"+url.PathEscape(request.PrincipalID), nil, nil)
	case model.BulkCommandUnassign:
		answer = a.runAsSingleCardRequest(bulk, "DELETE", cardPath+"/assignments/"+url.PathEscape(request.PrincipalID), nil, nil)
	case model.BulkCommandHold:
		answer = a.runAsSingleCardRequest(bulk, "POST", cardPath+"/hold", nil, map[string]string{"reason": request.Reason})
	case model.BulkCommandUnhold:
		answer = a.runAsSingleCardRequest(bulk, "POST", cardPath+"/unhold", nil, nil)
	case model.BulkCommandAddTags, model.BulkCommandRemoveTags:
		answer = a.changeTagsOfCard(bulk, cardPath, request.Command == model.BulkCommandAddTags, request.Tags)
	}
	result := model.BulkCardCommandResult{CardID: cardID, Status: answer.status}
	if answer.body.Len() > 0 {
		result.Response = answer.decoded()
	}
	return result
}

// changeTagsOfCard adds or removes tags without touching the card's others.
// noteboard's PATCH replaces the whole tag list, so this reads the card and
// writes the new list made against the version it read: if someone else saves
// in between, noteboard answers 412 for this card and their tags survive.
func (a *API) changeTagsOfCard(bulk *http.Request, cardPath string, adding bool, tags []string) *capturedResponse {
	read := a.runAsSingleCardRequest(bulk, "GET", cardPath, nil, nil)
	if read.status != http.StatusOK {
		return read
	}
	var detail struct {
		Item *struct {
			Tags      []string `json:"tags"`
			UpdatedAt string   `json:"updated_at"`
		} `json:"item"`
	}
	if err := json.Unmarshal(read.body.Bytes(), &detail); err != nil {
		failed := newCapturedResponse()
		writeError(failed, 500, "could not read the card's tags: "+err.Error())
		return failed
	}
	if detail.Item == nil {
		missing := newCapturedResponse()
		writeError(missing, 404, "the card's noteboard item is gone, so it has no tags to change")
		return missing
	}
	named := map[string]bool{}
	for _, tag := range tags {
		named[tag] = true
	}
	kept := []string{}
	held := map[string]bool{}
	for _, tag := range detail.Item.Tags {
		if !adding && named[tag] {
			continue
		}
		kept = append(kept, tag)
		held[tag] = true
	}
	if adding {
		added := []string{}
		for tag := range named {
			if !held[tag] {
				added = append(added, tag)
			}
		}
		sort.Strings(added)
		kept = append(kept, added...)
	}
	headers := map[string]string{}
	if detail.Item.UpdatedAt != "" {
		headers["If-Match"] = `"` + detail.Item.UpdatedAt + `"`
	}
	return a.runAsSingleCardRequest(bulk, "PATCH", cardPath, headers, map[string]any{"tags": kept})
}
