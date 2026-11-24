package projectstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Suggested next steps: a model reads what a project is for and where it
// stands, and proposes steps. They wait here until a person accepts one, which
// makes it a noteboard todo linked to the project (a next step like any
// other), or dismisses it. A suggestion is a proposal, never work: nothing
// reads an undecided one as a step.

// Statuses of a suggestion.
const (
	SuggestionProposed   = "proposed"
	SuggestionAccepted   = "accepted"
	SuggestionDismissed  = "dismissed"
	SuggestionSuperseded = "superseded"
)

// SuggestionStatuses lists every status.
var SuggestionStatuses = []string{SuggestionProposed, SuggestionAccepted, SuggestionDismissed, SuggestionSuperseded}

// SuggestionCaller is how llm-bridge-server's logs name the calls.
const SuggestionCaller = "project-store-next-step-suggestions"

// MaximumSuggestions is the most next steps one call asks for.
const MaximumSuggestions = 5

// Suggestion is one proposed next step.
type Suggestion struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	// Why is the model's reason, in a sentence or two.
	Why    string `json:"why"`
	Status string `json:"status"`
	// Model is the model that answered.
	Model       string `json:"model"`
	RequestedBy string `json:"requested_by"`
	CreatedAt   int64  `json:"created_at"`
	DecidedBy   string `json:"decided_by"`
	DecidedAt   int64  `json:"decided_at"`
	// NoteID is the noteboard todo an accepted suggestion became.
	NoteID string `json:"note_id"`
}

const suggestionColumns = `id, project_id, title, why, status, model, requested_by, created_at, decided_by, decided_at, note_id`

func scanSuggestion(row scanner) (Suggestion, error) {
	var suggestion Suggestion
	err := row.Scan(&suggestion.ID, &suggestion.ProjectID, &suggestion.Title, &suggestion.Why, &suggestion.Status, &suggestion.Model,
		&suggestion.RequestedBy, &suggestion.CreatedAt, &suggestion.DecidedBy, &suggestion.DecidedAt, &suggestion.NoteID)
	return suggestion, err
}

// Suggestions lists a project's suggestions in one status ("" for all),
// oldest first.
func (store *Store) Suggestions(projectID, status string) ([]Suggestion, error) {
	if status != "" && !contains(SuggestionStatuses, status) {
		return nil, fmt.Errorf("%w: status %q is not one of %s", ErrInvalid, status, strings.Join(SuggestionStatuses, ", "))
	}
	query := `SELECT ` + suggestionColumns + ` FROM suggestions WHERE project_id = ?`
	arguments := []any{projectID}
	if status != "" {
		query += ` AND status = ?`
		arguments = append(arguments, status)
	}
	rows, err := store.database.Query(query+` ORDER BY id`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suggestions := []Suggestion{}
	for rows.Next() {
		suggestion, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, suggestion)
	}
	return suggestions, rows.Err()
}

// Suggestion returns one suggestion.
func (store *Store) Suggestion(id string) (Suggestion, error) {
	suggestion, err := scanSuggestion(store.database.QueryRow(`SELECT `+suggestionColumns+` FROM suggestions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Suggestion{}, fmt.Errorf("suggestion %s: %w", id, ErrNotFound)
	}
	return suggestion, err
}

// ProposedStep is one step as the model gave it.
type ProposedStep struct {
	Title string `json:"title"`
	Why   string `json:"why"`
}

// ReplaceProposals stores a new round of suggestions for a project and marks
// the round before it that nobody decided on as superseded.
func (store *Store) ReplaceProposals(projectID string, steps []ProposedStep, model, requestedBy string) ([]Suggestion, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	now := store.now().Unix()
	if _, err := transaction.Exec(`UPDATE suggestions SET status = ?, decided_at = ? WHERE project_id = ? AND status = ?`,
		SuggestionSuperseded, now, projectID, SuggestionProposed); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, step := range steps {
		title := strings.TrimSpace(step.Title)
		if title == "" {
			continue
		}
		id, err := nextID(transaction, "suggestion")
		if err != nil {
			return nil, err
		}
		if _, err := transaction.Exec(`INSERT INTO suggestions (id, project_id, title, why, status, model, requested_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, projectID, title, strings.TrimSpace(step.Why), SuggestionProposed, model, requestedBy, now); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	suggestions := []Suggestion{}
	for _, id := range ids {
		suggestion, err := store.Suggestion(id)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, suggestion)
	}
	return suggestions, nil
}

// DecideSuggestion records that a person accepted or dismissed a proposal. A
// suggestion already decided is a conflict.
func (store *Store) DecideSuggestion(id, status, decidedBy, noteID string) (Suggestion, error) {
	result, err := store.database.Exec(`UPDATE suggestions SET status = ?, decided_by = ?, decided_at = ?, note_id = ? WHERE id = ? AND status = ?`,
		status, decidedBy, store.now().Unix(), noteID, id, SuggestionProposed)
	if err != nil {
		return Suggestion{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Suggestion{}, err
	} else if affected == 0 {
		current, err := store.Suggestion(id)
		if err != nil {
			return Suggestion{}, err
		}
		return Suggestion{}, fmt.Errorf("%w: suggestion %s is already %s", ErrConflict, id, current.Status)
	}
	return store.Suggestion(id)
}

var suggestionSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"suggestions":{"type":"array","maxItems":%d,"items":{"type":"object",
"properties":{"title":{"type":"string","description":"The step, as a todo title: an imperative sentence of at most 15 words."},
"why":{"type":"string","description":"One or two sentences: what in the project's state calls for this step."}},
"required":["title","why"]}}},"required":["suggestions"]}`, MaximumSuggestions))

const suggestionSystemPrompt = `You help one person run their software projects on a single host. Given one project's goal, what "done" means for it, and where its work stands, propose the next steps that would move it toward done most. Prefer steps that finish work already started (unmerged branches, open pull requests, undeployed commits, open todos waiting on something) over new work. Never repeat a step already listed as an open todo or card. Each step must be something one agent session could do or decide. A repo that was "never deployed" is often a library that goes live inside another repo's deploy, so do not propose deploying it on that ground alone. Write plainly; no figures of speech.`

// SuggestionPrompt is what the model reads about the project.
func SuggestionPrompt(rollup Rollup, activity []ActivityEvent, now time.Time) string {
	var prompt strings.Builder
	prompt.WriteString(DigestText([]Project{rollup.Project}, map[string]Rollup{rollup.Project.ID: rollup}, now))
	write := func(heading string, lines []string) {
		if len(lines) == 0 {
			fmt.Fprintf(&prompt, "\n%s: none\n", heading)
			return
		}
		fmt.Fprintf(&prompt, "\n%s:\n", heading)
		for _, line := range lines {
			fmt.Fprintf(&prompt, "- %s\n", line)
		}
	}
	var openTodos, otherNotes, openCards, pullRequests, recent []string
	for _, note := range rollup.Notes {
		switch {
		case note.isOpenTodo():
			openTodos = append(openTodos, fmt.Sprintf("%s (priority %d)", note.Title, note.Priority))
		case note.Type == "note" && note.Status == "open":
			otherNotes = append(otherNotes, note.Title)
		}
	}
	for _, card := range rollup.Cards {
		if card.Status == "open" {
			openCards = append(openCards, fmt.Sprintf("%s [%s]", card.Title, card.WorkState))
		}
	}
	for _, repo := range rollup.Repos {
		for _, pullRequest := range repo.OpenPullRequests {
			pullRequests = append(pullRequests, fmt.Sprintf("%s #%d %s (by %s, opened %s ago)", repo.Name, pullRequest.Number, pullRequest.Title,
				pullRequest.Author, roundedAge(now.Sub(time.Unix(pullRequest.CreatedAt, 0)))))
		}
	}
	for _, event := range activity {
		if len(recent) == 40 {
			break
		}
		ago := roundedAge(now.Sub(time.Unix(event.At, 0)))
		switch event.Kind {
		case ActivityCommitLanded:
			recent = append(recent, fmt.Sprintf("%s ago: commit on %s: %s", ago, event.RepoName, event.Subject))
		case ActivityDeployed:
			recent = append(recent, fmt.Sprintf("%s ago: deployed %s at %.7s", ago, event.RepoName, event.CommitSHA))
		case ActivityProjectChanged:
			recent = append(recent, fmt.Sprintf("%s ago: %s changed from %q to %q", ago, event.Field, event.OldValue, event.NewValue))
		case ActivityNoteCompleted:
			recent = append(recent, fmt.Sprintf("%s ago: finished todo: %s", ago, event.Label))
		case ActivityPullRequestMerged, ActivityPullRequestOpened, ActivityPullRequestClosed:
			recent = append(recent, fmt.Sprintf("%s ago: %s %s #%d %s", ago, strings.TrimPrefix(event.Kind, "pull_request_"), event.RepoName, event.PullRequestNumber, event.PullRequestTitle))
		case ActivitySuggestionDismissed:
			recent = append(recent, fmt.Sprintf("%s ago: the person dismissed a suggested step: %s", ago, event.Label))
		}
	}
	write("Open todos (the project's next steps already)", openTodos)
	write("Open cards", openCards)
	write("Notes (decisions and designs)", otherNotes)
	write("Open pull requests", pullRequests)
	write("Recent activity, newest first", recent)
	fmt.Fprintf(&prompt, "\nPropose up to %d next steps.\n", MaximumSuggestions)
	return prompt.String()
}

// SuggestNextSteps asks a model for next steps and stores them as proposals.
func SuggestNextSteps(ctx context.Context, store *Store, owners *Owners, projectID, requestedBy string) ([]Suggestion, error) {
	if owners.SuggestionModelRole == "" {
		return nil, fmt.Errorf("%w: no model role is set for suggestions (PROJECT_STORE_SUGGESTION_MODEL_ROLE)", ErrInvalid)
	}
	rollup, err := BuildRollup(ctx, store, owners, projectID)
	if err != nil {
		return nil, err
	}
	now := owners.now()
	activity, err := BuildActivity(ctx, store, owners, projectID, now.AddDate(0, 0, -DefaultActivityDays))
	if err != nil {
		return nil, err
	}
	request := map[string]any{"prompt": SuggestionPrompt(rollup, activity, now), "system_prompt": suggestionSystemPrompt,
		"model_role": owners.SuggestionModelRole, "caller": SuggestionCaller, "schema": suggestionSchema}
	var answer struct {
		Parsed json.RawMessage `json:"parsed"`
		Model  string          `json:"model"`
	}
	modelOwners := *owners
	modelOwners.HTTP = owners.ModelHTTP
	if err := modelOwners.sendJSON(ctx, http.MethodPost, owners.BridgeURL+"/oneshot", map[string]string{BridgeServiceTokenHeader: owners.BridgeServiceToken},
		request, http.StatusOK, &answer); err != nil {
		return nil, fmt.Errorf("ask a model for next steps: %w", err)
	}
	var parsed struct {
		Suggestions []ProposedStep `json:"suggestions"`
	}
	if len(answer.Parsed) == 0 {
		return nil, fmt.Errorf("%w: the model (%s) answered with no structured output", ErrOwnerUnavailable, answer.Model)
	}
	if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
		return nil, fmt.Errorf("%w: the model's (%s) answer does not fit the schema: %v", ErrOwnerUnavailable, answer.Model, err)
	}
	if len(parsed.Suggestions) > MaximumSuggestions {
		parsed.Suggestions = parsed.Suggestions[:MaximumSuggestions]
	}
	return store.ReplaceProposals(projectID, parsed.Suggestions, answer.Model, requestedBy)
}

func (handlers *Handlers) requestSuggestions(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "requested_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	requestedBy, err := stringField(fields, "requested_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	if requestedBy == nil {
		empty := ""
		requestedBy = &empty
	}
	suggestions, err := SuggestNextSteps(request.Context(), handlers.Store, handlers.Owners, request.PathValue("id"), *requestedBy)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"suggestions": suggestions})
}

func (handlers *Handlers) listSuggestions(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	for name := range query {
		if name != "status" {
			writeError(writer, fmt.Errorf("%w: unknown parameter %q; this route takes status", ErrInvalid, name))
			return
		}
	}
	id := request.PathValue("id")
	if _, err := handlers.Store.Project(id); err != nil {
		writeError(writer, err)
		return
	}
	suggestions, err := handlers.Store.Suggestions(id, query.Get("status"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"suggestions": suggestions})
}

func (handlers *Handlers) acceptSuggestion(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "decided_by", "priority")
	if err != nil {
		writeError(writer, err)
		return
	}
	decidedBy, err := stringField(fields, "decided_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	var step NextStep
	if decidedBy != nil {
		step.CreatedBy = *decidedBy
	}
	if raw, present := fields["priority"]; present {
		if err := json.Unmarshal(raw, &step.Priority); err != nil {
			writeError(writer, fmt.Errorf("%w: priority must be a whole number", ErrInvalid))
			return
		}
	}
	suggestion, err := handlers.Store.Suggestion(request.PathValue("suggestion_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	if suggestion.Status != SuggestionProposed {
		writeError(writer, fmt.Errorf("%w: suggestion %s is already %s", ErrConflict, suggestion.ID, suggestion.Status))
		return
	}
	step.Title = suggestion.Title
	step.Body = fmt.Sprintf("%s\n\nSuggested by %s for %s (%s).", suggestion.Why, suggestion.Model, suggestion.ProjectID, suggestion.ID)
	step.LinkNote = "accepted " + suggestion.ID
	note, err := AddNextStep(request.Context(), handlers.Store, handlers.Owners, suggestion.ProjectID, step)
	if err != nil {
		writeError(writer, err)
		return
	}
	decided, err := handlers.Store.DecideSuggestion(suggestion.ID, SuggestionAccepted, step.CreatedBy, note.NoteID)
	if err != nil {
		writeError(writer, fmt.Errorf("todo %s was made, but recording %s as accepted failed: %w", note.NoteID, suggestion.ID, err))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"suggestion": decided, "note": note})
}

func (handlers *Handlers) dismissSuggestion(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "decided_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	decidedBy, err := stringField(fields, "decided_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	if decidedBy == nil {
		empty := ""
		decidedBy = &empty
	}
	decided, err := handlers.Store.DecideSuggestion(request.PathValue("suggestion_id"), SuggestionDismissed, *decidedBy, "")
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, decided)
}
