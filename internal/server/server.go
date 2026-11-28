package server

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/login"

	"github.com/kayushkin/llm-bridge-server/internal/peers"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/parkedasks"

	"github.com/kayushkin/llm-bridge-server/internal/sessionactions"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"

	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/agents"
	"github.com/kayushkin/llm-bridge-server/internal/agents/bundles"
	"github.com/kayushkin/llm-bridge-server/internal/agents/hooks"
	"github.com/kayushkin/llm-bridge-server/internal/agents/memories"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelcredentials"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/oneshot"
	"github.com/kayushkin/llm-bridge-server/internal/agents/skills"
	"github.com/kayushkin/llm-bridge-server/internal/agents/tools"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationroutes"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/httproutes"
	"github.com/kayushkin/llm-bridge-server/internal/keepawake"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/mail"
	"github.com/kayushkin/llm-bridge-server/internal/references"
	"github.com/kayushkin/llm-bridge-server/internal/serviceinventory"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/briefs"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	itemsapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictions"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
	"github.com/kayushkin/llm-bridge-server/ui"
	"github.com/kayushkin/llm-bridge/servicesettings"

	"github.com/kayushkin/llm-bridge-server/internal/machines/machineroutes"
	"github.com/kayushkin/llm-bridge-server/internal/machines/runnerprotocol"
)

type Server struct {
	discoveryMu sync.Mutex
	// PeerProxyCalls shares identical reads through the connection proxy and
	// limits how often each connected server is called (peer_proxy_calls.go).
	mux           *httproutes.RecordingMux
	store         *store.Store
	agentStore    *agents.Store
	memoryStore   *memories.Store
	harnessStore  *machines.Store
	hookStore     *hooks.Store
	modelStore    *models.Store
	snapshotStore *snapshots.Store
	// predictionStore answers /predictions/ once the Postgres module is
	// open (prediction_store_routes.go); nil answers those routes 503.
	predictionStore atomic.Pointer[predictions.Handlers]
	// sessionLog is log-store in this process; see EnableSessionLog.
	sessionLog atomic.Pointer[sessionLogModule]
	// notes answers /notes/ once its Postgres module is open
	// (notes_routes.go); nil answers those routes 503.
	notes atomic.Pointer[itemsapi.API]
	// issueItems answers /issues/items, the content of issues, once the
	// issues module is open (EnableIssues); nil answers those routes 503.
	issueItems atomic.Pointer[itemsapi.API]
	// projectStore answers /projects/ once its Postgres module is open
	// (project_store_routes.go); nil answers those routes 503.
	projectStore atomic.Pointer[projects.Handlers]
	// userInterfaceBuild is the built front end served at /ui/ (user_interface.go).
	userInterfaceBuild fs.FS
	harness            *harness.Manager
	// harnessStartLockBySessionID holds a *sync.Mutex per session id, so
	// StartOnInstance spawns at most one harness process per session.
	harnessStartLockBySessionID sync.Map
	authClient                  *authstoreclient.Client
	// permissionEngine, grantStore and principalStore are the access stores in
	// this process (access_routes.go): permission-store's verdict on a tool
	// call, a session principal's effective grants at spawn, and the
	// principal behind every request. EnableAccess sets them; nil until then,
	// and every use of a nil one answers that access is off.
	permissionEngine permissionEvaluator
	grantStore       grantcheck.GrantReader
	principalStore   principalDirectory
	// grantsPrincipalDirectory and grantsResourceChecker are what grant-store
	// asks about principals and resources when this server reads or writes
	// grants, as grant-store's own routes do.
	grantsPrincipalDirectory grants.PrincipalDirectory
	grantsResourceChecker    grants.ResourceChecker
	// access answers /principals/, /grants/, /sign-in/ and
	// /permissions/ once EnableAccess has run; nil answers them 503.
	access atomic.Pointer[accessRouteHandlers]
	// peerSockets holds every open peer socket and every dialer (peer_socket.go).
	peerSockets *peers.PeerSocketRegistry
	// PeerRequestsInFlight lets ending a peer's access end the requests it
	// is being served right now (peer_requests_in_flight.go).
	peerRequestsInFlight *peers.PeerRequestsInFlight
	// keepAwake holds this machine awake while sessions run, once
	// StartKeepAwake has run; nil before that and in tests.
	keepAwake *keepawake.Controller
	// restartDrain holds new turns while a deploy waits to restart this
	// server (restart_drain.go).
	restartDrain restartDrainState
	// bundleStore checks a bundle_id at creation and resolves it at spawn;
	// nil while the bundle-store module is off (bundle_store_routes.go).
	bundleStore bundleResolver
	// toolRegistry answers the tool offer, the harness tool denials and the
	// harness tool reports; nil while the tool-store module is off
	// (tool_store_routes.go, tool_registry.go).
	toolRegistry toolRegistry
	// The agents stores moved into this process answer their routes once
	// their Postgres modules are open; nil answers those routes 503.
	toolStoreHandlers  atomic.Pointer[tools.Handlers]
	skillStoreHandlers atomic.Pointer[skills.Handlers]
	// The tool-store and skill-store modules, for grant-store to check that a
	// tool or skill a grant names exists. Nil until EnableTools and
	// EnableSkills, like the handlers above.
	toolStoreForResourceChecks  atomic.Pointer[tools.Store]
	skillStoreForResourceChecks atomic.Pointer[skills.Store]
	bundleStoreHandlers         atomic.Pointer[bundles.Handlers]
	// usageAndEvalStores answers /usage/ and /evaluations/ once their
	// Postgres modules are open (usage_and_eval_store_routes.go).
	usageAndEvalStores usageAndEvalStores
	// automationStores answers /scheduler/ and /rules/ once their
	// Postgres modules are open (automation_routes.go).
	automationStores automationStores
	// referenceReadable are the stores reference-store's readers call
	// (reference_readers.go).
	referenceReadable referenceReadableStores
	// issueReads answers "which noteboard todo is this session linked
	// to?" when a signal is minted, and every other read this server makes of
	// kanban-store. EnableIssues sets it; nil until then, which leaves
	// every signal unlinked rather than guessing.
	issueReads *issuereads.Reader
	// kanbanStore answers /issues/ once EnableIssues
	// has run (kanban_store_routes.go); nil answers them 503.
	kanbanStore atomic.Pointer[kanbanStoreHandlers]

	// mailstack answers /mail/ once EnableMail has run.
	mailstack atomic.Pointer[mail.Server]
	// fileStore keeps the bytes of files shared into sessions: the file-store
	// module in this process, called in Go. Nil until EnableFiles, which
	// switches session files off.
	fileStore sessionfiles.SessionFileStore
	// fileStoreCalls is the same module as the other modules in this process
	// call it (kanban-store's and mailstack's attachments, activity-store's);
	// nil until EnableFiles.
	fileStoreCalls atomic.Pointer[files.Server]
	// The repositories, file_storage and references modules
	// (repositories_references_routes.go); nil answers their routes 503.
	repositories          atomic.Pointer[repositoryStores]
	fileStoreHandler      atomic.Pointer[mountedStore]
	referenceStoreHandler atomic.Pointer[mountedStore]
	referenceResolver     atomic.Pointer[references.Resolver]
	// connectedRecords answers /people/, /calendar/ and
	// /journal/ once EnableConnectedRecords has run
	// (connected_records_routes.go); nil answers them 503.
	connectedRecords atomic.Pointer[connectedRecordsStores]
	// messageActivityAndTimeLedger answers /messages/, /online-activity/
	// and /time-ledger/ once EnableMessagesOnlineActivityAndTimeLedger
	// has run (message_activity_time_ledger_routes.go); nil answers them 503.
	messageActivityAndTimeLedger atomic.Pointer[messageActivityAndTimeLedgerStores]
	// photosAndLocations answers /photos/ and /locations/ once
	// EnablePhotosAndLocations has run (photos_locations_routes.go); nil
	// answers them 503.
	photosAndLocations atomic.Pointer[photosAndLocationsStores]
	// budgetStore answers /budget/ once EnableBudget has run
	// (budget_store_routes.go); nil answers it 503.
	budgetStore atomic.Pointer[budgetStoreModule]
	// possessions answers /possessions/ once EnablePossessions has run
	// (possessions_routes.go); nil answers it 503.
	possessions atomic.Pointer[possessionStores]
	// fileStoreModule is the file-store module EnableFiles was given:
	// photo-store keeps its thumbnails' bytes there, called in Go.
	fileStoreModule atomic.Pointer[files.Store]
	// repoRegistryForDeploys is repo-store as a deploy action reads it; nil
	// until EnableRepositories, which refuses deploy actions.
	repoRegistryForDeploys repoRegistry
	bridgePrefs            *bridgePrefsStore
	cfState                *conformanceState
	sessionHub             *sessionHub
	parkedAsks             *parkedasks.Asks
	responseCache          *responseCache
	// SignalClassifier is the derived signal producer: a cheap-model pass
	// over each turn-end. Nil only in tests that never exercise it.
	signalClassifier *signals.SignalClassifier
	// QuestionTriage stands between a surfacing worker's question and the
	// person on its card. See signal_question_triage.go.
	questionTriage *signals.QuestionTriage
	// mailMessages resolves the sender of a card's mail so a drafted
	// customer reply has a recipient, from mailstack in this process. Nil
	// until EnableMail, and every draft is then minted with an empty To.
	mailMessages signals.MailMessageReader
	cfg          *config.Config
	// serviceInventory is the last reading of the host's services and their
	// databases, held for serviceInventoryMaxAge so a page load that asks for
	// the list and then a schema does not walk /proc twice. See services.go.
	serviceInventoryMu sync.Mutex
	serviceInventory   *serviceinventory.Inventory
	// HarnessChildTokenSigner signs and verifies the session agent tokens
	// and session posting tokens. Always present: New refuses to build a
	// server without a signing key. See session_agent_token.go.
	harnessChildTokenSigner *login.HarnessChildTokenSigner
	// loginClock is what login sessions and login attempts read the time
	// from: time.Now, except in a test that lets one expire.
	loginClock func() time.Time
	// EntraSignIn is Entra ID sign-in, nil when the Entra settings are unset.
	// See entra_sign_in.go.
	entraSignIn *login.EntraSignIn
	// principalLookupCache holds principal-store's answer about a caller —
	// active or disabled, administrator or not — for a few seconds. See
	// request_principal_lookup.go.
	principalLookupCache *principalLookupCache
	// settings is this server's declared settings, with the stored behavior
	// settings attached: read at the time of use, described by GET /settings,
	// changed by PUT /settings/{key}. See service_settings.go.
	settings *servicesettings.Registry
	// operations runs the operation routes. Nil until EnableOperations, and
	// the routes answer 503 until then. See operations.go.
	operations *operations.Coordinator

	// SessionActionModels resolves, calls and prices the models session
	// actions use. The server itself, except in tests.
	sessionActionModels sessionactions.SessionActionModels

	// oneShotRouter is where every background model call goes: a model-store
	// role in, the first of its models that answers out. See
	// oneshot_routing.go.
	oneShotRouter *modelrouting.Router
	// renamesInFlight holds the renamer slot ids of the title calls this
	// process is making, so a slot left by a call a restart killed reads as
	// free. See renamer.go.
	renamesInFlight sync.Map
	// ChatLinks is nil when chat links are off (chat_links.go).
	chatLinkService *chatlinks.Service
	// grantChecker answers what a principal's grants allow; it reads
	// grantStore and principalLookupCache as they are when it is asked.
	grantChecker     *grantcheck.Checker
	briefService     *briefs.Service
	operationRoutes  *operationroutes.Service
	machineRoutes    *machineroutes.Service
	messageSenders   *messagesender.Senders
	modelCredentials *modelcredentials.Service
	oneShot          *oneshot.Service
	sessionActions   *sessionactions.Service
	signals          *signals.Service
	peers            *peers.Service
	login            *login.Service
	sessionFiles     *sessionfiles.Service
}

func New(st *store.Store, as *agents.Store, ms *memories.Store, hs *machines.Store, hks *hooks.Store, mds *models.Store, ss *snapshots.Store, cfg *config.Config) *Server {
	authClient := authstoreclient.New("", "", "llm-bridge-server")
	hub := newSessionHub(st)
	// The chat-page response cache and the SSE hub both need session-row mutation
	// signals; the store takes a single Notifier, so fan out to both. The cache
	// invalidates on any change/delete; the hub publishes list deltas as before.
	respCache := newResponseCache()
	st.SetNotifier(newNotifierFanout(hub, respCache))
	srv := &Server{
		mux:                  httproutes.NewRecordingMux(),
		userInterfaceBuild:   ui.Build(),
		store:                st,
		agentStore:           as,
		memoryStore:          ms,
		harnessStore:         hs,
		hookStore:            hks,
		modelStore:           mds,
		snapshotStore:        ss,
		harness:              harness.NewManager(st, cfg.PublicURL, httproutes.PublicBaseURL(cfg.ListenAddr), cfg.PTYRingBufferBytes, authClient),
		authClient:           authClient,
		peerSockets:          peers.NewPeerSocketRegistry(),
		peerRequestsInFlight: peers.NewPeerRequestsInFlight(),
		bridgePrefs:          newBridgePrefsStore(cfg.BridgePrefsPath),
		cfState:              newConformanceState(cfg.ConformancePath),
		sessionHub:           hub,
		parkedAsks:           parkedasks.New(),
		responseCache:        respCache,
		signalClassifier: signals.NewSignalClassifier(
			cfg.SignalClassifierModelRole,
			cfg.SignalClassifierTimeout,
			cfg.SignalClassifierMaxChars,
			cfg.SignalClassifierOptOut,
			nil, // wired below: it needs the server that owns it
		),
		cfg:      cfg,
		settings: attachServiceSettings(cfg, st),
	}
	// The manager promotes harness subagents to sessions below the HTTP layer,
	// so it needs the same purpose→folder mapping the HTTP layer files
	// sessions with. Wired here because folderForPurpose reads both the config
	// defaults and the source_folders overrides the manager has no access to.
	srv.harness.SetFolderResolver(srv.folderForPurpose)
	// The renamer and the classifier react to turn-ends, so they hang off the
	// manager's observer rather than reaching into the derivation state
	// machine.
	srv.harness.SetTurnEndObserver(srv.onTurnEnd)
	srv.harness.SetUnpromptedTurnStartObserver(srv.signals.OnUnpromptedTurnStart)
	srv.composeServices()
	if cfg.ChatLinksNATSURL != "" && cfg.MultichatURL != "" {
		srv.chatLinkService.ChatLinks = &chatlinks.ChatLinks{
			MultichatURL: cfg.MultichatURL,
			Senders:      chatLinkSendersInPeopleStore{server: srv},
			HTTPClient:   &http.Client{Timeout: 60 * time.Second},
			Token: func(ctx context.Context) (string, error) {
				resolved, err := authClient.ResolveByProvider(ctx, chatlinks.MultichatProvider, "", "", "read multichat's accounts and send a chat link's reply")
				if err != nil {
					return "", err
				}
				return resolved.Secret(), nil
			},
			Kick: make(chan struct{}, 1),
			Now:  func() time.Time { return time.Now().UTC() },
		}
		srv.chatLinkService.ChatLinks.SendIntoSession = srv.chatLinkService.SendIntoSessionAsTheServer
	} else {
		log.Printf("[chat-links] off: %v", chatlinks.ErrOff)
	}
	srv.harness.SetSessionInfoObserver(srv.onSessionInfo)
	// The classifier runs its call through the server's one-shot router, so
	// it is wired here, after the server that owns the router exists.
	srv.oneShotRouter = srv.oneShot.NewOneShotRouter()
	if srv.signalClassifier != nil {
		srv.signalClassifier.OneShotRunner = srv.oneShot.RoutedOneShotJSON
	}
	// Triage shares the classifier's timeout and router, and has its own
	// role (question_triage.model_role), so either can move to another model
	// without the other.
	srv.questionTriage = signals.NewQuestionTriage(cfg.QuestionTriageModelRole, cfg.SignalClassifierTimeout, srv.oneShot.RoutedOneShotJSON)
	// Every request this server answers is authorized first, and it cannot
	// identify a caller without these. main refuses to start on the same
	// check plus the store addresses; reaching this means a caller skipped it,
	// and a server that gated nothing while looking gated is worse than one
	// that does not start.
	if err := cfg.ValidateRequestAuthorizationCredentials(); err != nil {
		panic(fmt.Sprintf("request authorization: %v", err))
	}
	srv.harnessChildTokenSigner = &login.HarnessChildTokenSigner{
		SigningKey: []byte(cfg.DemoLoginSigningKey),
		Now:        time.Now,
	}
	srv.loginClock = time.Now
	// A half-configured Entra sign-in is refused here as main refuses it, so
	// no server starts with sign-in silently off.
	if err := cfg.ValidateEntraSignInSettings(); err != nil {
		panic(fmt.Sprintf("entra sign-in: %v", err))
	}
	if cfg.EntraSignInEnabled() {
		srv.entraSignIn = login.NewEntraSignIn(cfg)
	}
	srv.principalLookupCache = newPrincipalLookupCache(srv)
	srv.sessionActionModels = sessionActionModelsInThisServer{Service: srv.oneShot, server: srv}
	srv.wireServiceSettings()
	srv.routes()
	srv.syncHarnessTypes()
	srv.syncPromptHarnessDeliveries()
	srv.sessionActions.SettleSessionActionsLeftRunning()
	srv.syncSourceFolderRegistry()
	srv.startSnapshotGC()
	return srv
}

// syncHarnessTypes seeds the harness-store with harness type metadata.
func (s *Server) syncHarnessTypes() {
	if s.harnessStore == nil {
		return
	}
	for _, h := range allHarnesses {
		meta := harnessMetadata[h]
		if err := s.harnessStore.UpsertHarnessType(&machines.HarnessType{
			Name:  h,
			Label: meta.Label,
			Emoji: meta.Emoji,
			Image: meta.Image,
		}); err != nil {
			log.Printf("sync harness type %s: %v", h, err)
		}
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.registerLoginAndStoreProxyRoutes()
	s.registerPredictionStoreRoutes()
	s.registerSessionLogRoutes()
	s.registerToolStoreRoutes()
	s.registerSkillStoreRoutes()
	s.registerAgentsRegistryRoutes()
	s.registerBundleStoreRoutes()
	s.registerNotesRoutes()
	s.registerIssueItemsRoutes()
	s.registerProjectStoreRoutes()
	s.registerAccessRoutes()
	s.registerRepositoriesAndReferencesRoutes()
	s.registerUsageAndEvalStoreRoutes()
	s.registerConnectedRecordsRoutes()
	s.registerMessageActivityAndTimeLedgerRoutes()
	s.registerAutomationRoutes()
	s.registerPhotosAndLocationsRoutes()
	s.registerBudgetStoreRoutes()
	s.registerPossessionsRoutes()
	// This server's own settings, served the way every service serves them.
	serviceSettingsHandler := servicesettings.Handler(s.settings, "/settings")
	s.mux.Handle("GET /settings", serviceSettingsHandler)
	s.mux.Handle("PUT /settings/{key}", serviceSettingsHandler)
	s.mux.HandleFunc("GET /harnesses", s.handleHarnesses)
	s.mux.HandleFunc("GET /harnesses/{name}/capabilities", s.handleHarnessCapabilities)
	s.mux.HandleFunc("GET /harnesses/{name}/agents", s.handleHarnessAgents)

	// Static harness images
	s.mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(s.cfg.ImagesDir))))

	// The front end, compiled into this binary. The pattern is written out,
	// not built from userInterfacePathPrefix, because the route classification
	// test reads patterns from this file's string literals.
	s.mux.HandleFunc("GET /ui/", s.handleUserInterface)
	s.mux.HandleFunc("GET /{$}", s.handleRootRedirectsToUserInterface)

	// Session routes
	s.mux.HandleFunc("GET /sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /session-events", s.handleSessionListEvents)
	// chat-page endpoints — projected sidebar list, recent-bundle warmer,
	// and the cheap validators staleness check. See dash docs/chat-architecture.md §5.
	s.mux.HandleFunc("GET /sessions/summary", s.handleSessionsSummary)
	s.mux.HandleFunc("GET /sessions/principals", s.handleSessionPrincipals)
	// Same query, POST body: the id lookups (session_ids, manager_session_ids)
	// outgrow a URL — a long-enough query string kills the whole HTTP/2
	// connection at nginx, not just the request. See handleSessionsSummaryLookup.
	s.mux.HandleFunc("POST /sessions/summary", s.handleSessionsSummaryLookup)
	s.mux.HandleFunc("GET /sessions/recent-bundle", s.handleRecentBundle)
	s.mux.HandleFunc("GET /sessions/validators", s.handleSessionsValidators)
	// Same check, POST body — the id list grows with the client's cache and a
	// growing query string walks toward nginx's HTTP/2 connection-kill cliff.
	s.mux.HandleFunc("POST /sessions/validators", s.handleSessionsValidatorsLookup)
	s.mux.HandleFunc("GET /sessions/search", s.handleSearchSessions)
	s.mux.HandleFunc("GET /sessions/discover", s.handleDiscoverSessions)
	s.mux.HandleFunc("POST /sessions", s.touchingKeepAwake(s.handleCreateSession))
	s.mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("POST /sessions/{id}/send", s.touchingKeepAwake(s.handleSendMessage))
	s.sessionFiles.Mount(s.mux)
	s.mux.HandleFunc("GET /sessions/{id}/files-named-by-tools", s.handleFilesNamedByTools)
	s.mux.HandleFunc("GET /sessions/{id}/files-named-by-tools/content", s.handleFileNamedByToolsContent)
	s.mux.HandleFunc("GET /sessions/{id}/files-named-by-tools/image", s.handleImageNamedByTools)
	s.chatLinkService.Mount(s.mux)
	s.sessionActions.Mount(s.mux)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.handleSessionEvents)
	// Pty-mode session attach. Bidirectional WebSocket bound to the
	// session's pseudoterminal fd; rejected for sessions started in
	// events mode. Single-writer in v1 (child 2); resize / multi-reader
	// land in child 3.
	s.mux.HandleFunc("GET /sessions/{id}/attach", s.handleAttachSession)
	s.mux.HandleFunc("GET /sessions/{id}/attach-token", s.handleGetAttachToken)
	s.mux.HandleFunc("GET /sessions/aggregates", s.handleSessionAggregates)
	s.mux.HandleFunc("GET /sessions/{id}/messages", s.handleSessionMessages)
	// The unprojected page, for the Raw pane and for audit. `/messages` is projected
	// by log-store — no `raw` payloads, no duplicate entries — because the Turns view
	// renders about a tenth of what was being sent (9.91 MB against 0.95 MB, measured
	// on one real session). This route is how a caller asks for the rest, and it is a
	// route rather than a query param so nobody requests ten times the bytes by
	// accident. Proxied unchanged, like every other log-store path.
	s.mux.HandleFunc("GET /sessions/{id}/messages/raw", s.handleSessionMessages)
	// One entry of the reading page with its tool input and output in full. A page's
	// entries carry tool payloads shortened to 2 KB (`payload=preview`, log-store
	// stored_turns.go); a card that is opened fetches the whole entry here.
	s.mux.HandleFunc("GET /sessions/{id}/entries/{eventId}", s.proxyToLogStore)
	s.mux.HandleFunc("GET /sessions/{id}/entries/{eventId}/images/{index}", s.proxyToLogStore)
	s.mux.HandleFunc("POST /sessions/{id}/interrupt", s.handleInterruptSession)
	s.mux.HandleFunc("POST /sessions/{id}/resume", s.handleResumeSession)
	s.mux.HandleFunc("POST /sessions/{id}/stop", s.handleStopSession)
	s.mux.HandleFunc("POST /sessions/{id}/mode", s.handleSwitchMode)
	s.mux.HandleFunc("POST /sessions/{id}/compact", s.handleCompactSession)
	s.mux.HandleFunc("POST /sessions/{id}/fork", s.handleForkSession)
	s.mux.HandleFunc("POST /sessions/{id}/rename", s.handleRenameSession)
	s.mux.HandleFunc("POST /sessions/{id}/config", s.handleConfigSession)
	s.mux.HandleFunc("PUT /sessions/{id}/folder", s.handleSetSessionFolder)
	s.mux.HandleFunc("POST /sessions/{id}/mark-done", s.handleMarkSessionDone)
	s.mux.HandleFunc("GET /sessions/{id}/git/repos", s.handleSessionGitRepos)
	s.mux.HandleFunc("GET /sessions/{id}/git", s.handleSessionGit)
	s.mux.HandleFunc("GET /sessions/{id}/effective-config", s.handleSessionEffectiveConfig)
	s.mux.HandleFunc("GET /effective-config", s.handleDryRunEffectiveConfig)

	// Hook resolution — surface awaiting_resolution HookEvents and accept
	// a decision back. Used by bridge-ui to render permission prompts and
	// human-resolved hooks. Pending list lets a freshly-connected client
	// recover banner state without replaying the full event stream.
	s.mux.HandleFunc("GET /sessions/{id}/hooks/pending", s.handleListPendingHooks)
	s.mux.HandleFunc("POST /sessions/{id}/hooks/{request_id}/resolve", s.handleResolveHook)

	// Session signals — the canonical record of anything a session surfaces
	// to a human (docs/SESSION-SIGNALS.md). Rows are written by the tool path
	// when an AskUserQuestion parks and by the turn-end classifier, and are
	// closed out by whichever path delivers the answer.
	// /signals?state=open is the cross-session inbox; /signals/{id}/resolve
	// is the close verb for the answers that never reach a session — a
	// notification acknowledgement and a dismissal.
	// POST /sessions/{id}/signals is the structured notification producer:
	// a session raising a notification about itself, deliberately, mid-turn.
	s.signals.Mount(s.mux)
	// The ONE way to answer a question, whichever producer raised it and
	// whether or not its session is still running. The server picks the
	// delivery — a live parked hook, or the session's next message — because
	// a request_id only says a park EXISTED, and only the server knows if it
	// is still live. See signal_answer.go.

	s.briefService.Mount(s.mux)

	// Operations: asynchronous work with a durable receipt. See operations.go
	// and docs/OPERATIONS.md in llm-bridge.
	s.operationRoutes.Mount(s.mux)

	// The Services page: healthcheck's services joined to the SQLite files
	// their processes hold open, and read-only reads of those files. See
	// services.go for what a path must be before it can be read.
	s.mux.HandleFunc("GET /services", s.handleListServices)
	s.mux.HandleFunc("GET /services/databases/schema", s.handleDatabaseSchema)
	s.mux.HandleFunc("GET /services/databases/rows", s.handleDatabaseRows)

	// PreToolUse permission gate for Claude Code. Wired into every CC
	// session via buildClaudeCodeSettings's --settings injection so CC
	// posts here on every tool call. Sole permission gate now that the
	// embedded bridge_perm MCP is gone.
	s.mux.HandleFunc("POST /permission/cc-prehook/{bridge_id}", s.handleCCPermissionPrehook)

	// PreToolUse permission gate for codex. Wired in via
	// buildCodexHookConfig — codex's hook config carries a curl command
	// pointing here on every Bash/apply_patch/MCP tool call. Codex's
	// PreToolUse payload + hookSpecificOutput.PermissionDecision shapes
	// are byte-identical to CC's, so the same handler serves both URLs.
	s.mux.HandleFunc("POST /permission/codex-prehook/{bridge_id}", s.handleCCPermissionPrehook)

	// PreToolUse permission gate for devin. llm-bridge-devin writes
	// devin_hooks (injectDevinHookSettings) into the config devin starts
	// with; devin's PreToolUse payload and its reading of
	// hookSpecificOutput.PermissionDecision match CC's, so the same handler
	// serves it. The wrapper also posts here for a session/request_permission.
	s.mux.HandleFunc("POST /permission/devin-prehook/{bridge_id}", s.handleCCPermissionPrehook)

	// Sidecar event ingest for PTY-mode sessions. llm-bridge-claudecode's
	// -otel-sidecar process POSTs translated OTel events here so they flow
	// through the normal store + SSE + derivation pipeline. PTY sessions
	// have no in-process receiver because the harness exec's directly
	// into claude. See internal/harness/process.go's StartProcessPTY for
	// how the sidecar is spawned and the env wiring on the PTY child.
	s.mux.HandleFunc("POST /sidecar/event/{bridge_id}", s.handleSidecarEvent)

	// Global permission mode — persisted in bridge-prefs. Used as the
	// snapshot source when new sessions are created and as the fallback
	// for legacy sessions that pre-date the per-session snapshot. One of
	// "ask" / "auto" / "bypass".
	s.mux.HandleFunc("POST /bridge/permission-mode", s.handleSetGlobalPermissionMode)
	// Per-session permission mode override — persisted in
	// session.harness_config and read live by the PreToolUse prehook;
	// also forwarded to the harness as a start param on next spawn/resume.
	// Wins over the global.
	s.mux.HandleFunc("PUT /sessions/{id}/permission-mode", s.handleSetSessionPermissionMode)
	// Legacy boolean aliases — older bridge-ui builds keep working until
	// they're rebuilt against the new endpoint. true → bypass, false → ask.
	s.mux.HandleFunc("POST /bridge/bypass-permissions", s.handleSetBypassPermissions)
	s.mux.HandleFunc("PUT /sessions/{id}/bypass-permissions", s.handleSetSessionBypass)

	// Folder registry — sidebar organization for sessions
	s.mux.HandleFunc("GET /folders", s.handleListFolders)
	s.mux.HandleFunc("POST /folders", s.handleCreateFolder)
	s.mux.HandleFunc("DELETE /folders/{name}", s.handleDeleteFolder)
	s.mux.HandleFunc("PUT /folders/{name}", s.handleRenameFolder)

	// Source-folder mapping — runtime overrides for env-var defaults
	s.mux.HandleFunc("GET /source-folders", s.handleListSourceFolders)

	// Session classification vocabulary, and the sessions that disagree with
	// it. Read by session-taxonomy-guard; see internal/server/taxonomy.go.
	s.mux.HandleFunc("GET /session-taxonomy", s.handleSessionTaxonomy)
	s.mux.HandleFunc("GET /session-taxonomy/report", s.handleSessionTaxonomyReport)
	s.mux.HandleFunc("PUT /source-folders/{source}", s.handlePutSourceFolder)
	s.mux.HandleFunc("DELETE /source-folders/{source}", s.handleDeleteSourceFolder)

	// Agent-store routes (mounted from agent-store library). The hook
	// callbacks let agent-store nudge connected runners to reconcile when
	// the canonical context files change.
	if s.agentStore != nil {
		agents.RegisterHandlersWithHookSet(s.mux, s.agentStore, s.agentStoreHandlerHooks())
	}

	// Memory-store routes (mounted from memory-store library)
	if s.memoryStore != nil {
		memories.RegisterHandlers(s.mux, s.memoryStore)
	}

	// Instance routes only available if harness-store is loaded
	s.machineRoutes.Mount(s.mux)
	s.oneShot.Mount(s.mux)

	// Hook registry routes (mounted only when hook-store is loaded).
	// /hooks/exec/{id} is always registered because the hook-store is
	// where registered hooks are resolved; without the store, exec 404s.
	if s.hookStore != nil {
		s.mux.HandleFunc("POST /hooks", s.handleCreateHook)
		s.mux.HandleFunc("GET /hooks", s.handleListHooks)
		s.mux.HandleFunc("GET /hook-options", s.handleHookOptions)
		s.mux.HandleFunc("GET /hooks/{id}", s.handleGetHook)
		s.mux.HandleFunc("PATCH /hooks/{id}", s.handleUpdateHook)
		s.mux.HandleFunc("DELETE /hooks/{id}", s.handleDeleteHook)
		s.mux.HandleFunc("POST /hooks/exec/{id}", s.handleExecHook)
	}

	// Credential routes (auth-store)
	s.modelCredentials.Mount(s.mux)

	// Snapshot routes (snapshot-store). Metadata + blob retrieval for
	// Edit/Write tool-call before/after diffs rendered by the UI.
	if s.snapshotStore != nil {
		s.mux.HandleFunc("GET /sessions/{id}/tools/{tool_use_id}/snapshots", s.handleGetSnapshots)
		s.mux.HandleFunc("GET /snapshots/blob/{sha}", s.handleGetSnapshotBlob)
	}

	// Conformance
	s.mux.HandleFunc("GET /conformance", s.handleConformanceGet)
	s.mux.HandleFunc("POST /conformance/run", s.handleConformanceRun)

	// Bridge prefs
	s.mux.HandleFunc("GET /bridge-prefs", s.handleBridgePrefs)
	s.mux.HandleFunc("PUT /bridge-prefs", s.handleBridgePrefs)

	// Admin housekeeping (called by scheduler cron)
	s.mux.HandleFunc("POST /admin/file-inactive", s.handleFileInactive)
	s.mux.HandleFunc("POST /admin/archive-old", s.handleArchiveOld)

	// Machine routes — host-level configuration. Instances bind to a
	// machine; the machine carries transport/SSH/runner details.
	if s.harnessStore != nil {
	}

	// Runner WebSocket — long-lived connection from llm-bridge-runner
	// daemons on remote machines. Auth is per-machine bearer token in
	// the Authorization header, validated against machines.runner_token_hash.
	// Runner enrollment — single-use passphrase exchanged for a
	// durable per-machine token. Mint passphrases via the
	// `llm-bridge mint-enroll` CLI subcommand.

	// Runner asset distribution — install script + prebuilt binaries.
	// The server hosts these so a freshly-cloned WSL/laptop can bootstrap
	// from a single curl command, without depending on a public
	// download infrastructure (GitHub Releases, etc.).

	// Seed-broadcast trigger. Anything that mutates a seed source's content
	// (the standalone agent-store on :8300 reached via dash, a CLI editing
	// a file directly, an out-of-band sync) can POST here to nudge every
	// connected runner to reconcile. The bridge-server's own embedded
	// agent-store already broadcasts via its hook callbacks; this is the
	// fallback for actors outside this process.
	s.mux.HandleFunc("POST /api/runner/seed/broadcast", s.handleSeedBroadcast)

	// Seed sources for runners: agent-store's four seed routes
	// (registerAgentsRegistryRoutes) and skill-store's (registerSkillStoreRoutes)
	// are answered in process, checked by the runner's bearer token.

	// Harness backend proxy — service-style harnesses (inber, hermes…)
	// run their backend once on the bridge host. Wrappers on remote
	// runners hit /api/harness-proxy/{harness}/<rest> instead of
	// localhost:<backend-port>, eliminating the need to replicate
	// state and credentials on every machine.
}

// ServeHTTP authorizes every request before the mux dispatches it
// (request_authorization.go). There is no path around it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.authorizeAndServe(w, r)
}

// handleSeedBroadcast is the explicit "tell every runner to reconcile" trigger.
// Query params:
//
//	source — "agent-store" (default) or "skill-store"
//	reason — free-text label included in logs and the runner's snapshot event
func (s *Server) handleSeedBroadcast(w http.ResponseWriter, r *http.Request) {
	source := runnerprotocol.SeedSource(r.URL.Query().Get("source"))
	if source == "" {
		source = runnerprotocol.SeedSourceAgentStore
	}
	switch source {
	case runnerprotocol.SeedSourceAgentStore, runnerprotocol.SeedSourceSkillStore:
	default:
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "manual"
	}
	s.broadcastSeedSnapshot(source, reason)
	w.WriteHeader(http.StatusNoContent)
}

// authorizeRunnerRequest accepts only requests carrying a known runner's
// bearer token. The runner's existing per-machine token (validated against
// machines.runner_token_hash) is what gets sent.
func (s *Server) authorizeRunnerRequest(r *http.Request) bool {
	if s.harnessStore == nil {
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
		return false
	}
	token := auth[len(prefix):]
	if token == "" {
		return false
	}
	_, err := s.harnessStore.GetMachineByRunnerTokenHash(machines.HashRunnerToken(token))
	return err == nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// broadcastSeedSnapshot pokes every connected runner to do a full seed
// reconcile against the named source. Used after a UI save or scan: rather
// than computing per-runner deltas server-side, we ride on the runner's
// existing pull-based reconciler — the snapshot trigger is just the prompt.
//
// Send failures (closed conns, full outgoing buffers) are logged and
// swallowed; the runner will reconcile on its next periodic tick or
// reconnect anyway.
func (s *Server) broadcastSeedSnapshot(source runnerprotocol.SeedSource, reason string) {
	if s.harness == nil {
		return
	}
	conns := s.harness.Runners().List()
	if len(conns) == 0 {
		return
	}
	m := &runnerprotocol.RunnerMessage{
		Type: runnerprotocol.RunnerMsgSeedSnapshot,
		SeedSnapshot: &runnerprotocol.RunnerSeedSnapshot{
			Source: source,
			Reason: reason,
		},
	}
	for _, rc := range conns {
		if err := rc.Send(m); err != nil {
			log.Printf("[seed] broadcast %s/%s to %s: %v", source, reason, rc.Name(), err)
		}
	}
}

// ChatLinks is the chat link service: a session talking with a person
// through one of the bots multichat holds.
func (s *Server) ChatLinks() *chatlinks.Service { return s.chatLinkService }

// turnClassifierForBriefs is the signal classifier the brief pass rides on,
// read at each turn end so a classifier set later is the one used, and nil
// while there is none.
func (s *Server) turnClassifierForBriefs() briefs.TurnClassifier {
	if s.signalClassifier == nil {
		return nil
	}
	return s.signalClassifier
}

// Machines is the machine, instance and runner routes and the work behind
// them: machine stats, disk usage and runner sockets.
func (s *Server) Machines() *machineroutes.Service { return s.machineRoutes }

// OneShot is the one-shot and image generation path: the routed model calls
// every background feature makes, and the executors operations run with.
func (s *Server) OneShot() *oneshot.Service { return s.oneShot }

// Peers is the connected servers: their connections, sockets, messages and
// the calls made through them.
func (s *Server) Peers() *peers.Service { return s.peers }
