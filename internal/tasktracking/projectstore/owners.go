package projectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	HTTP                    *http.Client
	RepoStoreURL            string
	KanbanStoreURL          string
	KanbanStoreServiceToken string
	SchedulerURL            string
	HealthcheckURL          string
	PrincipalStoreURL       string
	// Noteboard is noteboard itself, in this process: what a note link names
	// and where a next step is written.
	Noteboard          Noteboard
	WorkGraphStoreURL  string
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

// KanbanServiceTokenHeader is how kanban-store recognizes an internal service.
const KanbanServiceTokenHeader = "X-Kanban-Store-Service-Token"

// getJSON fetches a URL into target. A 404 is ErrNotFound; anything else that
// is not 200 is ErrOwnerUnavailable.
func (owners *Owners) getJSON(ctx context.Context, rawURL string, headers map[string]string, target any) error {
	if err := ownerAddressIsSet(rawURL); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := owners.HTTP.Do(request)
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

func (owners *Owners) kanbanHeaders() map[string]string {
	return map[string]string{KanbanServiceTokenHeader: owners.KanbanStoreServiceToken}
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
		err = owners.getJSON(ctx, owners.RepoStoreURL+"/repos/"+escaped, nil, &repo)
		label = repo.Name
	case EntityBoard:
		var board struct {
			Name string `json:"name"`
		}
		err = owners.getJSON(ctx, owners.KanbanStoreURL+"/api/boards/"+escaped, owners.kanbanHeaders(), &board)
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
		var principal struct {
			DisplayName string `json:"display_name"`
		}
		err = owners.getJSON(ctx, owners.PrincipalStoreURL+"/principals/"+escaped, nil, &principal)
		label = principal.DisplayName
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
