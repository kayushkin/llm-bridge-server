package server

// The repositories, file_storage and references modules, moved in from the
// standalone repo-store, work-graph-store, file-store and reference-store:
// the same routes under /repo-store/, /work-graph-store/, /file-store/ and
// /reference-store/, answered from this process.
//
// Each store's own mux answers its routes at the paths it always had; the
// patterns below are written out, because the route classification test
// reads them from string literals, and each strips its prefix and hands the
// request to that mux. Who reaches each route is decided by
// routeAccessRules, as for every route.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/workgraphstore"
)

// Where each store is mounted.
const (
	repoStoreMountPrefix      = "/repo-store"
	workGraphStoreMountPrefix = "/work-graph-store"
	fileStoreMountPrefix      = "/file-store"
	// referenceStoreMountPrefix is where reference-store is mounted. Its
	// lookups run as the caller, so the session agent token is accepted on
	// it, as on the store proxies.
	referenceStoreMountPrefix = "/reference-store"
)

// mountedStore answers one store's routes, or why it is off.
type mountedStore struct {
	handler   http.Handler
	offCode   string
	offReason string
}

// mountedStoreRoute strips prefix and hands the request to the store's mux.
// A store that is not open answers 503 with its reason.
func mountedStoreRoute(prefix string, mounted func() *mountedStore, offCode, offReason string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store := mounted()
		if store == nil {
			writeJSONError(w, http.StatusServiceUnavailable, offCode, offReason)
			return
		}
		http.StripPrefix(prefix, store.handler).ServeHTTP(w, r)
	}
}

// repositoryStores is repo-store's and work-graph-store's routes, and the
// registry a deploy action reads.
type repositoryStores struct {
	repoStore      http.Handler
	workGraphStore http.Handler
}

// repoRegistry is repo-store as a deploy action reads it; the repo-store
// module in this process is the one in production.
type repoRegistry interface {
	GetRepo(id int64) (*repostore.Repo, error)
}

// EnableRepositories serves repo-store and work-graph-store from their
// modules. syncer is nil when this server cannot run gh, and then
// POST /repo-store/repos/{id}/pull-requests/sync answers 503 with
// syncerOffReason. main calls it once, before the server listens.
func (s *Server) EnableRepositories(repos *repostore.Store, syncer *repostore.PullRequestSyncer, syncerOffReason string, workGraph *workgraphstore.Store) {
	repoStoreMux := http.NewServeMux()
	repostore.RegisterHandlers(repoStoreMux, repos)
	if syncer != nil {
		repostore.RegisterPullRequestHandlers(repoStoreMux, repos, syncer)
	} else {
		repostore.RegisterPullRequestHandlersWithoutSync(repoStoreMux, repos, syncerOffReason)
	}
	workGraphStoreMux := http.NewServeMux()
	workgraphstore.RegisterHandlers(workGraphStoreMux, &workgraphstore.Handlers{Store: workGraph, RepoStore: workgraphstore.RepoStoreModule{Store: repos}})
	s.repoRegistryForDeploys = repos
	s.repositories.Store(&repositoryStores{repoStore: repoStoreMux, workGraphStore: workGraphStoreMux})
	s.referenceReadable.repos.Store(repos)
	s.referenceReadable.workGraph.Store(workGraph)
}

// EnableFileStore serves file-store from its module, and makes the session
// files and images this server keeps go to it in process. The module reads
// the two file_store settings at each request; a change to either through
// PUT /settings is judged by file-store's own validators, and the values in
// force are judged now, since a value seeded from the environment never
// passed through PUT. main calls it once, before the server listens.
func (s *Server) EnableFileStore(files *filestore.Store) error {
	s.settings.SetValidator(config.SettingFileStoreMaximumFileBytes, filestore.ValidateMaximumFileBytes)
	s.settings.SetValidator(config.SettingFileStoreInlineContentTypes, filestore.ValidateInlineContentTypes)
	if err := s.checkFileStoreSettings(); err != nil {
		return err
	}
	module := filestore.NewServer(files, s.fileStoreLimits)
	handler := module.Handler()
	s.fileStore = module
	s.fileStoreCalls.Store(module)
	s.fileStoreModule.Store(files)
	s.fileStoreHandler.Store(&mountedStore{handler: handler})
	return nil
}

// fileStoreLimits is the size limit and inline list in force, read from the
// stored settings at the time of each request.
func (s *Server) fileStoreLimits() filestore.Limits {
	return filestore.Limits{
		MaximumFileBytes:   int64(s.settings.Integer(config.SettingFileStoreMaximumFileBytes)),
		InlineContentTypes: s.settings.StringList(config.SettingFileStoreInlineContentTypes),
	}
}

// checkFileStoreSettings refuses the two file_store settings in force when
// the API would refuse them.
func (s *Server) checkFileStoreSettings() error {
	limits := s.fileStoreLimits()
	if err := filestore.ValidateMaximumFileBytes(fmt.Sprint(limits.MaximumFileBytes)); err != nil {
		return fmt.Errorf("%s: %w", config.SettingFileStoreMaximumFileBytes, err)
	}
	if err := filestore.ValidateInlineContentTypes(strings.Join(limits.InlineContentTypes, ",")); err != nil {
		return fmt.Errorf("%s: %w", config.SettingFileStoreInlineContentTypes, err)
	}
	return nil
}

// EnableReferenceStore serves reference-store from resolver, and makes the
// chat links' references and grant-store's reference checks call it in
// process. main calls it once, after EnableAccess.
func (s *Server) EnableReferenceStore(resolver *referencestore.Resolver) error {
	if err := referencestore.ValidateInProcessReaders(resolver.Types, resolver.InProcess); err != nil {
		return err
	}
	s.referenceResolver.Store(resolver)
	mux := http.NewServeMux()
	referencestore.RegisterHandlers(mux, &referencestore.Server{Resolver: resolver})
	s.referenceStoreHandler.Store(&mountedStore{handler: mux})
	return nil
}

// ReferenceAccessDirectory is what reference-store's access check reads,
// from principal-store and grant-store in this process.
func ReferenceAccessDirectory(principals *principalstore.Store, grants *grantstore.Store, directory grantstore.PrincipalDirectory) referencestore.AccessDirectory {
	return referenceAccessDirectory{principals: principals, grants: grants, directory: directory}
}

type referenceAccessDirectory struct {
	principals *principalstore.Store
	grants     *grantstore.Store
	directory  grantstore.PrincipalDirectory
}

func (d referenceAccessDirectory) Standing(ctx context.Context, principalID string) (referencestore.PrincipalStanding, error) {
	principal, err := d.principals.Get(ctx, principalID, false)
	if errors.Is(err, principalstore.ErrNotFound) {
		return referencestore.PrincipalStanding{}, fmt.Errorf("no principal %s", principalID)
	}
	if err != nil {
		return referencestore.PrincipalStanding{}, err
	}
	return referencestore.PrincipalStanding{IsAdministrator: principal.IsAdministrator, Disabled: principal.DisabledAt != 0}, nil
}

func (d referenceAccessDirectory) Grants(ctx context.Context, principalID string) ([]referencestore.HeldGrant, error) {
	grants, err := d.grants.Effective(ctx, d.directory, principalID, referencestore.RelationCanSeeReferences, "")
	if err != nil {
		return nil, err
	}
	held := make([]referencestore.HeldGrant, 0, len(grants))
	for _, grant := range grants {
		held = append(held, referencestore.HeldGrant{Relation: grant.Relation, ResourceType: grant.ResourceType,
			ResourceID: grant.ResourceID, Revoked: grant.RevokedAt != 0})
	}
	return held, nil
}

// referenceStoreRoute answers one reference-store route as the caller: the
// verified principal, narrowed by the store's access rules, or the internal
// service acting as itself, which looks records up for nobody, unchecked, as
// reference-store's own service token did. A request with neither reaches the
// store with no caller, and the store answers 401.
func (s *Server) referenceStoreRoute(w http.ResponseWriter, r *http.Request) {
	store := s.referenceStoreHandler.Load()
	if store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "reference_store_off", "reference-store is not open on this server")
		return
	}
	ctx := r.Context()
	if principalID, identified := principalIdentityOfRequest(r); identified {
		ctx = referencestore.ContextWithCaller(ctx, referencestore.Caller{PrincipalID: principalID})
	} else if s.requestIsTheInternalServiceActingAsItself(r) {
		ctx = referencestore.ContextWithCaller(ctx, referencestore.Caller{Unchecked: true})
	}
	http.StripPrefix(referenceStoreMountPrefix, store.handler).ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) repoStoreRoute() http.HandlerFunc {
	return mountedStoreRoute(repoStoreMountPrefix, func() *mountedStore {
		stores := s.repositories.Load()
		if stores == nil {
			return nil
		}
		return &mountedStore{handler: stores.repoStore}
	}, "repo_store_off", config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no repos")
}

func (s *Server) workGraphStoreRoute() http.HandlerFunc {
	return mountedStoreRoute(workGraphStoreMountPrefix, func() *mountedStore {
		stores := s.repositories.Load()
		if stores == nil {
			return nil
		}
		return &mountedStore{handler: stores.workGraphStore}
	}, "work_graph_store_off", config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no ref updates")
}

func (s *Server) fileStoreRoute() http.HandlerFunc {
	return mountedStoreRoute(fileStoreMountPrefix, s.fileStoreHandler.Load, "file_store_off",
		config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no files")
}

// registerRepositoriesAndReferencesRoutes mounts the four stores.
func (s *Server) registerRepositoriesAndReferencesRoutes() {
	repoStore := s.repoStoreRoute()
	s.mux.HandleFunc("GET /repo-store/health", repoStore)
	s.mux.HandleFunc("GET /repo-store/repos", repoStore)
	s.mux.HandleFunc("POST /repo-store/repos", repoStore)
	s.mux.HandleFunc("GET /repo-store/repos/{id}", repoStore)
	s.mux.HandleFunc("DELETE /repo-store/repos/{id}", repoStore)
	s.mux.HandleFunc("POST /repo-store/repos/{id}/detect", repoStore)
	s.mux.HandleFunc("PUT /repo-store/repos/{id}/provenance", repoStore)
	s.mux.HandleFunc("PUT /repo-store/repos/{id}/identity", repoStore)
	s.mux.HandleFunc("GET /repo-store/repos/by-name/{name}", repoStore)
	s.mux.HandleFunc("GET /repo-store/repos/by-mention/{mention}", repoStore)
	s.mux.HandleFunc("GET /repo-store/by-path", repoStore)
	s.mux.HandleFunc("POST /repo-store/detect", repoStore)
	s.mux.HandleFunc("POST /repo-store/repos/{id}/deployments", repoStore)
	s.mux.HandleFunc("GET /repo-store/deployments", repoStore)
	s.mux.HandleFunc("GET /repo-store/deployments/latest", repoStore)
	s.mux.HandleFunc("GET /repo-store/pull-requests", repoStore)
	s.mux.HandleFunc("POST /repo-store/repos/{id}/pull-requests/sync", repoStore)

	workGraphStore := s.workGraphStoreRoute()
	s.mux.HandleFunc("GET /work-graph-store/health", workGraphStore)
	s.mux.HandleFunc("GET /work-graph-store/ref-updates", workGraphStore)
	s.mux.HandleFunc("GET /work-graph-store/repos/{repo_store_id}/graph", workGraphStore)
	s.mux.HandleFunc("GET /work-graph-store/activity", workGraphStore)
	s.mux.HandleFunc("GET /work-graph-store/session-repos", workGraphStore)
	s.mux.HandleFunc("GET /work-graph-store/commits/{mention...}", workGraphStore)

	fileStore := s.fileStoreRoute()
	s.mux.HandleFunc("GET /file-store/health", fileStore)
	s.mux.HandleFunc("GET /file-store/limits", fileStore)
	s.mux.HandleFunc("POST /file-store/files", fileStore)
	s.mux.HandleFunc("GET /file-store/files", fileStore)
	s.mux.HandleFunc("GET /file-store/files/{id}", fileStore)
	s.mux.HandleFunc("GET /file-store/files/{id}/content", fileStore)
	s.mux.HandleFunc("DELETE /file-store/files/{id}", fileStore)
	s.mux.HandleFunc("POST /file-store/files/{id}/restore", fileStore)

	s.mux.HandleFunc("GET /reference-store/health", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /reference-store/types", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /reference-store/types/{type}", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /reference-store/vocabulary", s.referenceStoreRoute)
	s.mux.HandleFunc("POST /reference-store/resolve", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /reference-store/search", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /reference-store/attachments/{id}", s.referenceStoreRoute)
}

// RepositoriesHandler serves repo-store's routes under /repo-store and
// work-graph-store's under /work-graph-store, past this server's route rules,
// for the modules in this process that read them (project-store). It answers
// 503 until EnableRepositories has run.
func (s *Server) RepositoriesHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(repoStoreMountPrefix+"/", s.repoStoreRoute())
	mux.Handle(workGraphStoreMountPrefix+"/", s.workGraphStoreRoute())
	return mux
}
