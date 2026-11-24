package repostore

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// DefaultPullRequestListLimit is how many pull requests GET /pull-requests and
// POST /repos/{id}/pull-requests/sync answer with when no limit is given.
const DefaultPullRequestListLimit = 100

// RegisterPullRequestHandlers wires the pull-request routes onto mux. They are
// apart from RegisterHandlers because the sync route needs a syncer, and the
// rest of the API reaches nothing outside the database.
func RegisterPullRequestHandlers(mux *http.ServeMux, store *Store, syncer *PullRequestSyncer) {
	handler := &pullRequestHandler{store: store, syncer: syncer}
	mux.HandleFunc("GET /pull-requests", handler.listPullRequests) // ?repo_id=&state=&limit=
	mux.HandleFunc("POST /repos/{id}/pull-requests/sync", handler.syncPullRequestsOfRepo)
}

type pullRequestHandler struct {
	store  *Store
	syncer *PullRequestSyncer
}

type pullRequestList struct {
	PullRequests []PullRequest `json:"pull_requests"`
}

// refuseUnknownQueryParameters answers 400 naming any query parameter not in
// accepted, so a misspelled filter is loud rather than silently off. It
// reports whether it answered.
func refuseUnknownQueryParameters(w http.ResponseWriter, r *http.Request, accepted ...string) bool {
	var unknown []string
	for name := range r.URL.Query() {
		known := false
		for _, acceptedName := range accepted {
			if name == acceptedName {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	if len(unknown) == 0 {
		return false
	}
	sort.Strings(unknown)
	acceptedText := "none"
	if len(accepted) > 0 {
		acceptedText = strings.Join(accepted, ", ")
	}
	writeErr(w, 400, "unknown query parameter "+strings.Join(unknown, ", ")+"; accepted: "+acceptedText)
	return true
}

// listPullRequests answers GET /pull-requests. Every parameter is optional,
// but one that is given must hold a value: an empty ?state= or ?repo_id= is a
// 400 rather than a quiet default.
func (handler *pullRequestHandler) listPullRequests(w http.ResponseWriter, r *http.Request) {
	if refuseUnknownQueryParameters(w, r, "repo_id", "state", "limit") {
		return
	}
	query := r.URL.Query()
	filter := PullRequestListFilter{State: string(PullRequestStateOpen), Limit: DefaultPullRequestListLimit}
	if query.Has("repo_id") {
		repoID, err := parseID(query.Get("repo_id"))
		if err != nil {
			writeErr(w, 400, "repo_id must be a repo-store repo id, a positive integer")
			return
		}
		filter.RepoID = repoID
	}
	if query.Has("state") {
		state := query.Get("state")
		if _, known := pullRequestStatesAQueryAccepts[state]; !known {
			writeErr(w, 400, "unknown state "+strconv.Quote(state)+"; accepted: "+strings.Join(pullRequestStatesAQueryAcceptsSorted(), ", "))
			return
		}
		filter.State = state
	}
	if query.Has("limit") {
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil || limit <= 0 {
			writeErr(w, 400, "limit must be a positive integer")
			return
		}
		filter.Limit = limit
	}
	pullRequests, err := handler.store.ListPullRequests(filter)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, pullRequestList{PullRequests: pullRequests})
}

// syncPullRequestsOfRepo reads the repo's pull requests from GitHub now and
// answers as GET /pull-requests?repo_id=<id>&state=all would.
func (handler *pullRequestHandler) syncPullRequestsOfRepo(w http.ResponseWriter, r *http.Request) {
	if refuseUnknownQueryParameters(w, r) {
		return
	}
	repoID, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	repo, err := handler.store.GetRepo(repoID)
	if err != nil {
		respondLookup(w, err)
		return
	}
	_, err = handler.syncer.SyncRepo(r.Context(), repo)
	switch {
	case errors.Is(err, ErrRepoHasNoGitHubURL):
		writeErr(w, 400, err.Error())
		return
	case errors.Is(err, ErrGitHubRequestFailed):
		writeErr(w, 502, err.Error())
		return
	case err != nil:
		writeErr(w, 500, err.Error())
		return
	}
	pullRequests, err := handler.store.ListPullRequests(PullRequestListFilter{RepoID: repoID, State: PullRequestStateQueryAll, Limit: DefaultPullRequestListLimit})
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, pullRequestList{PullRequests: pullRequests})
}
