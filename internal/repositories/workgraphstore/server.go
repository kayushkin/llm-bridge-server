package workgraphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Handlers serves the HTTP routes CONTRACT.md lists.
type Handlers struct {
	Store     *Store
	RepoStore *RepoStoreClient
}

// RegisterHandlers mounts every route on mux.
func RegisterHandlers(mux *http.ServeMux, handlers *Handlers) {
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ref-updates", handlers.listRefUpdates)
	mux.HandleFunc("GET /repos/{repo_store_id}/graph", handlers.repoGraph)
	mux.HandleFunc("GET /activity", handlers.activity)
	mux.HandleFunc("GET /session-repos", handlers.sessionRepos)
	mux.HandleFunc("GET /commits/{mention...}", handlers.commitByMention)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

func statusOf(err error) int {
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, errBadRequest) {
		return http.StatusBadRequest
	}
	if errors.Is(err, ErrAmbiguous) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

var errBadRequest = errors.New("bad request")

func positiveIntegerParameter(request *http.Request, name string, defaultValue, maximum int64) (int64, error) {
	raw := request.URL.Query().Get(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || value > maximum {
		return 0, fmt.Errorf("%w: %s must be a whole number from 1 to %d", errBadRequest, name, maximum)
	}
	return value, nil
}

func (handlers *Handlers) listRefUpdates(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	for name := range query {
		switch name {
		case "repo_store_id", "session_id", "ref", "since_unix_nano", "limit":
		default:
			writeError(writer, http.StatusBadRequest, fmt.Errorf("unknown parameter %q", name))
			return
		}
	}
	filter := RefUpdateFilter{SessionID: query.Get("session_id"), Ref: query.Get("ref")}
	var err error
	if filter.RepoStoreID, err = positiveIntegerParameter(request, "repo_store_id", 0, 1<<62); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if filter.SinceUnixNano, err = positiveIntegerParameter(request, "since_unix_nano", 0, 1<<62); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	limit, err := positiveIntegerParameter(request, "limit", 200, 5000)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	filter.Limit = int(limit)
	updates, err := handlers.Store.ListRefUpdates(filter)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ref_updates": updates})
}

func (handlers *Handlers) repoGraph(writer http.ResponseWriter, request *http.Request) {
	repoStoreID, err := strconv.ParseInt(request.PathValue("repo_store_id"), 10, 64)
	if err != nil || repoStoreID <= 0 {
		writeError(writer, http.StatusBadRequest, errors.New("repo_store_id must be repo-store's numeric id"))
		return
	}
	maxCommits, err := positiveIntegerParameter(request, "max_commits", 300, 5000)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	repo, err := handlers.RepoStore.Repo(request.Context(), repoStoreID)
	if err != nil {
		writeError(writer, statusOf(err), err)
		return
	}
	graph, err := BuildGraph(request.Context(), handlers.Store, repo, int(maxCommits))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, graph)
}

func (handlers *Handlers) commitByMention(writer http.ResponseWriter, request *http.Request) {
	answer, err := ResolveCommitMention(request.Context(), handlers.Store, handlers.RepoStore, request.PathValue("mention"))
	if err != nil {
		writeError(writer, statusOf(err), err)
		return
	}
	writeJSON(writer, http.StatusOK, answer)
}

// RepoActivity is one repo's local branches that sessions touched recently.
type RepoActivity struct {
	Repo          Repo     `json:"repo"`
	DefaultBranch string   `json:"default_branch"`
	Branches      []Branch `json:"branches"`
	// DeletedBranches are refs sessions touched in the window that no longer
	// exist, usually because they were merged and cleaned up.
	DeletedBranches []string `json:"deleted_branches"`
}

func (handlers *Handlers) activity(writer http.ResponseWriter, request *http.Request) {
	hours, err := positiveIntegerParameter(request, "hours", 72, 24*90)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	format := request.URL.Query().Get("format")
	if format != "" && format != "text" && format != "json" {
		writeError(writer, http.StatusBadRequest, errors.New("format is json (the default) or text"))
		return
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	activity, err := BuildActivity(request.Context(), handlers.Store, handlers.RepoStore, since)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	if format == "text" {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(writer, ActivityText(activity, hours))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"since_unix_nano": since.UnixNano(), "repos": activity})
}

// BuildActivity lists, for every repo a session touched since then, the local
// branches sessions moved or checked out in that time, with where each stands.
func BuildActivity(ctx context.Context, store *Store, repoStore *RepoStoreClient, since time.Time) ([]RepoActivity, error) {
	updates, err := store.ListRefUpdates(RefUpdateFilter{SinceUnixNano: since.UnixNano()})
	if err != nil {
		return nil, err
	}
	touched := map[int64]map[string]bool{}
	for _, update := range updates {
		if update.RepoStoreID == 0 || update.SessionID == "" || !strings.HasPrefix(update.Ref, "refs/heads/") {
			continue
		}
		if touched[update.RepoStoreID] == nil {
			touched[update.RepoStoreID] = map[string]bool{}
		}
		touched[update.RepoStoreID][update.Ref] = true
	}

	activity := []RepoActivity{}
	for repoStoreID, refs := range touched {
		repo, err := repoStore.Repo(ctx, repoStoreID)
		if err != nil {
			return nil, err
		}
		defaultBranch := DefaultBranchOf(ctx, repo.Path)
		branches, err := readBranches(ctx, store, repo, defaultBranch)
		if err != nil {
			return nil, err
		}
		entry := RepoActivity{Repo: repo, DefaultBranch: defaultBranch, Branches: []Branch{}, DeletedBranches: []string{}}
		existing := map[string]bool{}
		for _, branch := range branches {
			existing[branch.Ref] = true
			if refs[branch.Ref] {
				entry.Branches = append(entry.Branches, branch)
			}
		}
		for ref := range refs {
			if !existing[ref] {
				entry.DeletedBranches = append(entry.DeletedBranches, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		sort.Strings(entry.DeletedBranches)
		sort.Slice(entry.Branches, func(left, right int) bool { return entry.Branches[left].Name < entry.Branches[right].Name })
		activity = append(activity, entry)
	}
	sort.Slice(activity, func(left, right int) bool { return activity[left].Repo.Name < activity[right].Repo.Name })
	return activity, nil
}

// ActivityText renders activity for an agent to read: one repo per block, one
// branch per line, unmerged branches marked.
func ActivityText(activity []RepoActivity, hours int64) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Branches agent sessions touched in the last %d hours (work-graph-store).\n", hours)
	if len(activity) == 0 {
		text.WriteString("None.\n")
		return text.String()
	}
	for _, repo := range activity {
		fmt.Fprintf(&text, "\n%s (repo-store id %d, default branch %q)\n", repo.Repo.Name, repo.Repo.ID, repo.DefaultBranch)
		for _, branch := range repo.Branches {
			state := "merged"
			if !branch.MergedIntoDefault {
				state = "NOT MERGED"
			}
			if branch.Name == repo.DefaultBranch {
				state = "default"
			}
			fmt.Fprintf(&text, "  %s  %s  %s", branch.Name, shortSHA(branch.HeadSHA), state)
			if branch.WorktreePath != "" {
				fmt.Fprintf(&text, "  worktree %s", branch.WorktreePath)
			}
			var sessions []string
			for _, session := range branch.Sessions {
				sessions = append(sessions, fmt.Sprintf("%s (%s)", session.SessionID, strings.Join(session.GitSubcommands, ",")))
			}
			fmt.Fprintf(&text, "\n    sessions: %s\n", strings.Join(sessions, "; "))
		}
		if len(repo.DeletedBranches) > 0 {
			fmt.Fprintf(&text, "  deleted since: %s\n", strings.Join(repo.DeletedBranches, ", "))
		}
	}
	return text.String()
}

func shortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// sessionRepos answers, for every session, how many refs it moved in each
// repo since since_unix_nano (required, so a caller always says how far back).
func (handlers *Handlers) sessionRepos(writer http.ResponseWriter, request *http.Request) {
	for name := range request.URL.Query() {
		if name != "since_unix_nano" {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("unknown parameter %q", name))
			return
		}
	}
	if request.URL.Query().Get("since_unix_nano") == "" {
		writeError(writer, http.StatusBadRequest, errors.New("since_unix_nano is required"))
		return
	}
	since, err := positiveIntegerParameter(request, "since_unix_nano", 0, 1<<62)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	repos, err := handlers.Store.SessionRepos(since)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session_repos": repos})
}
