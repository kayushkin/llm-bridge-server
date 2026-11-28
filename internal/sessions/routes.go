package sessions

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/httproutes"
)

// Mount registers the session core's routes on the server's mux. The hook
// registry routes need hook-store, and the snapshot routes snapshot-store;
// each is left out without its store. touchingKeepAwake wraps the routes that
// start a turn, so the machine stays awake while sessions run here.
func (s *Service) Mount(mux httproutes.Mux, touchingKeepAwake func(http.HandlerFunc) http.HandlerFunc) {
	s.PermissionGate.Mount(mux)
	// log-store's own routes.
	mux.HandleFunc("/session-log/", s.serveLogStoreMount)
	mux.HandleFunc("GET /ui/", s.HandleUserInterface)
	mux.HandleFunc("GET /{$}", s.HandleRootRedirectsToUserInterface)
	mux.HandleFunc("GET /sessions", s.HandleListSessions)
	mux.HandleFunc("GET /session-events", s.HandleSessionListEvents)
	mux.HandleFunc("GET /sessions/summary", s.HandleSessionsSummary)
	mux.HandleFunc("GET /sessions/principals", s.HandleSessionPrincipals)
	mux.HandleFunc("POST /sessions/summary", s.HandleSessionsSummaryLookup)
	mux.HandleFunc("GET /sessions/recent-bundle", s.HandleRecentBundle)
	mux.HandleFunc("GET /sessions/validators", s.HandleSessionsValidators)
	mux.HandleFunc("POST /sessions/validators", s.HandleSessionsValidatorsLookup)
	mux.HandleFunc("GET /sessions/search", s.HandleSearchSessions)
	mux.HandleFunc("GET /sessions/discover", s.HandleDiscoverSessions)
	mux.HandleFunc("POST /sessions", touchingKeepAwake(s.HandleCreateSession))
	mux.HandleFunc("GET /sessions/{id}", s.HandleGetSession)
	mux.HandleFunc("POST /sessions/{id}/send", touchingKeepAwake(s.HandleSendMessage))
	mux.HandleFunc("GET /sessions/{id}/files-named-by-tools", s.HandleFilesNamedByTools)
	mux.HandleFunc("GET /sessions/{id}/files-named-by-tools/content", s.HandleFileNamedByToolsContent)
	mux.HandleFunc("GET /sessions/{id}/files-named-by-tools/image", s.HandleImageNamedByTools)
	mux.HandleFunc("GET /sessions/{id}/events", s.HandleSessionEvents)
	mux.HandleFunc("GET /sessions/{id}/attach", s.HandleAttachSession)
	mux.HandleFunc("GET /sessions/{id}/attach-token", s.HandleGetAttachToken)
	mux.HandleFunc("GET /sessions/aggregates", s.HandleSessionAggregates)
	mux.HandleFunc("GET /sessions/{id}/messages", s.HandleSessionMessages)
	mux.HandleFunc("GET /sessions/{id}/messages/raw", s.HandleSessionMessages)
	mux.HandleFunc("GET /sessions/{id}/entries/{eventId}", s.ProxyToLogStore)
	mux.HandleFunc("GET /sessions/{id}/entries/{eventId}/images/{index}", s.ProxyToLogStore)
	mux.HandleFunc("POST /sessions/{id}/interrupt", s.HandleInterruptSession)
	mux.HandleFunc("POST /sessions/{id}/resume", s.HandleResumeSession)
	mux.HandleFunc("POST /sessions/{id}/stop", s.HandleStopSession)
	mux.HandleFunc("POST /sessions/{id}/mode", s.HandleSwitchMode)
	mux.HandleFunc("POST /sessions/{id}/compact", s.HandleCompactSession)
	mux.HandleFunc("POST /sessions/{id}/fork", s.HandleForkSession)
	mux.HandleFunc("POST /sessions/{id}/rename", s.HandleRenameSession)
	mux.HandleFunc("POST /sessions/{id}/config", s.HandleConfigSession)
	mux.HandleFunc("PUT /sessions/{id}/folder", s.HandleSetSessionFolder)
	mux.HandleFunc("POST /sessions/{id}/mark-done", s.HandleMarkSessionDone)
	mux.HandleFunc("GET /sessions/{id}/git/repos", s.HandleSessionGitRepos)
	mux.HandleFunc("GET /sessions/{id}/git", s.HandleSessionGit)
	mux.HandleFunc("GET /sessions/{id}/effective-config", s.HandleSessionEffectiveConfig)
	mux.HandleFunc("GET /effective-config", s.HandleDryRunEffectiveConfig)
	mux.HandleFunc("GET /services", s.HandleListServices)
	mux.HandleFunc("GET /services/databases/schema", s.HandleDatabaseSchema)
	mux.HandleFunc("GET /services/databases/rows", s.HandleDatabaseRows)
	mux.HandleFunc("GET /folders", s.HandleListFolders)
	mux.HandleFunc("POST /folders", s.HandleCreateFolder)
	mux.HandleFunc("DELETE /folders/{name}", s.HandleDeleteFolder)
	mux.HandleFunc("PUT /folders/{name}", s.HandleRenameFolder)
	mux.HandleFunc("GET /source-folders", s.HandleListSourceFolders)
	mux.HandleFunc("GET /session-taxonomy", s.HandleSessionTaxonomy)
	mux.HandleFunc("GET /session-taxonomy/report", s.HandleSessionTaxonomyReport)
	mux.HandleFunc("PUT /source-folders/{source}", s.HandlePutSourceFolder)
	mux.HandleFunc("DELETE /source-folders/{source}", s.HandleDeleteSourceFolder)
	if s.SnapshotStore() != nil {
		mux.HandleFunc("GET /sessions/{id}/tools/{tool_use_id}/snapshots", s.HandleGetSnapshots)
		mux.HandleFunc("GET /snapshots/blob/{sha}", s.HandleGetSnapshotBlob)
	}
	mux.HandleFunc("GET /bridge-prefs", s.HandleBridgePrefs)
	mux.HandleFunc("PUT /bridge-prefs", s.HandleBridgePrefs)
	mux.HandleFunc("POST /admin/file-inactive", s.HandleFileInactive)
	mux.HandleFunc("POST /admin/archive-old", s.HandleArchiveOld)
}
