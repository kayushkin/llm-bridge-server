package projects

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

var goalFieldNames = []string{"parent_goal_id", "statement", "done_when", "state", "priority", "target_at"}

// wholeNumberField reads a JSON whole number; nil when the field is absent.
func wholeNumberField(fields map[string]json.RawMessage, name string) (*int64, error) {
	raw, present := fields[name]
	if !present {
		return nil, nil
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, fmt.Errorf("%w: %s must be a whole number", ErrInvalid, name)
	}
	return value, nil
}

func goalFieldsOf(fields map[string]json.RawMessage) (GoalFields, error) {
	var result GoalFields
	var err error
	for name, target := range map[string]**string{"parent_goal_id": &result.ParentGoalID, "statement": &result.Statement, "done_when": &result.DoneWhen,
		"state": &result.State, "closed_because": &result.ClosedBecause} {
		if *target, err = stringField(fields, name); err != nil {
			return result, err
		}
	}
	priority, err := wholeNumberField(fields, "priority")
	if err != nil {
		return result, err
	}
	if priority != nil {
		value := int(*priority)
		result.Priority = &value
	}
	if result.TargetAt, err = wholeNumberField(fields, "target_at"); err != nil {
		return result, err
	}
	return result, nil
}

// goalStatesAsked reads ?state=: one state narrows to it, none lists every
// state but dismissed.
func goalStatesAsked(state string) ([]string, error) {
	if state == "" {
		return goalStatesListedByDefault, nil
	}
	if !contains(GoalStates, state) {
		return nil, fmt.Errorf("%w: state %q is not one of %s", ErrInvalid, state, strings.Join(GoalStates, ", "))
	}
	return []string{state}, nil
}

func refuseUnknownParameters(request *http.Request, allowed ...string) error {
	for name := range request.URL.Query() {
		if !contains(allowed, name) {
			if len(allowed) == 0 {
				return fmt.Errorf("%w: unknown parameter %q; this route takes none", ErrInvalid, name)
			}
			return fmt.Errorf("%w: unknown parameter %q; this route takes %s", ErrInvalid, name, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func (handlers *Handlers) ListProjectGoals(writer http.ResponseWriter, request *http.Request) {
	if err := refuseUnknownParameters(request, "state"); err != nil {
		writeError(writer, err)
		return
	}
	projectID := request.PathValue("id")
	if _, err := handlers.Store.Project(projectID); err != nil {
		writeError(writer, err)
		return
	}
	states, err := goalStatesAsked(request.URL.Query().Get("state"))
	if err != nil {
		writeError(writer, err)
		return
	}
	goals, err := handlers.Store.Goals(GoalFilter{ProjectID: projectID, States: states})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"goals": goals})
}

func (handlers *Handlers) ListGoals(writer http.ResponseWriter, request *http.Request) {
	if err := refuseUnknownParameters(request, "state", "project_id"); err != nil {
		writeError(writer, err)
		return
	}
	query := request.URL.Query()
	states, err := goalStatesAsked(query.Get("state"))
	if err != nil {
		writeError(writer, err)
		return
	}
	projectID := query.Get("project_id")
	if projectID != "" {
		if _, err := handlers.Store.Project(projectID); err != nil {
			writeError(writer, err)
			return
		}
	}
	goals, err := handlers.Store.Goals(GoalFilter{ProjectID: projectID, States: states})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"goals": goals})
}

func (handlers *Handlers) CreateGoal(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, append(goalFieldNames, "created_by")...)
	if err != nil {
		writeError(writer, err)
		return
	}
	goalFields, err := goalFieldsOf(fields)
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
	goal, err := handlers.Store.CreateGoal(request.PathValue("id"), goalFields, *createdBy)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, goal)
}

func (handlers *Handlers) GetGoal(writer http.ResponseWriter, request *http.Request) {
	goal, err := handlers.Store.Goal(request.PathValue("goal_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, goal)
}

func (handlers *Handlers) PatchGoal(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, append(goalFieldNames, "closed_because", "changed_by")...)
	if err != nil {
		writeError(writer, err)
		return
	}
	goalFields, err := goalFieldsOf(fields)
	if err != nil {
		writeError(writer, err)
		return
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
	goal, err := handlers.Store.UpdateGoal(request.PathValue("goal_id"), goalFields, *changedBy)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, goal)
}

func (handlers *Handlers) GoalChanges(writer http.ResponseWriter, request *http.Request) {
	changes, err := handlers.Store.GoalChanges(request.PathValue("goal_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"changes": changes})
}

func (handlers *Handlers) GoalLinks(writer http.ResponseWriter, request *http.Request) {
	if err := refuseUnknownParameters(request, "entity_type"); err != nil {
		writeError(writer, err)
		return
	}
	goalID := request.PathValue("goal_id")
	if _, err := handlers.Store.Goal(goalID); err != nil {
		writeError(writer, err)
		return
	}
	links, err := handlers.Store.GoalLinks(GoalLinkFilter{GoalID: goalID, EntityType: request.URL.Query().Get("entity_type")})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"links": links})
}

func (handlers *Handlers) AddGoalLink(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "entity_type", "entity_ref", "note", "created_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	link := GoalLink{GoalID: request.PathValue("goal_id")}
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
	if !goalLinkEntityTypeKnown(link.EntityType) {
		writeError(writer, fmt.Errorf("%w: entity_type %q is not one that can serve a goal (GET /vocabulary, goal_link_entity_types)", ErrInvalid, link.EntityType))
		return
	}
	if _, err := handlers.Store.Goal(link.GoalID); err != nil {
		writeError(writer, err)
		return
	}
	if link.Label, err = handlers.Owners.Label(request.Context(), link.EntityType, link.EntityRef); err != nil {
		writeError(writer, err)
		return
	}
	created, err := handlers.Store.AddGoalLink(link)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, created)
}

func (handlers *Handlers) DeleteGoalLink(writer http.ResponseWriter, request *http.Request) {
	linkID, err := strconv.ParseInt(request.PathValue("link_id"), 10, 64)
	if err != nil {
		writeError(writer, fmt.Errorf("%w: link id must be a number", ErrInvalid))
		return
	}
	if err := handlers.Store.DeleteGoalLink(request.PathValue("goal_id"), linkID); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// findGoalLinks is the reverse lookup: which goals this entity serves, or,
// without entity_ref, every goal link of one type.
func (handlers *Handlers) FindGoalLinks(writer http.ResponseWriter, request *http.Request) {
	if err := refuseUnknownParameters(request, "entity_type", "entity_ref"); err != nil {
		writeError(writer, err)
		return
	}
	query := request.URL.Query()
	filter := GoalLinkFilter{EntityType: query.Get("entity_type"), EntityRef: query.Get("entity_ref")}
	if filter.EntityType == "" {
		writeError(writer, fmt.Errorf("%w: entity_type is required", ErrInvalid))
		return
	}
	links, err := handlers.Store.GoalLinks(filter)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"links": links})
}
