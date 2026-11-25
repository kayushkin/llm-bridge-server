package projectstore

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ErrOwnerUnavailable means the store that owns an entity could not answer, so
// the HTTP layer answers 502 and writes nothing.
var ErrOwnerUnavailable = errors.New("owner unavailable")

// Owners asks each entity's own store about it. Every id a project links to is
// checked with its owner before it is written, and the owner's name for it is
// kept beside it for display. Every owner but healthcheck runs in the same
// process as this store and is asked through its Go interface.
type Owners struct {
	// HTTP carries the one call to a service of its own, healthcheck.
	HTTP *http.Client
	// Repositories is repo-store and work-graph-store.
	Repositories Repositories
	// Kanban is kanban-store, asked as the internal service.
	Kanban Kanban
	// Scheduler is the scheduler's jobs.
	Scheduler      SchedulerJobs
	HealthcheckURL string
	// Principals is principal-store, in this process: what a principal link
	// names.
	Principals Principals
	// Noteboard is noteboard itself, in this process: what a note link names
	// and where a next step is written.
	Noteboard Noteboard
	// Bridge is llm-bridge-server, the process this store runs in: its
	// sessions, their briefs and its one-shot router, asked as its internal
	// service.
	Bridge Bridge
	// SuggestionModelRole is the model-store role asked for suggested next
	// steps, through llm-bridge-server's one-shot router.
	SuggestionModelRole string
	// Clock is nil outside tests.
	Clock func() time.Time
}

// Repositories is repo-store and work-graph-store as this store reads them.
// A repo either does not have is ErrNotFound; any other failure is the
// store's.
type Repositories interface {
	// RepoName is repo-store's name for the repo now.
	RepoName(ctx context.Context, repoStoreID int64) (string, error)
	// Deployments is repo-store's ledger of the repo's deploys, newest first,
	// at most limit of them.
	Deployments(ctx context.Context, repoStoreID int64, limit int) ([]Deploy, error)
	// PullRequests is the repo's pull requests in one state (open, closed,
	// merged or all).
	PullRequests(ctx context.Context, repoStoreID int64, state string) ([]PullRequest, error)
	// Graph is work-graph-store's graph of the repo, its newest maxCommits
	// commits.
	Graph(ctx context.Context, repoStoreID int64, maxCommits int) (RepoGraph, error)
	// SessionRepos is, for each session, how many refs it moved in each repo
	// since the time given in Unix nanoseconds.
	SessionRepos(ctx context.Context, sinceUnixNano int64) ([]SessionRepo, error)
}

// SessionRepo is the refs one session moved in one repo.
type SessionRepo struct {
	SessionID   string
	RepoStoreID int64
	RefUpdates  int
}

// Kanban is kanban-store as this store reads it, as the internal service. A
// board it does not have is ErrNotFound.
type Kanban interface {
	// BoardName is the board's name.
	BoardName(ctx context.Context, boardID string) (string, error)
	// ProjectCards is every card linked to the project, oldest link first,
	// orphans (a link whose noteboard item is gone) included.
	ProjectCards(ctx context.Context, projectID string) ([]LinkedKanbanCard, error)
}

// LinkedKanbanCard is one card linked to a project, as kanban-store answers
// it. HasItem is false for an orphan.
type LinkedKanbanCard struct {
	CardID    string
	HasItem   bool
	Title     string
	Status    string
	WorkState *string
}

// SchedulerJobs is the scheduler's jobs as this store reads them. A job it
// does not have is ErrNotFound.
type SchedulerJobs interface {
	JobName(ctx context.Context, jobID int64) (string, error)
}

// Bridge is llm-bridge-server as this store reads it, as its internal
// service: the code its routes run, so a session, a brief and a model call
// are what GET /sessions/{id}, GET /session-briefs and POST /oneshot answer.
// A session it does not have is ErrNotFound.
type Bridge interface {
	// Session is one session's record.
	Session(ctx context.Context, sessionID string) (BridgeSession, error)
	// ChatSessions is one page of the chats (type interactive, purpose chat)
	// newest first, before the cursor ("" for the first page), and the cursor
	// of the next page ("" after the last).
	ChatSessions(ctx context.Context, before string, limit int) ([]BridgeChatSession, string, error)
	// SessionBriefsUpdatedSince is every brief updated since then.
	SessionBriefsUpdatedSince(ctx context.Context, since time.Time) ([]msg.SessionBriefView, error)
	// SessionBrief is one session's brief; a session no turn has briefed yet
	// has a nil Brief.
	SessionBrief(ctx context.Context, sessionID string) (msg.SessionBriefView, error)
	// OneShot is one routed model call for a model-store role.
	OneShot(ctx context.Context, request msg.OneShotRequest) (msg.OneShotResponse, error)
}

// BridgeSession is the part of a session's record this store reads.
type BridgeSession struct {
	SessionID        string
	DisplayName      string
	Type             string
	Purpose          string
	Origin           string
	WorkingDirectory string
	ManagerSessionID string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// BridgeChatSession is one row of the bridge's list of chats.
type BridgeChatSession struct {
	SessionID   string
	DisplayName string
	UpdatedAt   time.Time
	CreatedAt   time.Time
}

// modelCallTimeout bounds one model call, which takes far longer than a store
// lookup.
const modelCallTimeout = 5 * time.Minute

// askOwner is what an owner answered, as this store answers it: ErrNotFound
// as itself, and any other failure as ErrOwnerUnavailable, naming what was
// asked.
func askOwner(what string, err error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrOwnerUnavailable, what, err)
}

// repositories is Repositories, or the reason it cannot be asked.
func (owners *Owners) repositories(what string) (Repositories, error) {
	if owners.Repositories == nil {
		return nil, fmt.Errorf("%w: repo-store and work-graph-store are not open on this server, so %s cannot be read", ErrOwnerUnavailable, what)
	}
	return owners.Repositories, nil
}

// bridge is Bridge, or the reason it cannot be asked.
func (owners *Owners) bridge(what string) (Bridge, error) {
	if owners.Bridge == nil {
		return nil, fmt.Errorf("%w: this store runs in no llm-bridge-server to answer %s", ErrOwnerUnavailable, what)
	}
	return owners.Bridge, nil
}

// getJSON fetches a URL into target. A 404 is ErrNotFound; anything else that
// is not 200 is ErrOwnerUnavailable.
func (owners *Owners) getJSON(ctx context.Context, rawURL string, headers map[string]string, target any) error {
	return fetchJSON(ctx, owners.HTTP, rawURL, headers, target)
}

// fetchJSON is one GET through client into target.
func fetchJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, target any) error {
	if err := ownerAddressIsSet(rawURL); err != nil {
		return err
	}
	return getJSONWith(ctx, client, rawURL, headers, target)
}

func getJSONWith(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: GET %s: %v", ErrOwnerUnavailable, rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 300))
		return fmt.Errorf("%w: GET %s: %s: %s", ErrOwnerUnavailable, rawURL, response.Status, body)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("%w: GET %s: %v", ErrOwnerUnavailable, rawURL, err)
	}
	return nil
}

// ownerAddressIsSet refuses a call to a store whose address this server was
// not given: the URL then starts with a path, and the call fails here, naming
// what it was for, instead of being sent nowhere.
func ownerAddressIsSet(rawURL string) error {
	if strings.HasPrefix(rawURL, "/") {
		return fmt.Errorf("%w: the address of the store that answers %s is not set on this server", ErrOwnerUnavailable, rawURL)
	}
	return nil
}

// numericRef is an entity ref its owner keys by number. One that is not a
// number is refused as the owner's route refused it, a 400 that this store
// relays as the owner failing.
func numericRef(service, entityRef string) (int64, error) {
	id, err := strconv.ParseInt(entityRef, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: %s refuses %q: its ids are positive numbers", ErrOwnerUnavailable, service, entityRef)
	}
	return id, nil
}

// Label checks that the entity exists in its store and returns the store's
// name for it. An entity its store does not have is ErrInvalid.
func (owners *Owners) Label(ctx context.Context, entityType, entityRef string) (string, error) {
	var err error
	var label string
	switch entityType {
	case EntityRepo:
		var repoStoreID int64
		if repoStoreID, err = numericRef("repo-store", entityRef); err == nil {
			var repositories Repositories
			if repositories, err = owners.repositories("repo " + entityRef); err == nil {
				label, err = repositories.RepoName(ctx, repoStoreID)
				err = askOwner("repo-store, repo "+entityRef, err)
			}
		}
	case EntityBoard:
		if owners.Kanban == nil {
			err = fmt.Errorf("%w: this server has no kanban-store to read board %s from", ErrOwnerUnavailable, entityRef)
		} else {
			label, err = owners.Kanban.BoardName(ctx, entityRef)
			err = askOwner("kanban-store, board "+entityRef, err)
		}
	case EntitySchedulerJob:
		var jobID int64
		if jobID, err = numericRef("the scheduler", entityRef); err == nil {
			if owners.Scheduler == nil {
				err = fmt.Errorf("%w: this server has no scheduler to read job %s from", ErrOwnerUnavailable, entityRef)
			} else {
				label, err = owners.Scheduler.JobName(ctx, jobID)
				err = askOwner("the scheduler, job "+entityRef, err)
			}
		}
	case EntityService:
		var status struct {
			Services []struct {
				Name string `json:"name"`
				Unit string `json:"unit"`
			} `json:"services"`
		}
		if err = owners.getJSON(ctx, owners.HealthcheckURL+"/api/status", nil, &status); err == nil {
			err = ErrNotFound
			for _, service := range status.Services {
				if service.Name == entityRef {
					err, label = nil, service.Name
					if service.Unit != "" {
						label = service.Unit
					}
				}
			}
		}
	case EntityPrincipal:
		label, err = owners.Principals.DisplayName(ctx, entityRef)
	case EntityNote:
		label, err = owners.Noteboard.ItemTitle(ctx, entityRef)
	case EntitySession:
		var bridge Bridge
		if bridge, err = owners.bridge("session " + entityRef); err == nil {
			var session BridgeSession
			session, err = bridge.Session(ctx, entityRef)
			err = askOwner("llm-bridge-server, session "+entityRef, err)
			label = session.DisplayName
		}
	default:
		return "", fmt.Errorf("%w: entity_type %q is not one a project can own", ErrInvalid, entityType)
	}
	if errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("%w: %s has no %s %q", ErrInvalid, serviceOf(entityType), entityType, entityRef)
	}
	return label, err
}

func serviceOf(entityType string) string {
	for _, info := range EntityTypes {
		if info.Type == entityType {
			return info.Service
		}
	}
	return "no store"
}
