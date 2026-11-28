package server

// possession-store moved in from the standalone service: the same routes
// under /possessions/, answered from this process. Like the other
// connected records stores, every record has an owner and the store narrows
// every read to the caller's own, or every owner's for an administrator and
// for this server's own service acting as itself, and every write and pass to
// one owner (internal/connectedrecords/possessions/server_scope.go). The
// caller reaches the store in the request context, set by
// connectedRecordsRoute from the credential request authorization verified.
//
// What the store asks on the way is in this process too, asked as the
// caller in the request context: photo-store and location-store through
// their handlers, mailstack and budget-store through their handlers as that
// principal, harness-store through its Go store, model calls through the
// one-shot router priced at model-store's list price, and originals' bytes in
// the file-store module. Only article-store is outside this process, called
// over HTTP at possessions.article_store_url.
//
// The photo review and the background reading of covers' wearer boxes spend
// money on model calls, so they run only when
// possessions.review_worker.enabled is on, for the owners
// possessions.review_worker.owner_principal_ids names, each as that
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

	"github.com/kayushkin/llm-bridge-server/internal/server/routeaccess"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
)

// possessionsMountPrefix is where possession-store is mounted.
const possessionsMountPrefix = "/possessions"

// possessionStores is possession-store as this server serves it.
type possessionStores struct {
	store    *possessions.Store
	reviewer *possessions.Reviewer
	handler  http.Handler
	patterns []string
}

// PossessionsModule is what EnablePossessions needs that this server does not
// already hold: the way to open the module's pools, and the store's settings.
type PossessionsModule struct {
	Open                   possessions.PoolOpener
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
// once, after EnableFiles, EnablePhotosAndLocations, EnableMail and
// EnableBudget, and before the server listens. Until then its routes answer 503.
func (s *Server) EnablePossessions(module PossessionsModule) error {
	files := s.fileStoreModule.Load()
	if files == nil {
		return errors.New("possession-store: EnableFiles must run first, since possession-store keeps its originals in file-store")
	}
	if s.photosAndLocations.Load() == nil {
		return errors.New("possession-store: EnablePhotosAndLocations must run first, since possession-store reads photos and checks places there")
	}
	if s.budgetStore.Load() == nil {
		return errors.New("possession-store: EnableBudget must run first, since possession-store records eBay orders and checks transactions there")
	}
	if s.harnessStore == nil {
		return errors.New("possession-store: this server has no harness-store, where a computer's machine is checked")
	}
	if module.Open == nil || module.OwnerLocation == nil || module.CoverCacheDirectory == "" {
		return errors.New("possession-store: the pool opener, the owner's time zone and the cover cache directory are required")
	}
	photos := possessionPhotoStore{server: s}
	budget := possessionBudgetStore{server: s}
	references := possessions.ServiceReferences{
		Photos:          photos,
		Places:          possessionPlaces{server: s},
		Transactions:    budget,
		ArticleStoreURL: strings.TrimRight(module.ArticleStoreURL, "/"),
		// article-store runs apart, so its checks go over the network, bounded
		// and with no redirect followed, as they were.
		HTTPClient: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Machines:   possessionHarnessMachines{server: s},
	}
	store, err := possessions.New(module.Open, references, PossessionOriginalFiles(files, s.fileStoreLimits))
	if err != nil {
		return err
	}
	store.UseBudgetStore(budget)
	reviewer := &possessions.Reviewer{
		Store:  store,
		Models: possessions.OneShotModelCaller{RunRole: s.oneShotRouter.RunRole, ListPrice: s.possessionModelPrice},
		Photos: possessions.PhotoStoreSource{
			PhotoStore:  photos,
			Originals:   store,
			LongestSide: module.ReviewImageLongestSide,
		},
		FirstReaderRole:  module.FirstReaderRole,
		SecondReaderRole: module.SecondReaderRole,
		Articles:         possessions.ArticleStore{URL: strings.TrimRight(module.ArticleStoreURL, "/"), HTTPClient: &http.Client{Timeout: time.Minute}},
		Budget:           budget,
		Mail:             possessionMail{server: s},
		OwnerLocation:    module.OwnerLocation,
		ReadCoverWearersInBackgroundFor: func(owner string) bool {
			return slices.Contains(module.ReviewOwners, owner)
		},
		CoverImagesDirectory: module.CoverCacheDirectory,
	}
	mux := http.NewServeMux()
	possessionServer := &possessions.Server{Store: store, Reviewer: reviewer, LocalMachine: s.possessionLocalMachine}
	patterns := possessionServer.RegisterHandlers(mux)
	s.referenceReadable.possessionsAt.Store(possessionServer)
	s.possessions.Store(&possessionStores{store: store, reviewer: reviewer, handler: mux, patterns: patterns})
	return nil
}

// possessionModelPrice is model-store's list price for a model, from the
// module in this process.
func (s *Server) possessionModelPrice(modelID string) (possessions.ModelPrice, error) {
	input, output, known := s.ModelListPrice(modelID)
	if !known {
		return possessions.ModelPrice{}, fmt.Errorf("model-store has no price for %s", modelID)
	}
	return possessions.ModelPrice{InputCost: input, OutputCost: output}, nil
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
func PossessionOriginalFiles(fileStore *files.Store, limits func() files.Limits) possessions.OriginalFiles {
	return possessionOriginalFiles{files: fileStore, limits: limits, carry: files.CopyBlobs}
}

// PossessionOriginalFilesCarrying is PossessionOriginalFiles with KeepCopyOf
// putting each file in by carry: an import passes files.LinkBlobs to bring
// a standalone store's originals in as hard links, which take no room.
func PossessionOriginalFilesCarrying(files *files.Store, limits func() files.Limits, carry files.BlobCarry) possessions.OriginalFiles {
	return possessionOriginalFiles{files: files, limits: limits, carry: carry}
}

type possessionOriginalFiles struct {
	files  *files.Store
	limits func() files.Limits
	carry  files.BlobCarry
}

// possessionOriginalOwnerService is the owner_service of every original's file.
const possessionOriginalOwnerService = "possession-store"

func (originals possessionOriginalFiles) newFile(ownerPrincipalID, sum, mediaType string) files.NewFile {
	return files.NewFile{ContentType: mediaType, Filename: sum,
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
	return s.connectedRecordsRoute(possessionsMountPrefix, "possession-store", "possession_store_off",
		func(*connectedRecordsStores) http.Handler {
			stores := s.possessions.Load()
			if stores == nil {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "possession_store_off", "possession-store is not open on this server: EnablePossessions was never called")
				})
			}
			return stores.handler
		}, false)
}

// registerPossessionsRoutes mounts possession-store, every route the store
// registers, by its description.
func (s *Server) registerPossessionsRoutes() {
	route := s.possessionStoreRoute()
	for pattern := range routeaccess.PossessionStoreRouteDescriptions() {
		s.mux.HandleFunc(pattern, route)
	}
}

// StartPossessionReviewWorkers runs possession-store's review worker for each
// owner possessions.review_worker.owner_principal_ids names, as that
// owner, until ctx ends, when possessions.review_worker.enabled is on.
// main calls it after EnablePossessions, once every store the worker asks is
// open.
func (s *Server) StartPossessionReviewWorkers(ctx context.Context) error {
	if !s.settings.Boolean(config.SettingPossessionsReviewWorkerEnabled) {
		log.Printf("possession-store: the review worker is off (%s); queued photos wait", config.SettingPossessionsReviewWorkerEnabled)
		return nil
	}
	stores := s.possessions.Load()
	if stores == nil {
		return errors.New("possession-store review worker: EnablePossessions must run first")
	}
	owners := s.settings.StringList(config.SettingPossessionsReviewWorkerOwnerPrincipalIDs)
	for _, owner := range owners {
		if err := ownership.CheckPrincipalID(config.SettingPossessionsReviewWorkerOwnerPrincipalIDs, owner); err != nil {
			return err
		}
	}
	if len(owners) == 0 {
		log.Printf("possession-store: the review worker is on, and %s names no owner, so it reviews nobody's photos", config.SettingPossessionsReviewWorkerOwnerPrincipalIDs)
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
