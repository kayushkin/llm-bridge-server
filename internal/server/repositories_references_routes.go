package server

// The repositories, file_storage and references modules, moved in from the
// standalone repo-store, work-graph-store, file-store and reference-store:
// the same routes under /repos/, /git-activity/, /file-storage/ and
// /references/, answered from this process.
//
// Each store's own mux answers its routes at the paths it always had; the
// patterns below are written out, because the route classification test
// reads them from string literals, and each strips its prefix and hands the
// request to that mux. Who reaches each route is decided by
// RouteAccessRules, as for every route.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/references"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/gitactivity"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repos"
)

// Where each store is mounted.
const (
	reposMountPrefix       = "/repos"
	gitActivityMountPrefix = "/git-activity"
	fileStorageMountPrefix = "/file-storage"
	// referencesMountPrefix is where reference-store is mounted. Its
	// lookups run as the caller, so the session agent token is accepted on
	// it, as on the store proxies.
	referencesMountPrefix = "/references"
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
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, offCode, offReason)
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
	// The stores themselves, for project-store's reads in process.
	repos          *repos.Store
	workGraph      *gitactivity.Store
	workGraphRepos gitactivity.RepoDirectory
}

// repoRegistry is repo-store as a deploy action reads it; the repo-store
// module in this process is the one in production.
type repoRegistry interface {
	GetRepo(id int64) (*repos.Repo, error)
}

// EnableRepositories serves repo-store and work-graph-store from their
// modules. syncer is nil when this server cannot run gh, and then
// POST /repos/repos/{id}/pull-requests/sync answers 503 with
// syncerOffReason. main calls it once, before the server listens.
func (s *Server) EnableRepositories(repoStore *repos.Store, syncer *repos.PullRequestSyncer, syncerOffReason string, workGraph *gitactivity.Store) {
	repoStoreMux := http.NewServeMux()
	repos.RegisterHandlers(repoStoreMux, repoStore)
	if syncer != nil {
		repos.RegisterPullRequestHandlers(repoStoreMux, repoStore, syncer)
	} else {
		repos.RegisterPullRequestHandlersWithoutSync(repoStoreMux, repoStore, syncerOffReason)
	}
	workGraphStoreMux := http.NewServeMux()
	workGraphRepos := gitactivity.RepoStoreModule{Store: repoStore}
	gitactivity.RegisterHandlers(workGraphStoreMux, &gitactivity.Handlers{Store: workGraph, RepoStore: workGraphRepos})
	s.repoRegistryForDeploys = repoStore
	s.repositories.Store(&repositoryStores{repoStore: repoStoreMux, workGraphStore: workGraphStoreMux, repos: repoStore, workGraph: workGraph, workGraphRepos: workGraphRepos})
	s.referenceReadable.repos.Store(repoStore)
	s.referenceReadable.workGraph.Store(workGraph)
}

// EnableFiles serves file-store from its module, and makes the session
// files and images this server keeps go to it in process. The module reads
// the two file_store settings at each request; a change to either through
// PUT /settings is judged by file-store's own validators, and the values in
// force are judged now, since a value seeded from the environment never
// passed through PUT. main calls it once, before the server listens.
func (s *Server) EnableFiles(fileStore *files.Store) error {
	s.settings.SetValidator(config.SettingFilesMaximumFileBytes, files.ValidateMaximumFileBytes)
	s.settings.SetValidator(config.SettingFilesInlineContentTypes, files.ValidateInlineContentTypes)
	if err := s.checkFileStoreSettings(); err != nil {
		return err
	}
	module := files.NewServer(fileStore, s.fileStoreLimits)
	handler := module.Handler()
	s.fileStore = module
	s.fileStoreCalls.Store(module)
	s.fileStoreModule.Store(fileStore)
	s.fileStoreHandler.Store(&mountedStore{handler: handler})
	return nil
}

// fileStoreLimits is the size limit and inline list in force, read from the
// stored settings at the time of each request.
func (s *Server) fileStoreLimits() files.Limits {
	return files.Limits{
		MaximumFileBytes:   int64(s.settings.Integer(config.SettingFilesMaximumFileBytes)),
		InlineContentTypes: s.settings.StringList(config.SettingFilesInlineContentTypes),
	}
}

// checkFileStoreSettings refuses the two file_store settings in force when
// the API would refuse them.
func (s *Server) checkFileStoreSettings() error {
	limits := s.fileStoreLimits()
	if err := files.ValidateMaximumFileBytes(fmt.Sprint(limits.MaximumFileBytes)); err != nil {
		return fmt.Errorf("%s: %w", config.SettingFilesMaximumFileBytes, err)
	}
	if err := files.ValidateInlineContentTypes(strings.Join(limits.InlineContentTypes, ",")); err != nil {
		return fmt.Errorf("%s: %w", config.SettingFilesInlineContentTypes, err)
	}
	return nil
}

// EnableReferences serves reference-store from resolver, and makes the
// chat links' references and grant-store's reference checks call it in
// process. main calls it once, after EnableAccess.
func (s *Server) EnableReferences(resolver *references.Resolver) error {
	if err := references.ValidateInProcessReaders(resolver.Types, resolver.InProcess); err != nil {
		return err
	}
	s.referenceResolver.Store(resolver)
	mux := http.NewServeMux()
	references.RegisterHandlers(mux, &references.Server{Resolver: resolver})
	s.referenceStoreHandler.Store(&mountedStore{handler: mux})
	return nil
}

// ReferenceAccessDirectory is what reference-store's access check reads,
// from principal-store and grant-store in this process.
func ReferenceAccessDirectory(principals *principals.Store, grants *grants.Store, directory grants.PrincipalDirectory) references.AccessDirectory {
	return referenceAccessDirectory{principals: principals, grants: grants, directory: directory}
}

type referenceAccessDirectory struct {
	principals *principals.Store
	grants     *grants.Store
	directory  grants.PrincipalDirectory
}

func (d referenceAccessDirectory) Standing(ctx context.Context, principalID string) (references.PrincipalStanding, error) {
	principal, err := d.principals.Get(ctx, principalID, false)
	if errors.Is(err, principals.ErrNotFound) {
		return references.PrincipalStanding{}, fmt.Errorf("no principal %s", principalID)
	}
	if err != nil {
		return references.PrincipalStanding{}, err
	}
	return references.PrincipalStanding{IsAdministrator: principal.IsAdministrator, Disabled: principal.DisabledAt != 0}, nil
}

func (d referenceAccessDirectory) Grants(ctx context.Context, principalID string) ([]references.HeldGrant, error) {
	grants, err := d.grants.Effective(ctx, d.directory, principalID, references.RelationCanSeeReferences, "")
	if err != nil {
		return nil, err
	}
	held := make([]references.HeldGrant, 0, len(grants))
	for _, grant := range grants {
		held = append(held, references.HeldGrant{Relation: grant.Relation, ResourceType: grant.ResourceType,
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
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "reference_store_off", "reference-store is not open on this server")
		return
	}
	ctx := r.Context()
	if principalID, identified := requestcaller.PrincipalIdentityOfRequest(r); identified {
		ctx = references.ContextWithCaller(ctx, references.Caller{PrincipalID: principalID})
	} else if s.requestIsTheInternalServiceActingAsItself(r) {
		ctx = references.ContextWithCaller(ctx, references.Caller{Unchecked: true})
	}
	http.StripPrefix(referencesMountPrefix, store.handler).ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) repoStoreRoute() http.HandlerFunc {
	return mountedStoreRoute(reposMountPrefix, func() *mountedStore {
		stores := s.repositories.Load()
		if stores == nil {
			return nil
		}
		return &mountedStore{handler: stores.repoStore}
	}, "repo_store_off", config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no repos")
}

func (s *Server) workGraphStoreRoute() http.HandlerFunc {
	return mountedStoreRoute(gitActivityMountPrefix, func() *mountedStore {
		stores := s.repositories.Load()
		if stores == nil {
			return nil
		}
		return &mountedStore{handler: stores.workGraphStore}
	}, "work_graph_store_off", config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no ref updates")
}

func (s *Server) fileStoreRoute() http.HandlerFunc {
	return mountedStoreRoute(fileStorageMountPrefix, s.fileStoreHandler.Load, "file_store_off",
		config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no files")
}

// registerRepositoriesAndReferencesRoutes mounts the four stores.
func (s *Server) registerRepositoriesAndReferencesRoutes() {
	repoStore := s.repoStoreRoute()
	s.mux.HandleFunc("GET /repos/health", repoStore)
	s.mux.HandleFunc("GET /repos/repos", repoStore)
	s.mux.HandleFunc("POST /repos/repos", repoStore)
	s.mux.HandleFunc("GET /repos/repos/{id}", repoStore)
	s.mux.HandleFunc("DELETE /repos/repos/{id}", repoStore)
	s.mux.HandleFunc("POST /repos/repos/{id}/detect", repoStore)
	s.mux.HandleFunc("PUT /repos/repos/{id}/provenance", repoStore)
	s.mux.HandleFunc("PUT /repos/repos/{id}/identity", repoStore)
	s.mux.HandleFunc("GET /repos/repos/by-name/{name}", repoStore)
	s.mux.HandleFunc("GET /repos/repos/by-mention/{mention}", repoStore)
	s.mux.HandleFunc("GET /repos/by-path", repoStore)
	s.mux.HandleFunc("POST /repos/detect", repoStore)
	s.mux.HandleFunc("POST /repos/repos/{id}/deployments", repoStore)
	s.mux.HandleFunc("GET /repos/deployments", repoStore)
	s.mux.HandleFunc("GET /repos/deployments/latest", repoStore)
	s.mux.HandleFunc("GET /repos/pull-requests", repoStore)
	s.mux.HandleFunc("POST /repos/repos/{id}/pull-requests/sync", repoStore)

	workGraphStore := s.workGraphStoreRoute()
	s.mux.HandleFunc("GET /git-activity/health", workGraphStore)
	s.mux.HandleFunc("GET /git-activity/ref-updates", workGraphStore)
	s.mux.HandleFunc("GET /git-activity/repos/{repo_store_id}/graph", workGraphStore)
	s.mux.HandleFunc("GET /git-activity/activity", workGraphStore)
	s.mux.HandleFunc("GET /git-activity/session-repos", workGraphStore)
	s.mux.HandleFunc("GET /git-activity/commits/{mention...}", workGraphStore)

	fileStore := s.fileStoreRoute()
	s.mux.HandleFunc("GET /file-storage/health", fileStore)
	s.mux.HandleFunc("GET /file-storage/limits", fileStore)
	s.mux.HandleFunc("POST /file-storage/files", fileStore)
	s.mux.HandleFunc("GET /file-storage/files", fileStore)
	s.mux.HandleFunc("GET /file-storage/files/{id}", fileStore)
	s.mux.HandleFunc("GET /file-storage/files/{id}/content", fileStore)
	s.mux.HandleFunc("DELETE /file-storage/files/{id}", fileStore)
	s.mux.HandleFunc("POST /file-storage/files/{id}/restore", fileStore)

	s.mux.HandleFunc("GET /references/health", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /references/types", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /references/types/{type}", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /references/vocabulary", s.referenceStoreRoute)
	s.mux.HandleFunc("POST /references/resolve", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /references/search", s.referenceStoreRoute)
	s.mux.HandleFunc("GET /references/attachments/{id}", s.referenceStoreRoute)
}
