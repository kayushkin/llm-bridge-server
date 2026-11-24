package projectstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Handlers serves the routes CONTRACT.md lists.
type Handlers struct {
	Store  *Store
	Owners *Owners
	// SessionFiler runs the filing passes of POST /session-filings/run.
	SessionFiler *SessionFiler
}

// RegisterHandlers mounts every route on mux.
func RegisterHandlers(mux *http.ServeMux, handlers *Handlers) {
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /vocabulary", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"kinds": Kinds, "stages": Stages, "activity_kinds": ActivityKinds, "suggestion_statuses": SuggestionStatuses,
			"session_filing_statuses": SessionFilingStatuses, "session_filing_evidence_kinds": SessionFilingEvidenceKinds, "default_stage": DefaultStage, "entity_types": EntityTypes,
			"goal_states": GoalStates, "default_goal_state": DefaultGoalState, "goal_link_entity_types": GoalLinkEntityTypes})
	})
	mux.HandleFunc("GET /projects", handlers.listProjects)
	mux.HandleFunc("GET /digest", handlers.digest)
	mux.HandleFunc("GET /projects/{id}/rollup", handlers.projectRollup)
	mux.HandleFunc("POST /projects", handlers.createProject)
	mux.HandleFunc("GET /projects/{id}", handlers.getProject)
	mux.HandleFunc("PATCH /projects/{id}", handlers.patchProject)
	mux.HandleFunc("GET /projects/{id}/changes", handlers.projectChanges)
	mux.HandleFunc("GET /projects/{id}/links", handlers.projectLinks)
	mux.HandleFunc("POST /projects/{id}/links", handlers.addLink)
	mux.HandleFunc("DELETE /projects/{id}/links/{link_id}", handlers.deleteLink)
	mux.HandleFunc("GET /links", handlers.findLinks)
	mux.HandleFunc("GET /projects/{id}/activity", handlers.projectActivity)
	mux.HandleFunc("POST /projects/{id}/next-steps", handlers.addNextStep)
	mux.HandleFunc("GET /projects/{id}/suggestions", handlers.listSuggestions)
	mux.HandleFunc("POST /projects/{id}/suggestions", handlers.requestSuggestions)
	mux.HandleFunc("POST /suggestions/{suggestion_id}/accept", handlers.acceptSuggestion)
	mux.HandleFunc("POST /suggestions/{suggestion_id}/dismiss", handlers.dismissSuggestion)
	mux.HandleFunc("GET /projects/{id}/goals", handlers.listProjectGoals)
	mux.HandleFunc("POST /projects/{id}/goals", handlers.createGoal)
	mux.HandleFunc("GET /goals", handlers.listGoals)
	mux.HandleFunc("GET /goals/{goal_id}", handlers.getGoal)
	mux.HandleFunc("PATCH /goals/{goal_id}", handlers.patchGoal)
	mux.HandleFunc("GET /goals/{goal_id}/changes", handlers.goalChanges)
	mux.HandleFunc("GET /goals/{goal_id}/links", handlers.goalLinks)
	mux.HandleFunc("POST /goals/{goal_id}/links", handlers.addGoalLink)
	mux.HandleFunc("DELETE /goals/{goal_id}/links/{link_id}", handlers.deleteGoalLink)
	mux.HandleFunc("GET /goal-links", handlers.findGoalLinks)
	mux.HandleFunc("GET /session-filings", handlers.listSessionFilings)
	mux.HandleFunc("POST /session-filings/run", handlers.runSessionFiler)
	mux.HandleFunc("POST /session-filings/{session_filing_id}/accept", handlers.acceptSessionFiling)
	mux.HandleFunc("POST /session-filings/{session_filing_id}/dismiss", handlers.dismissSessionFiling)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, ErrOwnerUnavailable):
		status = http.StatusBadGateway
	}
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
	}
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

// decodeFields reads a JSON object and refuses a field the route does not take.
func decodeFields(request *http.Request, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&fields); err != nil {
		return nil, fmt.Errorf("%w: body is not a JSON object: %v", ErrInvalid, err)
	}
	for name := range fields {
		if !contains(allowed, name) {
			return nil, fmt.Errorf("%w: unknown field %q; this route takes %s", ErrInvalid, name, strings.Join(allowed, ", "))
		}
	}
	return fields, nil
}

func stringField(fields map[string]json.RawMessage, name string) (*string, error) {
	raw, present := fields[name]
	if !present {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be a string", ErrInvalid, name)
	}
	return &value, nil
}

var projectFieldNames = []string{"name", "kind", "parent_id", "goal", "done_when", "stage", "spend_target_share"}

func projectFieldsOf(fields map[string]json.RawMessage) (ProjectFields, error) {
	var result ProjectFields
	var err error
	for name, target := range map[string]**string{"name": &result.Name, "kind": &result.Kind, "parent_id": &result.ParentID,
		"goal": &result.Goal, "done_when": &result.DoneWhen, "stage": &result.Stage} {
		if *target, err = stringField(fields, name); err != nil {
			return result, err
		}
	}
	if raw, present := fields["spend_target_share"]; present {
		var share *float64
		if err := json.Unmarshal(raw, &share); err != nil {
			return result, fmt.Errorf("%w: spend_target_share must be a number from 0 to 1, or null", ErrInvalid)
		}
		result.SpendTargetShare = &share
	}
	return result, nil
}

func (handlers *Handlers) listProjects(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	for name := range query {
		if !contains([]string{"q", "kind", "parent_id", "include_archived"}, name) {
			writeError(writer, fmt.Errorf("%w: unknown parameter %q", ErrInvalid, name))
			return
		}
	}
	projects, err := handlers.Store.Projects(ProjectFilter{Query: query.Get("q"), Kind: query.Get("kind"), ParentID: query.Get("parent_id"), IncludeArchived: query.Get("include_archived") == "true"})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"projects": projects})
}

func (handlers *Handlers) createProject(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, append(projectFieldNames, "created_by")...)
	if err != nil {
		writeError(writer, err)
		return
	}
	projectFields, err := projectFieldsOf(fields)
	if err != nil {
		writeError(writer, err)
		return
	}
	createdBy, err := stringField(fields, "created_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	if createdBy == nil {
		empty := ""
		createdBy = &empty
	}
	project, err := handlers.Store.CreateProject(projectFields, *createdBy)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, project)
}

func (handlers *Handlers) getProject(writer http.ResponseWriter, request *http.Request) {
	project, err := handlers.Store.Project(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, project)
}

func (handlers *Handlers) patchProject(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, append(projectFieldNames, "archived", "changed_by")...)
	if err != nil {
		writeError(writer, err)
		return
	}
	projectFields, err := projectFieldsOf(fields)
	if err != nil {
		writeError(writer, err)
		return
	}
	var archived *bool
	if raw, present := fields["archived"]; present {
		if err := json.Unmarshal(raw, &archived); err != nil || archived == nil {
			writeError(writer, fmt.Errorf("%w: archived must be true or false", ErrInvalid))
			return
		}
	}
	changedBy, err := stringField(fields, "changed_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	if changedBy == nil {
		empty := ""
		changedBy = &empty
	}
	project, err := handlers.Store.UpdateProject(request.PathValue("id"), projectFields, archived, *changedBy)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, project)
}

func (handlers *Handlers) projectChanges(writer http.ResponseWriter, request *http.Request) {
	changes, err := handlers.Store.Changes(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"changes": changes})
}

func (handlers *Handlers) projectLinks(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if _, err := handlers.Store.Project(id); err != nil {
		writeError(writer, err)
		return
	}
	links, err := handlers.Store.Links(LinkFilter{ProjectID: id, EntityType: request.URL.Query().Get("entity_type")})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"links": links})
}

func (handlers *Handlers) addLink(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "entity_type", "entity_ref", "note", "created_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	link := Link{ProjectID: request.PathValue("id")}
	for name, target := range map[string]*string{"entity_type": &link.EntityType, "entity_ref": &link.EntityRef, "note": &link.Note, "created_by": &link.CreatedBy} {
		value, err := stringField(fields, name)
		if err != nil {
			writeError(writer, err)
			return
		}
		if value != nil {
			*target = strings.TrimSpace(*value)
		}
	}
	if link.EntityType == "" || link.EntityRef == "" {
		writeError(writer, fmt.Errorf("%w: entity_type and entity_ref are required", ErrInvalid))
		return
	}
	if !entityTypeKnown(link.EntityType) {
		writeError(writer, fmt.Errorf("%w: entity_type %q is not one a project can own (GET /vocabulary)", ErrInvalid, link.EntityType))
		return
	}
	if _, err := handlers.Store.Project(link.ProjectID); err != nil {
		writeError(writer, err)
		return
	}
	if link.Label, err = handlers.Owners.Label(request.Context(), link.EntityType, link.EntityRef); err != nil {
		writeError(writer, err)
		return
	}
	created, err := handlers.Store.AddLink(link)
	if err != nil {
		writeError(writer, err)
		return
	}
	if created.EntityType == EntitySession {
		// Someone filed the session directly, so what the filer proposed
		// for it no longer waits on anyone.
		if err := handlers.Store.SupersedeProposedSessionFilings(created.EntityRef, created.CreatedBy); err != nil {
			writeError(writer, fmt.Errorf("link %d was made, but closing the session's filing proposals failed: %w", created.ID, err))
			return
		}
	}
	writeJSON(writer, http.StatusCreated, created)
}

func (handlers *Handlers) deleteLink(writer http.ResponseWriter, request *http.Request) {
	linkID, err := strconv.ParseInt(request.PathValue("link_id"), 10, 64)
	if err != nil {
		writeError(writer, fmt.Errorf("%w: link id must be a number", ErrInvalid))
		return
	}
	if err := handlers.Store.DeleteLink(request.PathValue("id"), linkID); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// findLinks is the reverse lookup: which projects own this entity, or, without
// entity_ref, every link of one type (every filed session, to group a list of
// sessions by project in one call).
func (handlers *Handlers) findLinks(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	filter := LinkFilter{EntityType: query.Get("entity_type"), EntityRef: query.Get("entity_ref")}
	if filter.EntityType == "" {
		writeError(writer, fmt.Errorf("%w: entity_type is required", ErrInvalid))
		return
	}
	links, err := handlers.Store.Links(filter)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"links": links})
}
