package projectstore

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrOwnerUnavailable means the store that owns an entity could not answer, so
// the HTTP layer answers 502 and writes nothing.
var ErrOwnerUnavailable = errors.New("owner unavailable")

// Owners asks each entity's own store about it. Every id a project links to is
// checked with its owner before it is written, and the owner's name for it is
// kept beside it for display.
type Owners struct {
	HTTP *http.Client
	// Repositories serves repo-store's routes under /repo-store and
	// work-graph-store's under /work-graph-store. Both run in this process,
	// so they are asked through their handlers (internal/inprocesshttp), not
	// over a socket.
	Repositories http.Handler
	// Kanban carries the calls to kanban-store at KanbanStoreURL. In this
	// server kanban-store runs in the same process, and Kanban serves each
	// call with its handler as the internal service, with no socket between.
	Kanban         *http.Client
	KanbanStoreURL string
	SchedulerURL   string
	HealthcheckURL string
	// Principals is principal-store, in this process: what a principal link
	// names.
	Principals Principals
	// Noteboard is noteboard itself, in this process: what a note link names
	// and where a next step is written.
	Noteboard          Noteboard
	BridgeURL          string
	BridgeServiceToken string
	// SuggestionModelRole is the model-store role asked for suggested next
	// steps, through llm-bridge-server's one-shot router.
	SuggestionModelRole string
	// ModelHTTP carries the one-shot model call, which takes far longer than
	// a store lookup.
	ModelHTTP *http.Client
	// Clock is nil outside tests.
	Clock func() time.Time
}

// BridgeServiceTokenHeader is how llm-bridge-server recognizes an internal service.
const BridgeServiceTokenHeader = "X-LLM-Bridge-Service-Token"

// repositoriesGetJSON fetches path from repo-store or work-graph-store in
// this process into target, as getJSON does over HTTP.
func (owners *Owners) repositoriesGetJSON(ctx context.Context, path string, target any) error {
	if owners.Repositories == nil {
		return fmt.Errorf("%w: repo-store and work-graph-store are not open on this server, so %s cannot be read", ErrOwnerUnavailable, path)
	}
	return getJSONWith(ctx, inprocesshttp.Client(owners.Repositories), inprocesshttp.BaseURL+path, nil, target)
}

// getJSON fetches a URL into target. A 404 is ErrNotFound; anything else that
// is not 200 is ErrOwnerUnavailable.
func (owners *Owners) getJSON(ctx context.Context, rawURL string, headers map[string]string, target any) error {
	return fetchJSON(ctx, owners.HTTP, rawURL, headers, target)
}

// getKanbanJSON is getJSON for a kanban-store path, through Kanban.
func (owners *Owners) getKanbanJSON(ctx context.Context, path string, target any) error {
	if owners.Kanban == nil {
		return fmt.Errorf("%w: this server has no kanban-store to answer %s", ErrOwnerUnavailable, path)
	}
	return fetchJSON(ctx, owners.Kanban, owners.KanbanStoreURL+path, nil, target)
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

// Label checks that the entity exists in its store and returns the store's
// name for it. An entity its store does not have is ErrInvalid.
func (owners *Owners) Label(ctx context.Context, entityType, entityRef string) (string, error) {
	escaped := url.PathEscape(entityRef)
	var err error
	var label string
	switch entityType {
	case EntityRepo:
		var repo struct {
			Name string `json:"name"`
		}
		err = owners.repositoriesGetJSON(ctx, "/repo-store/repos/"+escaped, &repo)
		label = repo.Name
	case EntityBoard:
		var board struct {
			Name string `json:"name"`
		}
		err = owners.getKanbanJSON(ctx, "/api/boards/"+escaped, &board)
		label = board.Name
	case EntitySchedulerJob:
		var job struct {
			Name string `json:"name"`
		}
		err = owners.getJSON(ctx, owners.SchedulerURL+"/api/jobs/"+escaped, nil, &job)
		label = job.Name
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
		var session struct {
			DisplayName string `json:"display_name"`
		}
		err = owners.getJSON(ctx, owners.BridgeURL+"/sessions/"+escaped, map[string]string{BridgeServiceTokenHeader: owners.BridgeServiceToken}, &session)
		label = session.DisplayName
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
