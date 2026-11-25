package server

// possession-store moved in from the standalone service: the same routes
// under /possession-store/, answered from this process. Like the other
// connected records stores, every record has an owner and the store narrows
// every read to the caller's own, or every owner's for an administrator and
// for this server's own service acting as itself, and every write and pass to
// one owner (internal/connectedrecords/possessionstore/server_scope.go). The
// caller reaches the store in the request context, set by
// connectedRecordsRoute from the credential request authorization verified.
//
// What the store asks on the way is in this process too, asked as the
// caller in the request context: photo-store and location-store through
// their handlers, mailstack and budget-store through their handlers as that
// principal, harness-store through its Go store, model calls through the
// one-shot router priced at model-store's list price, and originals' bytes in
// the file-store module. Only article-store is outside this process, called
// over HTTP at possession_store.article_store_url.
//
// The photo review and the background reading of covers' wearer boxes spend
// money on model calls, so they run only when
// possession_store.review_worker.enabled is on, for the owners
// possession_store.review_worker.owner_principal_ids names, each as that
// owner (StartPossessionReviewWorkers).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessionstore"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack"
)

// possessionStoreMountPrefix is where possession-store is mounted.
const possessionStoreMountPrefix = "/possession-store"

// Hosts of the in-process services possession-store calls. A request to any
// other host leaves the process.
const (
	possessionPhotoStoreHost    = "photo-store.in-process"
	possessionLocationStoreHost = "location-store.in-process"
	possessionMailstackHost     = "mailstack.in-process"
	possessionBudgetStoreHost   = "budget-store.in-process"
)

// possessionStores is possession-store as this server serves it.
type possessionStores struct {
	store    *possessionstore.Store
	reviewer *possessionstore.Reviewer
	handler  http.Handler
	patterns []string
}

// PossessionsModule is what EnablePossessions needs that this server does not
// already hold: the way to open the module's pools, and the store's settings.
type PossessionsModule struct {
	Open                   possessionstore.PoolOpener
	FirstReaderRole        string
	SecondReaderRole       string
	OwnerLocation          *time.Location
	ReviewImageLongestSide int
	CoverCacheDirectory    string
	ArticleStoreURL        string
	// ReviewOwners are the owners whose outfit covers the outfit list may
	// read wearer boxes for in the background: the review worker's owners
	// while it is on, and nobody while it is off.
	ReviewOwners []string
}

// EnablePossessions serves possession-store from its module. main calls it
// once, after EnableFileStore, EnablePhotosAndLocations, EnableMailstack and
// EnableBudgetStore, and before the server listens. Until then its routes answer 503.
func (s *Server) EnablePossessions(module PossessionsModule) error {
	files := s.fileStoreModule.Load()
	if files == nil {
		return errors.New("possession-store: EnableFileStore must run first, since possession-store keeps its originals in file-store")
	}
	if s.photosAndLocations.Load() == nil {
		return errors.New("possession-store: EnablePhotosAndLocations must run first, since possession-store reads photos and checks places there")
	}
	if s.budgetStore.Load() == nil {
		return errors.New("possession-store: EnableBudgetStore must run first, since possession-store records eBay orders and checks transactions there")
	}
	if s.harnessStore == nil {
		return errors.New("possession-store: this server has no harness-store, where a computer's machine is checked")
	}
	if module.Open == nil || module.OwnerLocation == nil || module.CoverCacheDirectory == "" {
		return errors.New("possession-store: the pool opener, the owner's time zone and the cover cache directory are required")
	}
	client := s.possessionOutboundClient()
	references := possessionstore.ServiceReferences{
		PhotoStoreURL:    "http://" + possessionPhotoStoreHost,
		LocationStoreURL: "http://" + possessionLocationStoreHost,
		ArticleStoreURL:  strings.TrimRight(module.ArticleStoreURL, "/"),
		BudgetStoreURL:   "http://" + possessionBudgetStoreHost,
		HTTPClient:       client,
		Machines:         possessionHarnessMachines{server: s},
	}
	store, err := possessionstore.New(module.Open, references, PossessionOriginalFiles(files, s.fileStoreLimits))
	if err != nil {
		return err
	}
	budget := possessionstore.BudgetStore{URL: "http://" + possessionBudgetStoreHost, HTTPClient: client}
	store.UseBudgetStore(budget)
	reviewer := &possessionstore.Reviewer{
		Store:  store,
		Models: possessionstore.OneShotModelCaller{RunRole: s.oneShotRouter.RunRole, ListPrice: s.possessionModelPrice},
		Photos: possessionstore.PhotoStoreSource{
			PhotoStoreURL: "http://" + possessionPhotoStoreHost,
			Originals:     store,
			LongestSide:   module.ReviewImageLongestSide,
			HTTPClient:    client,
		},
		FirstReaderRole:  module.FirstReaderRole,
		SecondReaderRole: module.SecondReaderRole,
		Articles:         possessionstore.ArticleStore{URL: strings.TrimRight(module.ArticleStoreURL, "/"), HTTPClient: &http.Client{Timeout: time.Minute}},
		Budget:           budget,
		Mail:             possessionstore.Mailstack{URL: "http://" + possessionMailstackHost, HTTPClient: client},
		OwnerLocation:    module.OwnerLocation,
		ReadCoverWearersInBackgroundFor: func(owner string) bool {
			return slices.Contains(module.ReviewOwners, owner)
		},
		CoverImagesDirectory: module.CoverCacheDirectory,
	}
	mux := http.NewServeMux()
	patterns := (&possessionstore.Server{Store: store, Reviewer: reviewer, LocalMachine: s.possessionLocalMachine}).RegisterHandlers(mux)
	s.possessions.Store(&possessionStores{store: store, reviewer: reviewer, handler: mux, patterns: patterns})
	return nil
}

// possessionOutboundClient is the one client possession-store's calls to
// other stores go through: the in-process hosts to their handlers as the
// caller in the request context, every other host over the network.
func (s *Server) possessionOutboundClient() *http.Client {
	return &http.Client{
		Transport: photoFacePassTransport{
			byHost: map[string]http.Handler{
				possessionPhotoStoreHost:    s.possessionInProcessStore("photo-store", func(stores *photosAndLocationsStores) http.Handler { return stores.photoHandler }),
				possessionLocationStoreHost: s.possessionInProcessStore("location-store", func(stores *photosAndLocationsStores) http.Handler { return stores.locationHandler }),
				possessionMailstackHost:     http.HandlerFunc(s.possessionMailstack),
				possessionBudgetStoreHost:   http.HandlerFunc(s.possessionBudgetStore),
			},
			network: http.DefaultTransport,
		},
		Timeout:       15 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// possessionInProcessStore serves a store's handler with only the caller the
// request context carries, which possession-store set to its request's
// caller or to the owner it acts for; a request with none is refused.
func (s *Server) possessionInProcessStore(storeName string, handlerOf func(*photosAndLocationsStores) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ownership.CallerFromContext(r.Context()); !ok {
			writeJSONError(w, http.StatusUnauthorized, "possession_store_call_without_a_caller", "possession-store asked "+storeName+" with no caller in its context")
			return
		}
		stores := s.photosAndLocations.Load()
		if stores == nil {
			photosAndLocationsOff(storeName, strings.ReplaceAll(storeName, "-", "_")+"_off").ServeHTTP(w, r)
			return
		}
		served := r.Clone(r.Context())
		served.Header = keptHeaders(r.Header)
		handlerOf(stores).ServeHTTP(w, served)
	})
}

// possessionMailstack serves mailstack's /api routes as the principal the
// request context names. Mail is read only for one owner, so a context that
// names no principal is refused rather than read as the internal service.
func (s *Server) possessionMailstack(w http.ResponseWriter, r *http.Request) {
	caller, ok := ownership.CallerFromContext(r.Context())
	if !ok || caller.PrincipalID == "" {
		writeJSONError(w, http.StatusUnauthorized, "possession_store_mail_without_a_principal", "possession-store reads mail only as one owner, and the call names none")
		return
	}
	mail := s.mailstack.Load()
	if mail == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "mailstack_off", errMailstackOff.Error())
		return
	}
	served := r.Clone(mailstack.ContextWithCaller(r.Context(), mailstack.Caller{PrincipalID: caller.PrincipalID}))
	served.Header = keptHeaders(r.Header)
	mail.Handler().ServeHTTP(w, served)
}

// possessionBudgetStore serves budget-store's routes as the owner the request
// context names, in that owner's own books. Every call possession-store makes
// there acts for one owner's possessions, so a context that names no
// principal is refused, and an administrator's sight of every owner's books
// is not passed on.
func (s *Server) possessionBudgetStore(w http.ResponseWriter, r *http.Request) {
	caller, ok := ownership.CallerFromContext(r.Context())
	if !ok || caller.PrincipalID == "" {
		writeJSONError(w, http.StatusUnauthorized, "possession_store_budget_without_a_principal", "possession-store asks budget-store only as one owner, and the call names none")
		return
	}
	module := s.budgetStore.Load()
	if module == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "budget_store_off", "budget-store is not open on this server: EnableBudgetStore was never called")
		return
	}
	served := r.Clone(ownership.ContextWithCaller(r.Context(), ownership.Caller{PrincipalID: caller.PrincipalID}))
	served.Header = keptHeaders(r.Header)
	module.handler.ServeHTTP(w, served)
}

// keptHeaders are the only request headers an in-process store sees.
func keptHeaders(header http.Header) http.Header {
	kept := http.Header{}
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := header.Get(name); value != "" {
			kept.Set(name, value)
		}
	}
	return kept
}

// possessionModelPrice is model-store's list price for a model, from the
// module in this process.
func (s *Server) possessionModelPrice(modelID string) (possessionstore.ModelPrice, error) {
	input, output, known := s.ModelListPrice(modelID)
	if !known {
		return possessionstore.ModelPrice{}, fmt.Errorf("model-store has no price for %s", modelID)
	}
	return possessionstore.ModelPrice{InputCost: input, OutputCost: output}, nil
}

// possessionLocalMachine is the name of the one machine harness-store records
// as this server's own (transport local).
func (s *Server) possessionLocalMachine(context.Context) (string, error) {
	machines, err := s.harnessStore.ListMachines()
	if err != nil {
		return "", err
	}
	var local []string
	for _, machine := range machines {
		if machine.Transport == "local" {
			local = append(local, machine.Name)
		}
	}
	if len(local) != 1 {
		return "", fmt.Errorf("harness-store records %d local machines, not one", len(local))
	}
	return local[0], nil
}

// possessionHarnessMachines is harness-store, in this process, as
// possession-store checks a computer's harness_machine_id.
type possessionHarnessMachines struct{ server *Server }

func (machines possessionHarnessMachines) MachineExists(_ context.Context, machineID string) (bool, error) {
	all, err := machines.server.harnessStore.ListMachines()
	if err != nil {
		return false, err
	}
	for _, machine := range all {
		if machine.ID == machineID {
			return true, nil
		}
	}
	return false, nil
}

// PossessionOriginalFiles is file-store, in this process, as possession-store
// keeps originals: one file per original an owner keeps, owner service
// possession-store, owner ref the original's sha256, uploaded by the owner.
func PossessionOriginalFiles(files *filestore.Store, limits func() filestore.Limits) possessionstore.OriginalFiles {
	return possessionOriginalFiles{files: files, limits: limits, carry: filestore.CopyBlobs}
}

// PossessionOriginalFilesCarrying is PossessionOriginalFiles with KeepCopyOf
// putting each file in by carry: an import passes filestore.LinkBlobs to bring
// a standalone store's originals in as hard links, which take no room.
func PossessionOriginalFilesCarrying(files *filestore.Store, limits func() filestore.Limits, carry filestore.BlobCarry) possessionstore.OriginalFiles {
	return possessionOriginalFiles{files: files, limits: limits, carry: carry}
}

type possessionOriginalFiles struct {
	files  *filestore.Store
	limits func() filestore.Limits
	carry  filestore.BlobCarry
}

// possessionOriginalOwnerService is the owner_service of every original's file.
const possessionOriginalOwnerService = "possession-store"

func (originals possessionOriginalFiles) newFile(ownerPrincipalID, sum, mediaType string) filestore.NewFile {
	return filestore.NewFile{ContentType: mediaType, Filename: sum,
		OwnerService: possessionOriginalOwnerService, OwnerRef: sum, UploadedByPrincipalID: ownerPrincipalID}
}

func (originals possessionOriginalFiles) Keep(_ context.Context, ownerPrincipalID, sum, mediaType string, content []byte) (string, error) {
	file, err := originals.files.Put(bytes.NewReader(content), originals.limits().MaximumFileBytes, originals.newFile(ownerPrincipalID, sum, mediaType))
	if err != nil {
		return "", fmt.Errorf("file-store, keeping original %s: %w", sum, err)
	}
	return file.ID, nil
}

func (originals possessionOriginalFiles) KeepCopyOf(_ context.Context, ownerPrincipalID, sum, mediaType, path string) (string, error) {
	file, err := originals.files.PutCarried(path, originals.carry, originals.newFile(ownerPrincipalID, sum, mediaType))
	if err != nil {
		return "", fmt.Errorf("file-store, carrying original %s: %w", sum, err)
	}
	return file.ID, nil
}

func (originals possessionOriginalFiles) Open(_ context.Context, fileID string) (io.ReadCloser, error) {
	file, err := originals.files.Get(fileID, false)
	if err != nil {
		return nil, fmt.Errorf("file-store, original %s: %w", fileID, err)
	}
	if file.OwnerService != possessionOriginalOwnerService {
		return nil, fmt.Errorf("file-store's %s belongs to %s, not to possession-store", fileID, file.OwnerService)
	}
	content, err := originals.files.OpenContent(file)
	if err != nil {
		return nil, fmt.Errorf("file-store, original %s: %w", fileID, err)
	}
	return content, nil
}

func (originals possessionOriginalFiles) Destroy(_ context.Context, fileID string) error {
	if err := originals.files.Purge(fileID); err != nil {
		return fmt.Errorf("file-store, destroying original %s: %w", fileID, err)
	}
	return nil
}

func (s *Server) possessionStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(possessionStoreMountPrefix, "possession-store", "possession_store_off",
		func(*connectedRecordsStores) http.Handler {
			stores := s.possessions.Load()
			if stores == nil {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					writeJSONError(w, http.StatusServiceUnavailable, "possession_store_off", "possession-store is not open on this server: EnablePossessions was never called")
				})
			}
			return stores.handler
		}, false)
}

// registerPossessionsRoutes mounts possession-store, every route the store
// registers, by its description.
func (s *Server) registerPossessionsRoutes() {
	route := s.possessionStoreRoute()
	for pattern := range possessionStoreRouteDescriptions() {
		s.mux.HandleFunc(pattern, route)
	}
}

// StartPossessionReviewWorkers runs possession-store's review worker for each
// owner possession_store.review_worker.owner_principal_ids names, as that
// owner, until ctx ends, when possession_store.review_worker.enabled is on.
// main calls it after EnablePossessions, once every store the worker asks is
// open.
func (s *Server) StartPossessionReviewWorkers(ctx context.Context) error {
	if !s.settings.Boolean(config.SettingPossessionStoreReviewWorkerEnabled) {
		log.Printf("possession-store: the review worker is off (%s); queued photos wait", config.SettingPossessionStoreReviewWorkerEnabled)
		return nil
	}
	stores := s.possessions.Load()
	if stores == nil {
		return errors.New("possession-store review worker: EnablePossessions must run first")
	}
	owners := s.settings.StringList(config.SettingPossessionStoreReviewWorkerOwnerPrincipalIDs)
	for _, owner := range owners {
		if err := ownership.CheckPrincipalID(config.SettingPossessionStoreReviewWorkerOwnerPrincipalIDs, owner); err != nil {
			return err
		}
	}
	if len(owners) == 0 {
		log.Printf("possession-store: the review worker is on, and %s names no owner, so it reviews nobody's photos", config.SettingPossessionStoreReviewWorkerOwnerPrincipalIDs)
		return nil
	}
	for _, owner := range owners {
		ownerContext := ownership.ContextWithCaller(ctx, ownership.Caller{PrincipalID: owner})
		reviewer, err := stores.reviewer.NarrowedToOwner(ownerContext, owner)
		if err != nil {
			return fmt.Errorf("possession-store review worker for %s: %w", owner, err)
		}
		go reviewer.Run(ownerContext)
	}
	log.Printf("possession-store: the review worker reviews the queued photos of %d owners", len(owners))
	return nil
}

// ClosePossessions closes every pool possession-store opened.
func (s *Server) ClosePossessions() {
	stores := s.possessions.Load()
	if stores == nil {
		return
	}
	if err := stores.store.Close(); err != nil {
		log.Printf("postgres: closing possession-store: %v", err)
	}
}
