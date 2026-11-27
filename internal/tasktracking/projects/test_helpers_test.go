package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

// newTestStore is a store on a Postgres database of the test's own.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}

// httpNotesAndIssues is NotesAndIssues over a fake noteboard's HTTP routes, as the
// standalone store called noteboard before both moved into one process.
type httpNotesAndIssues struct {
	client *http.Client
	url    string
}

func (noteboard httpNotesAndIssues) call(ctx context.Context, method, path string, body any, wantStatus int, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, noteboard.url+path, reader)
	if err != nil {
		return err
	}
	response, err := noteboard.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOwnerUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && wantStatus == http.StatusOK && method == http.MethodGet {
		return ErrNotFound
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%w: %s %s: %s", ErrOwnerUnavailable, method, path, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (noteboard httpNotesAndIssues) ItemTitle(ctx context.Context, id string) (string, error) {
	var item LinkedItem
	err := noteboard.call(ctx, http.MethodGet, "/api/items/"+id, nil, http.StatusOK, &item)
	return item.Title, err
}

func (noteboard httpNotesAndIssues) LinkedItems(ctx context.Context, ids []string) ([]LinkedItem, []string, error) {
	var answer struct {
		Items      []LinkedItem `json:"items"`
		MissingIDs []string     `json:"missing_ids"`
	}
	err := noteboard.call(ctx, http.MethodPost, "/api/items/query", map[string]any{"ids": ids, "include_items": true, "limit": len(ids)}, http.StatusOK, &answer)
	return answer.Items, answer.MissingIDs, err
}

func (noteboard httpNotesAndIssues) CreateTodo(ctx context.Context, title, body string, tags []string, priority *int) (LinkedItem, error) {
	todo := map[string]any{"type": "todo", "title": title, "body": body, "tags": tags}
	if priority != nil {
		todo["priority"] = *priority
	}
	var item LinkedItem
	err := noteboard.call(ctx, http.MethodPost, "/api/items", todo, http.StatusCreated, &item)
	return item, err
}

// httpPrincipals answers Principals from a fake principal-store's
// GET /principals/{id}, so the tests' fake owners serve principals as before.
type httpPrincipals struct {
	client *http.Client
	url    string
}

func (principals httpPrincipals) DisplayName(ctx context.Context, id string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, principals.url+"/principals/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	response, err := principals.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrOwnerUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: principal-store answered %d", ErrOwnerUnavailable, response.StatusCode)
	}
	var principal struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&principal); err != nil {
		return "", err
	}
	return principal.DisplayName, nil
}

// httpOwners answers Repositories, Kanban, SchedulerJobs and Bridge from a
// fake that serves the routes those owners answered when each was a service
// of its own, at its root: repo-store's and work-graph-store's, kanban-store's
// /api/…, the scheduler's /api/jobs/… and llm-bridge-server's. The tests' fakes
// keep the shapes they were written in, and what the owners answer is decoded
// into the Go values the interfaces carry.
type httpOwners struct {
	client *http.Client
	url    string
}

func (owners httpOwners) get(ctx context.Context, path string, target any) error {
	return getJSONWith(ctx, owners.client, owners.url+path, nil, target)
}

func (owners httpOwners) RepoName(ctx context.Context, repoStoreID int64) (string, error) {
	var repo struct {
		Name string `json:"name"`
	}
	err := owners.get(ctx, "/repos/"+strconv.FormatInt(repoStoreID, 10), &repo)
	return repo.Name, err
}

func (owners httpOwners) Deployments(ctx context.Context, repoStoreID int64, limit int) ([]Deploy, error) {
	var deploys []Deploy
	err := owners.get(ctx, "/deployments?repo_id="+strconv.FormatInt(repoStoreID, 10)+"&limit="+strconv.Itoa(limit), &deploys)
	return deploys, err
}

func (owners httpOwners) PullRequests(ctx context.Context, repoStoreID int64, state string) ([]PullRequest, error) {
	var answer struct {
		PullRequests []PullRequest `json:"pull_requests"`
	}
	err := owners.get(ctx, "/pull-requests?repo_id="+strconv.FormatInt(repoStoreID, 10)+"&state="+url.QueryEscape(state), &answer)
	return answer.PullRequests, err
}

func (owners httpOwners) Graph(ctx context.Context, repoStoreID int64, maxCommits int) (RepoGraph, error) {
	var graph struct {
		DefaultBranch string `json:"default_branch"`
		Commits       []struct {
			SHA         string   `json:"sha"`
			Parents     []string `json:"parents"`
			CommittedAt int64    `json:"committed_at"`
			AuthorName  string   `json:"author_name"`
			Subject     string   `json:"subject"`
			MadeBy      *struct {
				SessionID string `json:"session_id"`
			} `json:"made_by"`
			OnOriginBranch bool `json:"on_origin_branch"`
		} `json:"commits"`
		Branches []struct {
			Name              string `json:"name"`
			IsRemote          bool   `json:"is_remote"`
			HeadSHA           string `json:"head_sha"`
			CommittedAt       int64  `json:"committed_at"`
			MergedIntoDefault bool   `json:"merged_into_default"`
			WorktreePath      string `json:"worktree_path"`
			Sessions          []struct {
				SessionID string `json:"session_id"`
			} `json:"sessions"`
		} `json:"branches"`
	}
	if err := owners.get(ctx, "/repos/"+strconv.FormatInt(repoStoreID, 10)+"/graph?max_commits="+strconv.Itoa(maxCommits), &graph); err != nil {
		return RepoGraph{}, err
	}
	answer := RepoGraph{DefaultBranch: graph.DefaultBranch}
	for _, commit := range graph.Commits {
		converted := GraphCommit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt, AuthorName: commit.AuthorName, Subject: commit.Subject, OnOriginBranch: commit.OnOriginBranch}
		if commit.MadeBy != nil {
			converted.MadeBySessionID = commit.MadeBy.SessionID
		}
		answer.Commits = append(answer.Commits, converted)
	}
	for _, branch := range graph.Branches {
		converted := GraphBranch{Name: branch.Name, IsRemote: branch.IsRemote, HeadSHA: branch.HeadSHA, CommittedAt: branch.CommittedAt,
			MergedIntoDefault: branch.MergedIntoDefault, WorktreePath: branch.WorktreePath}
		for _, session := range branch.Sessions {
			converted.SessionIDs = append(converted.SessionIDs, session.SessionID)
		}
		answer.Branches = append(answer.Branches, converted)
	}
	return answer, nil
}

func (owners httpOwners) SessionRepos(ctx context.Context, sinceUnixNano int64) ([]SessionRepo, error) {
	var answer struct {
		SessionRepos []struct {
			SessionID   string `json:"session_id"`
			RepoStoreID int64  `json:"repo_store_id"`
			RefUpdates  int    `json:"ref_updates"`
		} `json:"session_repos"`
	}
	if err := owners.get(ctx, "/session-repos?since_unix_nano="+strconv.FormatInt(sinceUnixNano, 10), &answer); err != nil {
		return nil, err
	}
	var repos []SessionRepo
	for _, row := range answer.SessionRepos {
		repos = append(repos, SessionRepo{SessionID: row.SessionID, RepoStoreID: row.RepoStoreID, RefUpdates: row.RefUpdates})
	}
	return repos, nil
}

func (owners httpOwners) BoardName(ctx context.Context, boardID string) (string, error) {
	var board struct {
		Name string `json:"name"`
	}
	err := owners.get(ctx, "/api/boards/"+url.PathEscape(boardID), &board)
	return board.Name, err
}

func (owners httpOwners) ProjectCards(ctx context.Context, projectID string) ([]LinkedKanbanCard, error) {
	var rows []struct {
		CardID string `json:"card_id"`
		Item   *struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"item"`
		WorkState *string `json:"work_state"`
	}
	if err := owners.get(ctx, "/api/entities/project/"+url.PathEscape(projectID)+"/cards", &rows); err != nil {
		return nil, err
	}
	cards := []LinkedKanbanCard{}
	for _, row := range rows {
		card := LinkedKanbanCard{CardID: row.CardID, HasItem: row.Item != nil, WorkState: row.WorkState}
		if row.Item != nil {
			card.Title, card.Status = row.Item.Title, row.Item.Status
		}
		cards = append(cards, card)
	}
	return cards, nil
}

func (owners httpOwners) JobName(ctx context.Context, jobID int64) (string, error) {
	var job struct {
		Name string `json:"name"`
	}
	err := owners.get(ctx, "/api/jobs/"+strconv.FormatInt(jobID, 10), &job)
	return job.Name, err
}

func (owners httpOwners) Session(ctx context.Context, sessionID string) (BridgeSession, error) {
	var record struct {
		SessionID        string    `json:"session_id"`
		DisplayName      string    `json:"display_name"`
		Type             string    `json:"type"`
		Purpose          string    `json:"purpose"`
		Origin           string    `json:"origin"`
		WorkingDirectory string    `json:"working_dir"`
		ManagerSessionID string    `json:"manager_session_id"`
		CreatedAt        time.Time `json:"created_at"`
		UpdatedAt        time.Time `json:"updated_at"`
	}
	err := owners.get(ctx, "/sessions/"+url.PathEscape(sessionID), &record)
	return BridgeSession(record), err
}

func (owners httpOwners) ChatSessions(ctx context.Context, before string, limit int) ([]BridgeChatSession, string, error) {
	query := url.Values{"type": {"interactive"}, "purpose": {"chat"}, "limit": {strconv.Itoa(limit)}}
	if before != "" {
		query.Set("before", before)
	}
	var page struct {
		Sessions []struct {
			SessionID   string    `json:"sessionId"`
			DisplayName string    `json:"displayName"`
			UpdatedAt   time.Time `json:"updatedAt"`
			CreatedAt   time.Time `json:"createdAt"`
		} `json:"sessions"`
		Next string `json:"next"`
	}
	if err := owners.get(ctx, "/sessions/summary?"+query.Encode(), &page); err != nil {
		return nil, "", err
	}
	var chats []BridgeChatSession
	for _, row := range page.Sessions {
		chats = append(chats, BridgeChatSession(row))
	}
	return chats, page.Next, nil
}

func (owners httpOwners) SessionBriefsUpdatedSince(ctx context.Context, since time.Time) ([]msg.SessionBriefView, error) {
	var views []msg.SessionBriefView
	err := owners.get(ctx, "/session-briefs?"+url.Values{"updated_since": {since.UTC().Format(time.RFC3339)}}.Encode(), &views)
	return views, err
}

func (owners httpOwners) SessionBrief(ctx context.Context, sessionID string) (msg.SessionBriefView, error) {
	var view msg.SessionBriefView
	err := owners.get(ctx, "/sessions/"+url.PathEscape(sessionID)+"/brief", &view)
	return view, err
}

func (owners httpOwners) OneShot(ctx context.Context, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, owners.url+"/oneshot", bytes.NewReader(encoded))
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	response, err := owners.client.Do(httpRequest)
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return msg.OneShotResponse{}, fmt.Errorf("one-shot answered %s: %s", response.Status, body)
	}
	var answer msg.OneShotResponse
	return answer, json.NewDecoder(response.Body).Decode(&answer)
}
