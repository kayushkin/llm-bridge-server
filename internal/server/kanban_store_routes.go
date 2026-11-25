package server

// kanban-store, run in this process (internal/tasktracking/kanbanstore). Its
// routes stay where the identity-carrying proxy put them: /kanban/<rest>
// answers kanban-store's /api/<rest>, and /kanban-store/<rest> its /<rest>.
//
// The standalone store trusted X-Principal-Id, which only the proxy was meant
// to set. In this process the store reads its caller from the request
// context, set here from the credential request authorization verified, so no
// header a client sends can name the caller. The internal service — this
// server's own service token, naming nobody — is unrestricted, as
// kanban-store's own service token was; that token retires with the store.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
	kanbanapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/api"
	kanbanbundles "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/bundlestore"
	kanbandb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/db"
	kanbanfiles "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/filestore"
	kanbangrants "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/grantstore"
	kanbanbridge "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/llmbridge"
	kanbanmultichat "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/multichat"
	kanbannoteboard "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/noteboard"
	kanbanprincipals "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/principalstore"
	kanbanreferences "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/referencestore"
	noteboardapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
)

// kanbanStoreHandlers is the store as this server serves it.
type kanbanStoreHandlers struct {
	// handler answers every route at the store's own paths (/api/…, /health).
	handler http.Handler
	// store is the module, for grant-store's check that a board exists.
	store *kanbandb.Store
	// api is the store's own, for mailstack's question of which messages a
	// person sees through a ticket.
	api *kanbanapi.API
	// messageTriggersEnabled is whether triggers send, for the start log.
	messageTriggersEnabled bool
}

// KanbanStoreOwners is what kanban-store asks the stores beside it, in this
// process: noteboard for a card's content, and the access stores for who a
// principal is and what it was granted.
type KanbanStoreOwners struct {
	Noteboard *items.Store
	Access    *access.Stores
	// MessageTriggersEnabled is kanban_store.message_triggers.enabled: send
	// through multichat, or record each delivery not_configured.
	MessageTriggersEnabled bool
}

// EnableKanbanStore serves kanban-store from store, joined to the owners it
// asks. main calls it once, after EnableAccess and before the server listens;
// until then, and in a test that never calls it, /kanban/ and /kanban-store/
// answer 503 and this server's own kanban reads are off.
func (s *Server) EnableKanbanStore(store *kanbandb.Store, owners KanbanStoreOwners) {
	notes := &kanbannoteboard.Client{
		BaseURL: "http://noteboard.in-process",
		HTTP:    &http.Client{Transport: inprocesshttp.Transport{Handler: noteboardapi.New(owners.Noteboard).Handler()}},
	}
	directory := access.GrantsPrincipalDirectory(owners.Access.Principals)
	api := kanbanapi.New(store, notes,
		kanbanPrincipalDirectory{principals: owners.Access.Principals},
		kanbanAgentsAndInstances{server: s},
		kanbanBundles{server: s})
	// reference-store and file-store run in this process too; kanban-store's
	// clients of them reach their handlers through inprocesshttp, with no
	// socket and no token. Each answers 503 until its module is enabled.
	references := kanbanreferences.New(inprocesshttp.BaseURL)
	references.HTTP = inprocesshttp.Client(s.ReferenceStoreModuleHandler())
	api.SetReferenceStore(references)
	files := kanbanfiles.New(inprocesshttp.BaseURL, "")
	files.HTTP = inprocesshttp.Client(s.FileStoreModuleHandler())
	api.SetFileStore(files)
	if owners.MessageTriggersEnabled {
		api.SetMessageSender(kanbanmultichat.New(s.cfg.MultichatURL, kanbanmultichat.AuthStoreTokenSource(s.cfg.AuthStoreURL, s.cfg.AuthStoreToken)))
	}
	api.SetPrincipalEnforcement(kanbanapi.PrincipalEnforcement{Grants: kanbanBoardGrants{
		grants:    owners.Access.Grants,
		directory: directory,
		checker:   s.grantResourceChecker(owners.Access.Principals),
	}})
	handler := api.Handler()
	s.kanbanClient = kanbanclient.New(api)
	s.kanbanStore.Store(&kanbanStoreHandlers{handler: handler, store: store, api: api, messageTriggersEnabled: owners.MessageTriggersEnabled})
}

// KanbanStoreDescription says, for the start log, what kanban-store was
// joined to.
func (s *Server) KanbanStoreDescription() string {
	handlers := s.kanbanStore.Load()
	if handlers == nil {
		return errKanbanStoreOff.Error()
	}
	triggers := "message triggers are recorded not_configured and nothing is sent (" + config.SettingKanbanStoreMessageTriggersEnabled + " is off)"
	if handlers.messageTriggersEnabled {
		triggers = "message triggers send through multichat at " + s.cfg.MultichatURL
	}
	return fmt.Sprintf("attachments in the file-store module and reference types from reference-store, both in this process; %s", triggers)
}

// CheckKanbanStoreConfiguration refuses what the store would refuse at start:
// message triggers switched on with no multichat to send through, or no
// auth-store token to read multichat's from.
func CheckKanbanStoreConfiguration(cfg *config.Config, messageTriggersEnabled bool) error {
	if messageTriggersEnabled {
		if cfg.MultichatURL == "" {
			return fmt.Errorf("%s is on but multichat.url is not set: message triggers send through multichat", config.SettingKanbanStoreMessageTriggersEnabled)
		}
		if cfg.AuthStoreToken == "" {
			return fmt.Errorf("%s is on but auth_store.token is not set: message triggers read multichat's API token from auth-store provider \"multichat\"", config.SettingKanbanStoreMessageTriggersEnabled)
		}
	}
	return nil
}

// errKanbanStoreOff is a use of the store on a server that never enabled it.
var errKanbanStoreOff = errors.New("kanban-store is off: EnableKanbanStore was never called on this server")

// serveKanbanStore answers <mountPrefix>/<rest> with the store's
// <storePathPrefix>/<rest>, as the verified caller.
func (s *Server) serveKanbanStore(w http.ResponseWriter, r *http.Request, mountPrefix, storePathPrefix string) {
	handlers := s.kanbanStore.Load()
	if handlers == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "kanban_store_off", errKanbanStoreOff.Error())
		return
	}
	var caller kanbanapi.Caller
	if principalID, identified := principalIdentityOfRequest(r); identified {
		caller = kanbanapi.Caller{PrincipalID: principalID}
	} else if s.requestIsTheInternalServiceActingAsItself(r) {
		caller = kanbanapi.Caller{InternalService: true}
	} else {
		// Request authorization lets nothing else through to a store route.
		writeJSONError(w, http.StatusUnauthorized, "kanban_store_requires_a_caller",
			mountPrefix+"/ answers a principal or this server's service token")
		return
	}
	storePath, err := url.Parse(storePathPrefix + escapedPathAfterPrefix(r.URL, mountPrefix))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_path", fmt.Sprintf("the path after %s/ is not one kanban-store can read: %v", mountPrefix, err))
		return
	}
	served := r.Clone(kanbanapi.ContextWithCaller(r.Context(), caller))
	served.URL.Path = storePath.Path
	served.URL.RawPath = storePath.RawPath
	served.RequestURI = served.URL.RequestURI()
	// The store reads none of these, and none is its business: the caller is
	// in the context.
	for key := range served.Header {
		for _, removed := range requestHeadersNeverForwardedToStores {
			if strings.EqualFold(key, removed) {
				delete(served.Header, key)
			}
		}
	}
	removeCookieFromRequestHeader(served.Header, loginSessionCookieName)
	handlers.handler.ServeHTTP(w, served)
}

// handleKanbanStoreAsCaller answers /kanban/<rest> with kanban-store's
// /api/<rest>.
func (s *Server) handleKanbanStoreAsCaller(w http.ResponseWriter, r *http.Request) {
	s.serveKanbanStore(w, r, kanbanProxyMountPrefix, "/api")
}

// handleKanbanStoreRootAsCaller answers /kanban-store/<rest> with kanban-store's
// /<rest>, unchanged.
func (s *Server) handleKanbanStoreRootAsCaller(w http.ResponseWriter, r *http.Request) {
	s.serveKanbanStore(w, r, kanbanStoreProxyMountPrefix, "")
}

// checkKanbanBoardExists is grant-store's check that a board a grant names
// exists, made against the module.
func (s *Server) checkKanbanBoardExists(boardID string) error {
	handlers := s.kanbanStore.Load()
	if handlers == nil {
		return fmt.Errorf("%w, so board %q cannot be checked", errKanbanStoreOff, boardID)
	}
	if _, err := handlers.store.GetBoard(boardID); errors.Is(err, kanbandb.ErrNotFound) {
		return fmt.Errorf("%w: kanban-store has no board %q", grantstore.ErrResourceNotFound, boardID)
	} else if err != nil {
		return fmt.Errorf("kanban-store: reading board %s: %w", boardID, err)
	}
	return nil
}

// kanbanPrincipalDirectory answers kanban-store's questions about principals
// from principal-store in this process, as principal-store's GET
// /principals/{id} and GET /principals/{group}/members?available_at= did.
type kanbanPrincipalDirectory struct {
	principals *principalstore.Store
}

func kanbanPrincipalOf(principal *principalstore.Principal) kanbanprincipals.Principal {
	return kanbanprincipals.Principal{
		ID:              principal.ID,
		Kind:            principal.Kind,
		DisplayName:     principal.DisplayName,
		DisabledAt:      principal.DisabledAt,
		IsAdministrator: principal.IsAdministrator,
	}
}

func (directory kanbanPrincipalDirectory) Get(id string) (*kanbanprincipals.Principal, error) {
	principal, err := directory.principals.Get(context.Background(), id, false)
	if errors.Is(err, principalstore.ErrNotFound) {
		return nil, kanbanprincipals.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("principal-store: reading %s: %w", id, err)
	}
	read := kanbanPrincipalOf(principal)
	return &read, nil
}

func (directory kanbanPrincipalDirectory) ListAvailableMembers(groupID string, at time.Time) ([]kanbanprincipals.Principal, error) {
	members, err := directory.principals.ListAvailableMembers(context.Background(), groupID, at)
	if errors.Is(err, principalstore.ErrNotFound) {
		return nil, kanbanprincipals.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("principal-store: the members of %s available at %s: %w", groupID, at.UTC().Format(time.RFC3339), err)
	}
	read := make([]kanbanprincipals.Principal, 0, len(members))
	for _, member := range members {
		read = append(read, kanbanPrincipalOf(member))
	}
	return read, nil
}

// kanbanBoardGrants answers kanban-store's questions about board grants from
// grant-store in this process, as the internal service, the way kanban-store
// called the standalone grant-store with its service token.
type kanbanBoardGrants struct {
	grants    *grantstore.Store
	directory grantstore.PrincipalDirectory
	checker   grantstore.ResourceChecker
}

func (boardGrants kanbanBoardGrants) EffectiveBoardGrants(principalID string) ([]kanbangrants.BoardGrant, error) {
	grants, err := boardGrants.grants.Effective(context.Background(), boardGrants.directory, principalID, "", kanbangrants.ResourceTypeBoard)
	if errors.Is(err, grantstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: %v", kanbangrants.ErrPrincipalNotFound, err)
	}
	if err != nil {
		return nil, fmt.Errorf("grant-store: the effective board grants of %s: %w", principalID, err)
	}
	read := make([]kanbangrants.BoardGrant, 0, len(grants))
	for _, grant := range grants {
		read = append(read, kanbangrants.BoardGrant{PrincipalID: grant.PrincipalID, Relation: grant.Relation, BoardID: grant.ResourceID})
	}
	return read, nil
}

func (boardGrants kanbanBoardGrants) GrantBoardAdministration(principalID, boardID string) error {
	_, _, err := boardGrants.grants.Create(context.Background(), boardGrants.directory, boardGrants.checker, grantstore.GrantRequest{
		PrincipalID:  principalID,
		Relation:     kanbangrants.RelationCanAdminister,
		ResourceType: kanbangrants.ResourceTypeBoard,
		ResourceID:   boardID,
		Note:         "created the board",
	})
	if err != nil {
		return fmt.Errorf("grant-store: can_administer on board %s for %s: %w", boardID, principalID, err)
	}
	return nil
}

// kanbanAgentsAndInstances answers kanban-store's check that a board's default
// agent or instance exists, from this server's own agent-store and
// harness-store.
type kanbanAgentsAndInstances struct {
	server *Server
}

func (owner kanbanAgentsAndInstances) CheckInstanceExists(id string) error {
	if owner.server.harnessStore == nil {
		return fmt.Errorf("this server has no harness-store, so instance %q cannot be checked", id)
	}
	if _, err := owner.server.harnessStore.GetInstanceWithMachine(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: instance %s", kanbanbridge.ErrNotFound, id)
		}
		return fmt.Errorf("harness-store: reading instance %s: %w", id, err)
	}
	return nil
}

func (owner kanbanAgentsAndInstances) CheckAgentExists(id string) error {
	if owner.server.agentStore == nil {
		return fmt.Errorf("this server has no agent-store, so agent %q cannot be checked", id)
	}
	// A board names agent-store's numeric id; the slug is renameable.
	number, err := strconv.ParseInt(id, 10, 64)
	if err != nil || strconv.FormatInt(number, 10) != id {
		return fmt.Errorf("%w: agent %q is not agent-store's numeric id (send the id, not the slug)", kanbanbridge.ErrNotFound, id)
	}
	if _, err := owner.server.agentStore.GetAgent(number); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: agent-store has no agent %s", kanbanbridge.ErrNotFound, id)
		}
		return fmt.Errorf("agent-store: reading agent %s: %w", id, err)
	}
	return nil
}

// kanbanBundles answers kanban-store's check that a board's default bundle
// exists, from bundle-store in this process.
type kanbanBundles struct {
	server *Server
}

func (owner kanbanBundles) CheckBundleExists(id string) error {
	if owner.server.bundleClient == nil {
		return errors.New(bundleStoreOffMessage)
	}
	err := owner.server.bundleClient.CheckExists(context.Background(), id)
	if errors.Is(err, bundleclient.ErrNotFound) {
		return fmt.Errorf("%w: %v", kanbanbundles.ErrNotFound, err)
	}
	return err
}

// KanbanStoreHTTPClientAsInternalService is an HTTP client whose every
// request kanban-store's handler serves in this process as the internal
// service. Only rule-store's live rules use it: a rule's effect names a
// method, a path and a body, and its run records the answer's status, so it
// is a request by design. Its requests go to KanbanStoreInProcessBaseURL.
func (s *Server) KanbanStoreHTTPClientAsInternalService() (*http.Client, error) {
	handlers := s.kanbanStore.Load()
	if handlers == nil {
		return nil, errKanbanStoreOff
	}
	asInternalService := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.handler.ServeHTTP(w, r.WithContext(kanbanapi.ContextWithCaller(r.Context(), kanbanapi.Caller{InternalService: true})))
	})
	return &http.Client{Transport: inprocesshttp.Transport{Handler: asInternalService}}, nil
}

// KanbanStoreInProcessBaseURL is the address requests to
// KanbanStoreHTTPClientAsInternalService are written to. No request leaves the
// process, so it names nothing on the network.
const KanbanStoreInProcessBaseURL = "http://kanban-store.in-process"
