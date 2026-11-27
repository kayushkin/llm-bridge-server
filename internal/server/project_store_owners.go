package server

// project-store's owners in this process: repo-store and work-graph-store,
// kanban-store, the scheduler and this server itself, each asked through its
// Go methods, the same code its routes run. project-store asks as the
// internal service, as it did over HTTP with the service token, so nothing
// here narrows an answer to a principal.

import (
	"context"
	"errors"
	"fmt"
	"time"

	schedulerapi "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/api"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/gitactivity"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repos"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	issuesapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
	"github.com/kayushkin/llm-bridge/msg"
)

// ProjectStoreRepositories is repo-store and work-graph-store as
// project-store reads them. They answer once EnableRepositories has run.
func (s *Server) ProjectStoreRepositories() projects.Repositories {
	return projectStoreRepositories{server: s}
}

// ProjectStoreKanban is kanban-store as project-store reads it. It answers
// once EnableIssues has run.
func (s *Server) ProjectStoreKanban() projects.Kanban {
	return projectStoreKanban{server: s}
}

// ProjectStoreScheduler is the scheduler's jobs as project-store reads them.
// It answers once EnableScheduler has run.
func (s *Server) ProjectStoreScheduler() projects.SchedulerJobs {
	return projectStoreScheduler{server: s}
}

// ProjectStoreBridge is this server as project-store reads it: its sessions,
// their briefs and its one-shot router.
func (s *Server) ProjectStoreBridge() projects.Bridge {
	return projectStoreBridge{server: s}
}

type projectStoreRepositories struct {
	server *Server
}

func (repositories projectStoreRepositories) stores() (*repositoryStores, error) {
	stores := repositories.server.repositories.Load()
	if stores == nil {
		return nil, errors.New("repo-store and work-graph-store are not open on this server")
	}
	return stores, nil
}

// repoStoreAnswer is a repo-store error as project-store reads it: a repo it
// does not have is projects.ErrNotFound.
func repoStoreAnswer(err error) error {
	if errors.Is(err, repos.ErrNotFound) || errors.Is(err, gitactivity.ErrNotFound) {
		return projects.ErrNotFound
	}
	return err
}

func (repositories projectStoreRepositories) RepoName(_ context.Context, repoStoreID int64) (string, error) {
	stores, err := repositories.stores()
	if err != nil {
		return "", err
	}
	repo, err := stores.repos.GetRepo(repoStoreID)
	if err != nil {
		return "", repoStoreAnswer(err)
	}
	return repo.Name, nil
}

func (repositories projectStoreRepositories) Deployments(_ context.Context, repoStoreID int64, limit int) ([]projects.Deploy, error) {
	stores, err := repositories.stores()
	if err != nil {
		return nil, err
	}
	stored, err := stores.repos.ListDeployments(repoStoreID, limit)
	if err != nil {
		return nil, repoStoreAnswer(err)
	}
	deploys := make([]projects.Deploy, len(stored))
	for i, deploy := range stored {
		deploys[i] = projects.Deploy{CommitSHA: deploy.CommitSHA, DeployedBy: deploy.DeployedBy, CreatedAt: deploy.CreatedAt}
	}
	return deploys, nil
}

func (repositories projectStoreRepositories) PullRequests(_ context.Context, repoStoreID int64, state string) ([]projects.PullRequest, error) {
	stores, err := repositories.stores()
	if err != nil {
		return nil, err
	}
	stored, err := stores.repos.ListPullRequests(repos.PullRequestListFilter{RepoID: repoStoreID, State: state, Limit: repos.DefaultPullRequestListLimit})
	if err != nil {
		return nil, repoStoreAnswer(err)
	}
	pullRequests := make([]projects.PullRequest, len(stored))
	for i, pullRequest := range stored {
		pullRequests[i] = projects.PullRequest{RepoID: pullRequest.RepoID, Number: pullRequest.Number, Title: pullRequest.Title, State: string(pullRequest.State),
			IsDraft: pullRequest.IsDraft, Author: pullRequest.Author, HeadRef: pullRequest.HeadRef, BaseRef: pullRequest.BaseRef, HeadSHA: pullRequest.HeadSHA,
			URL: pullRequest.URL, CreatedAt: pullRequest.CreatedAt, UpdatedAt: pullRequest.UpdatedAt, MergedAt: pullRequest.MergedAt, ClosedAt: pullRequest.ClosedAt,
			SyncedAt: pullRequest.SyncedAt}
	}
	return pullRequests, nil
}

func (repositories projectStoreRepositories) Graph(ctx context.Context, repoStoreID int64, maxCommits int) (projects.RepoGraph, error) {
	stores, err := repositories.stores()
	if err != nil {
		return projects.RepoGraph{}, err
	}
	repo, err := stores.workGraphRepos.Repo(ctx, repoStoreID)
	if err != nil {
		return projects.RepoGraph{}, repoStoreAnswer(err)
	}
	graph, err := gitactivity.BuildGraph(ctx, stores.workGraph, repo, maxCommits)
	if err != nil {
		return projects.RepoGraph{}, err
	}
	answer := projects.RepoGraph{DefaultBranch: graph.DefaultBranch}
	for _, commit := range graph.Commits {
		converted := projects.GraphCommit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt, AuthorName: commit.AuthorName,
			Subject: commit.Subject, OnOriginBranch: commit.OnOriginBranch}
		if commit.MadeBy != nil {
			converted.MadeBySessionID = commit.MadeBy.SessionID
		}
		answer.Commits = append(answer.Commits, converted)
	}
	for _, branch := range graph.Branches {
		converted := projects.GraphBranch{Name: branch.Name, IsRemote: branch.IsRemote, HeadSHA: branch.HeadSHA, CommittedAt: branch.CommittedAt,
			MergedIntoDefault: branch.MergedIntoDefault, WorktreePath: branch.WorktreePath}
		for _, session := range branch.Sessions {
			converted.SessionIDs = append(converted.SessionIDs, session.SessionID)
		}
		answer.Branches = append(answer.Branches, converted)
	}
	return answer, nil
}

func (repositories projectStoreRepositories) SessionRepos(_ context.Context, sinceUnixNano int64) ([]projects.SessionRepo, error) {
	stores, err := repositories.stores()
	if err != nil {
		return nil, err
	}
	rows, err := stores.workGraph.SessionRepos(sinceUnixNano)
	if err != nil {
		return nil, err
	}
	repos := make([]projects.SessionRepo, len(rows))
	for i, row := range rows {
		repos[i] = projects.SessionRepo{SessionID: row.SessionID, RepoStoreID: row.RepoStoreID, RefUpdates: int(row.RefUpdates)}
	}
	return repos, nil
}

type projectStoreKanban struct {
	server *Server
}

func (kanban projectStoreKanban) api() (*issuesapi.API, error) {
	handlers := kanban.server.kanbanStore.Load()
	if handlers == nil || handlers.api == nil {
		return nil, errKanbanStoreOff
	}
	return handlers.api, nil
}

// kanbanAnswer is a kanban-store refusal as project-store reads it: its 404
// is projects.ErrNotFound.
func kanbanAnswer(err error) error {
	var refusal *issuesapi.Refusal
	if errors.As(err, &refusal) && refusal.Status == 404 {
		return projects.ErrNotFound
	}
	return err
}

func (kanban projectStoreKanban) BoardName(_ context.Context, boardID string) (string, error) {
	api, err := kanban.api()
	if err != nil {
		return "", err
	}
	board, err := api.Board(issuesapi.Caller{InternalService: true}, boardID)
	if err != nil {
		return "", kanbanAnswer(err)
	}
	return board.Name, nil
}

func (kanban projectStoreKanban) ProjectCards(_ context.Context, projectID string) ([]projects.LinkedKanbanCard, error) {
	api, err := kanban.api()
	if err != nil {
		return nil, err
	}
	rows, err := api.EntityCards(issuesapi.Caller{InternalService: true}, "project", projectID)
	if err != nil {
		return nil, kanbanAnswer(err)
	}
	cards := make([]projects.LinkedKanbanCard, 0, len(rows))
	for _, row := range rows {
		card := projects.LinkedKanbanCard{CardID: row.CardID}
		if row.WorkState != nil {
			state := string(*row.WorkState)
			card.WorkState = &state
		}
		if item, isItem := row.Item.(map[string]any); isItem && item != nil {
			card.HasItem = true
			if card.Title, err = textOfItem(item, "title"); err != nil {
				return nil, fmt.Errorf("card %s: %w", row.CardID, err)
			}
			if card.Status, err = textOfItem(item, "status"); err != nil {
				return nil, fmt.Errorf("card %s: %w", row.CardID, err)
			}
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// textOfItem is a noteboard item's text field, "" when it is absent.
func textOfItem(item map[string]any, field string) (string, error) {
	value, present := item[field]
	if !present || value == nil {
		return "", nil
	}
	text, isText := value.(string)
	if !isText {
		return "", fmt.Errorf("noteboard item's %s is %T, not text", field, value)
	}
	return text, nil
}

type projectStoreScheduler struct {
	server *Server
}

func (scheduler projectStoreScheduler) JobName(_ context.Context, jobID int64) (string, error) {
	jobs := scheduler.server.schedulerJobsInProcess()
	if jobs == nil {
		return "", errors.New("the scheduler is not open on this server")
	}
	job, err := jobs.Job(jobID)
	if errors.Is(err, schedulerapi.ErrJobNotFound) {
		return "", projects.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return job.Name, nil
}

type projectStoreBridge struct {
	server *Server
}

// Session is GET /sessions/{id}: any failure to read the row is the route's
// 404.
func (bridge projectStoreBridge) Session(_ context.Context, sessionID string) (projects.BridgeSession, error) {
	session, err := bridge.server.store.GetSession(sessionID)
	if err != nil {
		return projects.BridgeSession{}, fmt.Errorf("%w: %v", projects.ErrNotFound, err)
	}
	return projects.BridgeSession{SessionID: session.SessionID, DisplayName: session.DisplayName, Type: string(session.Type), Purpose: session.Purpose,
		Origin: session.Origin, WorkingDirectory: session.WorkingDir, ManagerSessionID: session.ManagerSessionID,
		CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt}, nil
}

// ChatSessions is GET /sessions/summary?type=interactive&purpose=chat, as
// the internal service, whose list nothing narrows.
func (bridge projectStoreBridge) ChatSessions(_ context.Context, before string, limit int) ([]projects.BridgeChatSession, string, error) {
	rows, err := bridge.server.store.ListSessionSummaries(limit, before, store.SessionSummaryFilter{
		Types: []string{string(msg.SessionTypeInteractive)}, Purposes: []string{msg.PurposeChat}})
	if err != nil {
		return nil, "", err
	}
	chats := make([]projects.BridgeChatSession, len(rows))
	for i, row := range rows {
		chats[i] = projects.BridgeChatSession{SessionID: row.SessionID, DisplayName: row.DisplayName, UpdatedAt: row.UpdatedAt, CreatedAt: row.CreatedAt}
	}
	next := ""
	// A full page implies there may be more, as the route answers it.
	if len(rows) == limit && limit > 0 {
		next = rows[len(rows)-1].Cursor
	}
	return chats, next, nil
}

// SessionBriefsUpdatedSince is GET /session-briefs?updated_since=…, as the
// internal service.
func (bridge projectStoreBridge) SessionBriefsUpdatedSince(_ context.Context, since time.Time) ([]msg.SessionBriefView, error) {
	return bridge.server.store.ListSessionBriefs(store.SessionBriefFilter{UpdatedSince: since})
}

// SessionBrief is GET /sessions/{id}/brief.
func (bridge projectStoreBridge) SessionBrief(_ context.Context, sessionID string) (msg.SessionBriefView, error) {
	if _, err := bridge.server.store.GetSession(sessionID); err != nil {
		return msg.SessionBriefView{}, fmt.Errorf("%w: %v", projects.ErrNotFound, err)
	}
	return bridge.server.store.GetSessionBriefView(sessionID)
}

// OneShot is POST /oneshot, with the same checks of the request.
func (bridge projectStoreBridge) OneShot(ctx context.Context, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	response, refusal := bridge.server.routedOneShot(ctx, request)
	if refusal != nil {
		return msg.OneShotResponse{}, refusal
	}
	return response, nil
}
