package projectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// A project's next steps are noteboard todos linked to it, the same way a
// decision or a design note is: noteboard holds what a step says and whether
// it is done, and the link here says whose step it is. Nothing about a step is
// copied into this store.

// LinkedNote is a noteboard item one of the project's note links names, with
// noteboard's fields as noteboard gives them.
type LinkedNote struct {
	LinkID int64  `json:"link_id"`
	NoteID string `json:"note_id"`
	// Type is noteboard's: note, todo, rank or workspace.
	Type  string `json:"type"`
	Title string `json:"title"`
	// Status is noteboard's: open, done or archived.
	Status   string   `json:"status"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags"`
	// Times are noteboard's RFC 3339 strings; DueAt and CompletedAt are ""
	// when not set.
	DueAt       string `json:"due_at"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at"`
}

// noteboardItem is the part of a noteboard item a LinkedNote carries.
type noteboardItem struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Tags        []string `json:"tags"`
	DueAt       *string  `json:"due_at"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	CompletedAt *string  `json:"completed_at"`
}

func (item noteboardItem) linkedNote(linkID int64) LinkedNote {
	note := LinkedNote{LinkID: linkID, NoteID: item.ID, Type: item.Type, Title: item.Title, Status: item.Status, Priority: item.Priority,
		Tags: item.Tags, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	if note.Tags == nil {
		note.Tags = []string{}
	}
	if item.DueAt != nil {
		note.DueAt = *item.DueAt
	}
	if item.CompletedAt != nil {
		note.CompletedAt = *item.CompletedAt
	}
	return note
}

// LinkedNotes reads every note link's item from noteboard in one call. Open
// todos come first, the most urgent first; the rest follow, newest first.
// Links whose item noteboard no longer has are returned as missing ids.
func (owners *Owners) LinkedNotes(ctx context.Context, links []Link) ([]LinkedNote, []string, error) {
	notes, missing := []LinkedNote{}, []string{}
	linkByNote := map[string]int64{}
	ids := []string{}
	for _, link := range links {
		if link.EntityType == EntityNote {
			linkByNote[link.EntityRef] = link.ID
			ids = append(ids, link.EntityRef)
		}
	}
	if len(ids) == 0 {
		return notes, missing, nil
	}
	var answer struct {
		Items      []noteboardItem `json:"items"`
		MissingIDs []string        `json:"missing_ids"`
	}
	query := map[string]any{"ids": ids, "include_items": true, "limit": len(ids)}
	if err := owners.sendJSON(ctx, http.MethodPost, owners.NoteboardURL+"/api/items/query", nil, query, http.StatusOK, &answer); err != nil {
		return nil, nil, fmt.Errorf("notes linked to the project: %w", err)
	}
	for _, item := range answer.Items {
		notes = append(notes, item.linkedNote(linkByNote[item.ID]))
	}
	if answer.MissingIDs != nil {
		missing = answer.MissingIDs
	}
	sort.SliceStable(notes, func(left, right int) bool {
		leftOpen, rightOpen := notes[left].isOpenTodo(), notes[right].isOpenTodo()
		if leftOpen != rightOpen {
			return leftOpen
		}
		if leftOpen && notes[left].Priority != notes[right].Priority {
			return notes[left].Priority > notes[right].Priority
		}
		return notes[left].UpdatedAt > notes[right].UpdatedAt
	})
	return notes, missing, nil
}

func (note LinkedNote) isOpenTodo() bool { return note.Type == "todo" && note.Status == "open" }

// sendJSON sends body as JSON and decodes the answer into target, which may be
// nil. A status other than wantStatus is ErrOwnerUnavailable, with the body.
func (owners *Owners) sendJSON(ctx context.Context, method, rawURL string, headers map[string]string, body any, wantStatus int, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := owners.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrOwnerUnavailable, method, rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 500))
		return fmt.Errorf("%w: %s %s: %s: %s", ErrOwnerUnavailable, method, rawURL, response.Status, answer)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrOwnerUnavailable, method, rawURL, err)
	}
	return nil
}

// NextStepTag is the tag a todo made as a project's next step carries, so it
// reads as one on noteboard too. The project link, not the tag, says whose
// step it is.
const NextStepTag = "next-step"

// NextStep is what a caller gives to make a next step.
type NextStep struct {
	Title     string
	Body      string
	Priority  *int
	CreatedBy string
	// LinkNote is kept on the project link: why the step was added.
	LinkNote string
}

// AddNextStep makes a noteboard todo and links it to the project. When the
// todo is made but the link cannot be written, the error names the todo, so
// nothing is left that nobody knows about.
func AddNextStep(ctx context.Context, store *Store, owners *Owners, projectID string, step NextStep) (LinkedNote, error) {
	step.Title = strings.TrimSpace(step.Title)
	if step.Title == "" {
		return LinkedNote{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if _, err := store.Project(projectID); err != nil {
		return LinkedNote{}, err
	}
	todo := map[string]any{"type": "todo", "title": step.Title, "body": step.Body, "tags": []string{NextStepTag}}
	if step.Priority != nil {
		todo["priority"] = *step.Priority
	}
	var item noteboardItem
	if err := owners.sendJSON(ctx, http.MethodPost, owners.NoteboardURL+"/api/items", nil, todo, http.StatusCreated, &item); err != nil {
		return LinkedNote{}, fmt.Errorf("make the todo: %w", err)
	}
	link, err := store.AddLink(Link{ProjectID: projectID, EntityType: EntityNote, EntityRef: item.ID, Label: item.Title, Note: step.LinkNote, CreatedBy: step.CreatedBy})
	if err != nil {
		return LinkedNote{}, fmt.Errorf("noteboard made todo %s, but linking it to %s failed: %w", item.ID, projectID, err)
	}
	return item.linkedNote(link.ID), nil
}

func (handlers *Handlers) addNextStep(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "title", "body", "priority", "created_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	var step NextStep
	for name, target := range map[string]*string{"title": &step.Title, "body": &step.Body, "created_by": &step.CreatedBy} {
		value, err := stringField(fields, name)
		if err != nil {
			writeError(writer, err)
			return
		}
		if value != nil {
			*target = *value
		}
	}
	if raw, present := fields["priority"]; present {
		if err := json.Unmarshal(raw, &step.Priority); err != nil {
			writeError(writer, fmt.Errorf("%w: priority must be a whole number", ErrInvalid))
			return
		}
	}
	step.LinkNote = "next step"
	note, err := AddNextStep(request.Context(), handlers.Store, handlers.Owners, request.PathValue("id"), step)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, note)
}

// now is the time owners treat as now: Clock when a test sets it.
func (owners *Owners) now() time.Time {
	if owners.Clock != nil {
		return owners.Clock()
	}
	return time.Now()
}
