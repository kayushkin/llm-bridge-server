package projectstore

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// A project's activity is worked out on request, like its rollup: what
// changed in the project's own fields and links, what landed on and was
// deployed from its repos, what happened to their pull requests, and which of
// its todos were finished and suggestions decided. Nothing in it is stored.

// Kinds of activity event.
const (
	ActivityProjectChanged      = "project_changed"
	ActivityLinkAdded           = "link_added"
	ActivityCommitLanded        = "commit_landed"
	ActivityDeployed            = "deployed"
	ActivityPullRequestOpened   = "pull_request_opened"
	ActivityPullRequestMerged   = "pull_request_merged"
	ActivityPullRequestClosed   = "pull_request_closed"
	ActivityNoteCompleted       = "note_completed"
	ActivitySuggestionAccepted  = "suggestion_accepted"
	ActivitySuggestionDismissed = "suggestion_dismissed"
	ActivityGoalCreated         = "goal_created"
	ActivityGoalStateChanged    = "goal_state_changed"
	ActivityGoalLinkAdded       = "goal_link_added"
)

// ActivityKinds lists every kind of activity event.
var ActivityKinds = []string{ActivityProjectChanged, ActivityLinkAdded, ActivityCommitLanded, ActivityDeployed,
	ActivityPullRequestOpened, ActivityPullRequestMerged, ActivityPullRequestClosed, ActivityNoteCompleted,
	ActivitySuggestionAccepted, ActivitySuggestionDismissed, ActivityGoalCreated, ActivityGoalStateChanged, ActivityGoalLinkAdded}

// ActivityEvent is one thing that happened in a project. Kind says which of
// the other fields are set; the rest are empty.
type ActivityEvent struct {
	Kind string `json:"kind"`
	// At is unix seconds.
	At int64 `json:"at"`
	// Actor is who did it as its store records them: a change's changed_by,
	// a deploy's deployed_by, a commit's author, a pull request's author.
	Actor string `json:"actor"`
	// SessionID is the session that did it, when its store knows one.
	SessionID string `json:"session_id"`

	RepoStoreID int64  `json:"repo_store_id"`
	RepoName    string `json:"repo_name"`
	CommitSHA   string `json:"commit_sha"`
	// Subject is a commit's subject line.
	Subject string `json:"subject"`

	PullRequestNumber int64  `json:"pull_request_number"`
	PullRequestTitle  string `json:"pull_request_title"`
	URL               string `json:"url"`

	// Field, OldValue and NewValue describe a project_changed event, and a
	// goal_state_changed one (field state).
	Field    string `json:"field"`
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`

	// EntityType, EntityRef and Label describe a link_added or goal_link_added
	// event, and a note_completed one (entity_type note). On goal_created and
	// goal_state_changed, Label is the goal's statement.
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Label      string `json:"label"`

	SuggestionID string `json:"suggestion_id"`

	// GoalID is set on goal_created, goal_state_changed and goal_link_added.
	GoalID string `json:"goal_id"`
}

// DefaultActivityDays and MaximumActivityDays bound ?days= on the activity route.
const (
	DefaultActivityDays = 14
	MaximumActivityDays = 90
)

// deploymentsRead is how many of a repo's newest deploys the activity reads.
const deploymentsRead = 200

// BuildActivity works out what happened in a project since the given time,
// newest first.
func BuildActivity(ctx context.Context, store *Store, owners *Owners, projectID string, since time.Time) ([]ActivityEvent, error) {
	events := []ActivityEvent{}
	sinceUnix := since.Unix()
	changes, err := store.Changes(projectID)
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		if change.ChangedAt >= sinceUnix {
			events = append(events, ActivityEvent{Kind: ActivityProjectChanged, At: change.ChangedAt, Actor: change.ChangedBy,
				Field: change.Field, OldValue: change.OldValue, NewValue: change.NewValue})
		}
	}
	links, err := store.Links(LinkFilter{ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	repoNames := map[string]string{}
	for _, link := range links {
		if link.EntityType != EntityRepo {
			continue
		}
		repoStoreID, err := strconv.ParseInt(link.EntityRef, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("repo link %d holds %q, not a repo-store id", link.ID, link.EntityRef)
		}
		if repoNames[link.EntityRef], err = owners.RepoName(ctx, repoStoreID); err != nil {
			return nil, err
		}
	}
	for _, link := range links {
		if link.CreatedAt >= sinceUnix {
			event := ActivityEvent{Kind: ActivityLinkAdded, At: link.CreatedAt, Actor: link.CreatedBy, EntityType: link.EntityType, EntityRef: link.EntityRef, Label: link.Label}
			if link.EntityType == EntityRepo {
				// The repo's name now, not when it was linked.
				event.Label = repoNames[link.EntityRef]
			}
			if link.EntityType == EntitySession {
				event.SessionID = link.EntityRef
			}
			events = append(events, event)
		}
	}

	notes, _, err := owners.LinkedNotes(ctx, links)
	if err != nil {
		return nil, err
	}
	for _, note := range notes {
		if note.CompletedAt == "" {
			continue
		}
		completedAt, err := time.Parse(time.RFC3339Nano, note.CompletedAt)
		if err != nil {
			return nil, fmt.Errorf("noteboard item %s has completed_at %q, not an RFC 3339 time: %w", note.NoteID, note.CompletedAt, err)
		}
		if completedAt.Unix() >= sinceUnix {
			events = append(events, ActivityEvent{Kind: ActivityNoteCompleted, At: completedAt.Unix(), EntityType: EntityNote, EntityRef: note.NoteID, Label: note.Title})
		}
	}

	suggestions, err := store.Suggestions(projectID, "")
	if err != nil {
		return nil, err
	}
	for _, suggestion := range suggestions {
		if suggestion.DecidedAt < sinceUnix {
			continue
		}
		kind := ""
		switch suggestion.Status {
		case SuggestionAccepted:
			kind = ActivitySuggestionAccepted
		case SuggestionDismissed:
			kind = ActivitySuggestionDismissed
		default:
			continue
		}
		event := ActivityEvent{Kind: kind, At: suggestion.DecidedAt, Actor: suggestion.DecidedBy, SuggestionID: suggestion.ID, Label: suggestion.Title}
		if suggestion.NoteID != "" {
			event.EntityType, event.EntityRef = EntityNote, suggestion.NoteID
		}
		events = append(events, event)
	}

	goalEvents, err := goalActivity(store, projectID, sinceUnix)
	if err != nil {
		return nil, err
	}
	events = append(events, goalEvents...)

	for _, link := range links {
		if link.EntityType != EntityRepo {
			continue
		}
		repoStoreID, err := strconv.ParseInt(link.EntityRef, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("repo link %d holds %q, not a repo-store id", link.ID, link.EntityRef)
		}
		repoEvents, err := owners.repoActivity(ctx, repoStoreID, repoNames[link.EntityRef], sinceUnix)
		if err != nil {
			return nil, err
		}
		events = append(events, repoEvents...)
	}
	sort.SliceStable(events, func(left, right int) bool { return events[left].At > events[right].At })
	return events, nil
}

// goalActivity is which of a project's goals were created, which changed
// state and what was linked to them since the given unix time. Every state
// counts, a dismissed proposal too.
func goalActivity(store *Store, projectID string, since int64) ([]ActivityEvent, error) {
	events := []ActivityEvent{}
	goals, err := store.Goals(GoalFilter{ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	statementByGoal := map[string]string{}
	for _, goal := range goals {
		statementByGoal[goal.ID] = goal.Statement
		if goal.CreatedAt >= since {
			events = append(events, ActivityEvent{Kind: ActivityGoalCreated, At: goal.CreatedAt, Actor: goal.CreatedBy, GoalID: goal.ID, Label: goal.Statement})
		}
	}
	changes, err := store.goalChangesOfProject(projectID)
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		if change.Field == "state" && change.ChangedAt >= since {
			events = append(events, ActivityEvent{Kind: ActivityGoalStateChanged, At: change.ChangedAt, Actor: change.ChangedBy, GoalID: change.GoalID,
				Label: statementByGoal[change.GoalID], Field: change.Field, OldValue: change.OldValue, NewValue: change.NewValue})
		}
	}
	links, err := store.GoalLinks(GoalLinkFilter{ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.CreatedAt < since {
			continue
		}
		event := ActivityEvent{Kind: ActivityGoalLinkAdded, At: link.CreatedAt, Actor: link.CreatedBy, GoalID: link.GoalID,
			EntityType: link.EntityType, EntityRef: link.EntityRef, Label: link.Label}
		if link.EntityType == EntitySession {
			event.SessionID = link.EntityRef
		}
		events = append(events, event)
	}
	return events, nil
}

// repoActivity is what landed on, was deployed from, and happened to the pull
// requests of one repo since the given unix time.
func (owners *Owners) repoActivity(ctx context.Context, repoStoreID int64, name string, since int64) ([]ActivityEvent, error) {
	events := []ActivityEvent{}
	graph, err := owners.repoGraph(ctx, repoStoreID)
	if err != nil {
		return nil, err
	}
	for _, commit := range graph.defaultBranchCommits(since, graphCommitsRead) {
		events = append(events, ActivityEvent{Kind: ActivityCommitLanded, At: commit.CommittedAt, Actor: commit.AuthorName, SessionID: commit.SessionID,
			RepoStoreID: repoStoreID, RepoName: name, CommitSHA: commit.SHA, Subject: commit.Subject})
	}

	deploys, err := owners.deployments(ctx, repoStoreID, deploymentsRead)
	if err != nil {
		return nil, err
	}
	for _, deploy := range deploys {
		if deploy.CreatedAt >= since {
			events = append(events, ActivityEvent{Kind: ActivityDeployed, At: deploy.CreatedAt, Actor: deploy.DeployedBy,
				RepoStoreID: repoStoreID, RepoName: name, CommitSHA: deploy.CommitSHA})
		}
	}

	pullRequests, err := owners.PullRequests(ctx, repoStoreID, "all")
	if err != nil {
		return nil, err
	}
	for _, pullRequest := range pullRequests {
		event := func(kind string, at int64) ActivityEvent {
			return ActivityEvent{Kind: kind, At: at, Actor: pullRequest.Author, RepoStoreID: repoStoreID, RepoName: name,
				CommitSHA: pullRequest.HeadSHA, PullRequestNumber: pullRequest.Number, PullRequestTitle: pullRequest.Title, URL: pullRequest.URL}
		}
		if pullRequest.CreatedAt >= since {
			events = append(events, event(ActivityPullRequestOpened, pullRequest.CreatedAt))
		}
		switch {
		case pullRequest.MergedAt >= since && pullRequest.MergedAt != 0:
			events = append(events, event(ActivityPullRequestMerged, pullRequest.MergedAt))
		case pullRequest.State == "closed" && pullRequest.ClosedAt >= since && pullRequest.ClosedAt != 0:
			events = append(events, event(ActivityPullRequestClosed, pullRequest.ClosedAt))
		}
	}
	return events, nil
}

func (handlers *Handlers) ProjectActivity(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	for name := range query {
		if name != "days" {
			writeError(writer, fmt.Errorf("%w: unknown parameter %q; this route takes days", ErrInvalid, name))
			return
		}
	}
	days := DefaultActivityDays
	if raw := query.Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > MaximumActivityDays {
			writeError(writer, fmt.Errorf("%w: days must be a whole number from 1 to %d", ErrInvalid, MaximumActivityDays))
			return
		}
		days = parsed
	}
	id := request.PathValue("id")
	if _, err := handlers.Store.Project(id); err != nil {
		writeError(writer, err)
		return
	}
	since := handlers.Owners.now().Add(-time.Duration(days) * 24 * time.Hour)
	events, err := BuildActivity(request.Context(), handlers.Store, handlers.Owners, id, since)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"since": since.Unix(), "events": events})
}
