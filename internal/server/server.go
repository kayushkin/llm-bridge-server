package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	agentstore "github.com/kayushkin/agent-store"
	harnessstore "github.com/kayushkin/harness-store"
	hookstore "github.com/kayushkin/hook-store"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/permclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	memorystore "github.com/kayushkin/memory-store"
	modelstore "github.com/kayushkin/model-store"
	snapshotstore "github.com/kayushkin/snapshot-store"
)

// autoResumeWindow caps how recently a session must have been active (by
// LastActivityAt) for startup reconciliation to auto-restart its harness.
// Sessions older than this are left at idle for the user to resume manually.
// Tuned for Claude Code: turns regularly go quiet for tens of minutes (long
// thinking, user reading, etc.) — anything shorter drops still-active sessions
// across deploys.
const autoResumeWindow = 2 * time.Hour

// watchdogInterval is how often startWatchdog scans for `running` sessions
// whose harness process has died without emitting a terminal event (crash,
// OOM, harness binary panic). Each tick reconciles via autoResume.
const watchdogInterval = 60 * time.Second

type Server struct {
	mux           *http.ServeMux
	store         *store.Store
	agentStore    *agentstore.Store
	memoryStore   *memorystore.Store
	harnessStore  *harnessstore.Store
	hookStore     *hookstore.Store
	modelStore    *modelstore.Store
	snapshotStore *snapshotstore.Store
	harness       *harness.Manager
	authClient    *authstoreclient.Client
	permClient    *permclient.Client
	bridgePrefs   *bridgePrefsStore
	cfState       *conformanceState
	sessionHub    *sessionHub
	parkedAsks    *parkedAsks
	cfg           *config.Config
}

func New(st *store.Store, as *agentstore.Store, ms *memorystore.Store, hs *harnessstore.Store, hks *hookstore.Store, mds *modelstore.Store, ss *snapshotstore.Store, cfg *config.Config) *Server {
	authClient := authstoreclient.New("", "", "llm-bridge-server")
	hub := newSessionHub(st)
	st.SetNotifier(hub)
	srv := &Server{
		mux:           http.NewServeMux(),
		store:         st,
		agentStore:    as,
		memoryStore:   ms,
		harnessStore:  hs,
		hookStore:     hks,
		modelStore:    mds,
		snapshotStore: ss,
		harness:       harness.NewManager(st, cfg.LogStoreURL, cfg.PublicURL, publicBaseURL(cfg.ListenAddr), cfg.PTYRingBufferBytes, authClient),
		authClient:    authClient,
		permClient:    permclient.New(cfg.PermissionStoreURL),
		bridgePrefs:   newBridgePrefsStore(cfg.BridgePrefsPath),
		cfState:       newConformanceState(cfg.ConformancePath),
		sessionHub:    hub,
		parkedAsks:    newParkedAsks(),
		cfg:           cfg,
	}
	srv.routes()
	srv.syncHarnessTypes()
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
		if err := s.harnessStore.UpsertHarnessType(&harnessstore.HarnessType{
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
	s.mux.HandleFunc("GET /harnesses", s.handleHarnesses)
	s.mux.HandleFunc("GET /harnesses/{name}/capabilities", s.handleHarnessCapabilities)
	s.mux.HandleFunc("GET /harnesses/{name}/agents", s.handleHarnessAgents)

	// Static harness images
	s.mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(s.cfg.ImagesDir))))

	// Session routes
	s.mux.HandleFunc("GET /sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /session-events", s.handleSessionListEvents)
	s.mux.HandleFunc("GET /sessions/search", s.handleSearchSessions)
	s.mux.HandleFunc("GET /sessions/discover", s.handleDiscoverSessions)
	s.mux.HandleFunc("POST /sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("POST /sessions/{id}/send", s.handleSendMessage)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.handleSessionEvents)
	// Pty-mode session attach. Bidirectional WebSocket bound to the
	// session's pseudoterminal fd; rejected for sessions started in
	// events mode. Single-writer in v1 (child 2); resize / multi-reader
	// land in child 3.
	s.mux.HandleFunc("GET /sessions/{id}/attach", s.handleAttachSession)
	s.mux.HandleFunc("GET /sessions/{id}/attach-token", s.handleGetAttachToken)
	s.mux.HandleFunc("GET /sessions/aggregates", s.handleSessionAggregates)
	s.mux.HandleFunc("GET /sessions/{id}/messages", s.proxyToLogStore)
	s.mux.HandleFunc("GET /sessions/{id}/history", s.proxyToLogStore)
	s.mux.HandleFunc("POST /sessions/{id}/interrupt", s.handleInterruptSession)
	s.mux.HandleFunc("POST /sessions/{id}/resume", s.handleResumeSession)
	s.mux.HandleFunc("POST /sessions/{id}/stop", s.handleStopSession)
	s.mux.HandleFunc("POST /sessions/{id}/mode", s.handleSwitchMode)
	s.mux.HandleFunc("POST /sessions/{id}/compact", s.handleCompactSession)
	s.mux.HandleFunc("POST /sessions/{id}/fork", s.handleForkSession)
	s.mux.HandleFunc("POST /sessions/{id}/rename", s.handleRenameSession)
	s.mux.HandleFunc("POST /sessions/{id}/auto-rename", s.handleAutoRenameSession)
	s.mux.HandleFunc("POST /sessions/{id}/config", s.handleConfigSession)
	s.mux.HandleFunc("PUT /sessions/{id}/folder", s.handleSetSessionFolder)
	s.mux.HandleFunc("POST /sessions/{id}/mark-done", s.handleMarkSessionDone)
	s.mux.HandleFunc("GET /sessions/{id}/git/repos", s.handleSessionGitRepos)
	s.mux.HandleFunc("GET /sessions/{id}/git", s.handleSessionGit)

	// Hook resolution — surface awaiting_resolution HookEvents and accept
	// a decision back. Used by bridge-ui to render permission prompts and
	// human-resolved hooks. Pending list lets a freshly-connected client
	// recover banner state without replaying the full event stream.
	s.mux.HandleFunc("GET /sessions/{id}/hooks/pending", s.handleListPendingHooks)
	s.mux.HandleFunc("POST /sessions/{id}/hooks/{request_id}/resolve", s.handleResolveHook)

	// PreToolUse permission gate for Claude Code. Wired into every CC
	// session via buildClaudeCodeSettings's --settings injection so CC
	// posts here on every tool call. Sole permission gate now that the
	// embedded bridge_perm MCP is gone.
	s.mux.HandleFunc("POST /permission/cc-prehook/{bridge_id}", s.handleCCPermissionPrehook)

	// PreToolUse permission gate for codex. Wired in via
	// buildCodexHookConfig — codex's hook config carries a curl command
	// pointing here on every Bash/apply_patch/MCP tool call. Codex's
	// PreToolUse payload + hookSpecificOutput.permissionDecision shapes
	// are byte-identical to CC's, so the same handler serves both URLs.
	s.mux.HandleFunc("POST /permission/codex-prehook/{bridge_id}", s.handleCCPermissionPrehook)

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
	s.mux.HandleFunc("PUT /source-folders/{source}", s.handlePutSourceFolder)
	s.mux.HandleFunc("DELETE /source-folders/{source}", s.handleDeleteSourceFolder)

	// Agent-store routes (mounted from agent-store library). The hook
	// callbacks let agent-store nudge connected runners to reconcile when
	// the canonical context files change.
	if s.agentStore != nil {
		agentstore.RegisterHandlersWithHooks(
			s.mux,
			s.agentStore,
			func(f *agentstore.TrackedFile, v *agentstore.TrackedFileVersion) {
				s.broadcastSeedSnapshot(msg.SeedSourceAgentStore, "save")
			},
			func(_ *agentstore.ScanResult) {
				s.broadcastSeedSnapshot(msg.SeedSourceAgentStore, "scan")
			},
		)
	}

	// Memory-store routes (mounted from memory-store library)
	if s.memoryStore != nil {
		memorystore.RegisterHandlers(s.mux, s.memoryStore)
	}

	// Instance routes only available if harness-store is loaded
	if s.harnessStore != nil {
		s.mux.HandleFunc("GET /instances", s.handleListInstances)
		s.mux.HandleFunc("POST /instances", s.handleCreateInstance)
		s.mux.HandleFunc("GET /instances/{id}", s.handleGetInstance)
		s.mux.HandleFunc("PUT /instances/{id}", s.handleUpdateInstance)
		s.mux.HandleFunc("DELETE /instances/{id}", s.handleDeleteInstance)
		s.mux.HandleFunc("GET /instances/{id}/status", s.handleInstanceStatus)
		s.mux.HandleFunc("GET /instances/{id}/sessions", s.handleListInstanceSessions)
		s.mux.HandleFunc("GET /instances/{id}/credentials", s.handleListInstanceCredentials)
		s.mux.HandleFunc("POST /instances/{id}/credentials", s.handleBindCredential)
		s.mux.HandleFunc("DELETE /instances/{id}/credentials/{cred_id}", s.handleUnbindCredential)
		s.mux.HandleFunc("POST /instances/{id}/oneshot", s.handleInstanceOneShot)
	}

	// Hook registry routes (mounted only when hook-store is loaded).
	// /hooks/exec/{id} is always registered because the hook-store is
	// where registered hooks are resolved; without the store, exec 404s.
	if s.hookStore != nil {
		s.mux.HandleFunc("POST /hooks", s.handleCreateHook)
		s.mux.HandleFunc("GET /hooks", s.handleListHooks)
		s.mux.HandleFunc("GET /hooks/{id}", s.handleGetHook)
		s.mux.HandleFunc("PATCH /hooks/{id}", s.handleUpdateHook)
		s.mux.HandleFunc("DELETE /hooks/{id}", s.handleDeleteHook)
		s.mux.HandleFunc("POST /hooks/exec/{id}", s.handleExecHook)
	}

	// Credential routes (auth-store)
	s.mux.HandleFunc("GET /credentials", s.handleCredentialsList)
	s.mux.HandleFunc("POST /credentials", s.handleCredentialCreate)
	s.mux.HandleFunc("DELETE /credentials/{id}", s.handleCredentialDelete)

	// Model routes (model-store)
	if s.modelStore != nil {
		s.mux.HandleFunc("GET /models", s.handleModels)
	}

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
		s.mux.HandleFunc("GET /machines", s.handleListMachines)
		s.mux.HandleFunc("POST /machines", s.handleCreateMachine)
		s.mux.HandleFunc("GET /machines/{id}", s.handleGetMachine)
		s.mux.HandleFunc("PUT /machines/{id}", s.handleUpdateMachine)
		s.mux.HandleFunc("DELETE /machines/{id}", s.handleDeleteMachine)
	}

	// Runner WebSocket — long-lived connection from llm-bridge-runner
	// daemons on remote machines. Auth is per-machine bearer token in
	// the Authorization header, validated against machines.runner_token_hash.
	s.mux.HandleFunc("GET /api/runner/ws", s.handleRunnerWS)
	// Runner enrollment — single-use passphrase exchanged for a
	// durable per-machine token. Mint passphrases via the
	// `llm-bridge mint-enroll` CLI subcommand.
	s.mux.HandleFunc("POST /api/runner/enroll", s.handleEnrollRunner)

	// Runner asset distribution — install script + prebuilt binaries.
	// The server hosts these so a freshly-cloned WSL/laptop can bootstrap
	// from a single curl command, without depending on a public
	// download infrastructure (GitHub Releases, etc.).
	s.mux.HandleFunc("GET /api/runner/install.sh", s.handleRunnerInstallScript)
	s.mux.HandleFunc("GET /api/runner/binary", s.handleRunnerBinary)

	// Seed-broadcast trigger. Anything that mutates a seed source's content
	// (the standalone agent-store on :8300 reached via dash, a CLI editing
	// a file directly, an out-of-band sync) can POST here to nudge every
	// connected runner to reconcile. The bridge-server's own embedded
	// agent-store already broadcasts via its hook callbacks; this is the
	// fallback for actors outside this process.
	s.mux.HandleFunc("POST /api/runner/seed/broadcast", s.handleSeedBroadcast)

	// Seed source proxies. Runners hit bridge-server with their bearer
	// token; bridge-server forwards to the standalone agent-store/skill-store
	// services. This way the runner has a single base URL and a single
	// auth credential for all bridge-side data.
	s.mux.Handle("/api/agent-store/", http.HandlerFunc(s.proxyAgentStore))
	s.mux.Handle("/api/skill-store/", http.HandlerFunc(s.proxySkillStore))

	// Harness backend proxy — service-style harnesses (inber, hermes…)
	// run their backend once on the bridge host. Wrappers on remote
	// runners hit /api/harness-proxy/{harness}/<rest> instead of
	// localhost:<backend-port>, eliminating the need to replicate
	// state and credentials on every machine.
	s.mux.HandleFunc("/api/harness-proxy/{harness}/{rest...}", s.handleHarnessProxy)
}

// localInstancesByHarness returns a {harness: instance_id} map of the
// first enabled local-transport instance for each requested harness type.
// Used by session discovery / auto-resume to land orphaned sessions on
// the local-host instance when one exists.
func (s *Server) localInstancesByHarness(types []msg.Harness) map[msg.Harness]string {
	out := make(map[msg.Harness]string)
	if s.harnessStore == nil {
		return out
	}
	for _, h := range types {
		instances, err := s.harnessStore.ListInstancesByHarness(h)
		if err != nil {
			continue
		}
		for _, inst := range instances {
			if !inst.Enabled {
				continue
			}
			m, err := s.harnessStore.GetMachine(inst.MachineID)
			if err != nil || m.Transport != msg.TransportLocal {
				continue
			}
			out[h] = inst.ID
			break
		}
	}
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Compress compressible responses when the client accepts gzip. The
	// wrapper decides per-response from the Content-Type and passes SSE and
	// hijacked WebSocket connections through untouched (see gzipResponseWriter).
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		w = gw
	}
	s.mux.ServeHTTP(w, r)
}

// handleSearchSessions proxies /sessions/search to log-store's /api/v1/sessions/search.
func (s *Server) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	target := fmt.Sprintf("%s/api/v1/sessions/search", s.cfg.LogStoreURL)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := http.Get(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleSessionAggregates proxies /sessions/aggregates to log-store. Kept
// separate from proxyToLogStore because that handler infers the log-store
// path from the URL's {id}/{endpoint} shape, which doesn't apply here.
func (s *Server) handleSessionAggregates(w http.ResponseWriter, r *http.Request) {
	target := s.cfg.LogStoreURL + "/api/v1/sessions/aggregates"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := http.Get(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyToLogStore proxies /sessions/{id}/messages and /sessions/{id}/history to log-store.
func (s *Server) proxyToLogStore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	endpoint := path.Base(r.URL.Path) // "messages" or "history"
	target := fmt.Sprintf("%s/api/v1/sessions/%s/%s", s.cfg.LogStoreURL, id, endpoint)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	resp, err := http.Get(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ReconcileAndResume clears stale 'running' state left over from the previous
// server lifetime and auto-restarts the harness for sessions whose last
// activity was inside autoResumeWindow. Sessions that went quiet before the
// window are left idle — the user can resume them on demand.
func (s *Server) ReconcileAndResume() {
	sessions, err := s.store.ReconcileSessions(msg.ActiveSessionStates()...)
	if err != nil {
		log.Printf("[reconcile] %v", err)
		return
	}
	if len(sessions) == 0 {
		return
	}
	log.Printf("[reconcile] reset %d stale running→idle", len(sessions))

	if s.harnessStore == nil {
		return
	}
	cutoff := time.Now().Add(-autoResumeWindow)
	var resumed int
	for i := range sessions {
		sess := sessions[i]
		lastAt, err := s.store.LastActivityAt(sess.SessionID)
		if err != nil {
			log.Printf("[reconcile] %s: last-activity lookup failed: %v", sess.SessionID, err)
			continue
		}
		if lastAt.Before(cutoff) {
			continue
		}
		resumed++
		go s.autoResume(sess)
	}
	if resumed > 0 {
		log.Printf("[reconcile] auto-resuming %d sessions active within %s", resumed, autoResumeWindow)
	}
}

// StartWatchdog launches a goroutine that periodically scans for sessions
// the database still marks `running` but for which no harness subprocess is
// registered with the manager. That gap opens when a harness crashes (OOM,
// panic, network drop on SSH-runner) without emitting a terminal `result` or
// `error` event — readEvents removes the process from the manager map, but
// state stays `running` because no terminal event arrived. Without this
// watchdog the session sits hung until the user touches it.
//
// Startup-only ReconcileAndResume covers the cross-restart case; this loop
// covers in-lifetime crashes.
func (s *Server) StartWatchdog() {
	go func() {
		ticker := time.NewTicker(watchdogInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.watchdogTick()
			s.reapIdleTick()
		}
	}()
}

func (s *Server) watchdogTick() {
	if s.harnessStore == nil {
		return
	}
	for _, st := range msg.ActiveSessionStates() {
		sessions, err := s.store.ListSessionsByState(string(st))
		if err != nil {
			log.Printf("[watchdog] list %s sessions: %v", st, err)
			continue
		}
		for i := range sessions {
			sess := sessions[i]
			if s.harness.HasProcess(sess.SessionID) {
				continue
			}
			log.Printf("[watchdog] %s: state=%s but no harness process; resuming", sess.SessionID, st)
			// Drop back to idle so autoResume's startOnInstance path takes a
			// clean state transition. Skip the auto-resume on failure to flip
			// state — leaving it active would just refire next tick.
			if err := s.store.UpdateSessionState(sess.SessionID, string(msg.SessionIdle)); err != nil {
				log.Printf("[watchdog] %s: state reset failed: %v", sess.SessionID, err)
				continue
			}
			go s.autoResume(sess)
		}
	}
}

// reapIdleTick kills harness processes that have gone quiet past their
// idle timeout and marks the session aborted. This is the source-level
// fix for the warm-process leak: after a turn completes the claude
// subprocess lingers (on stdin in events mode, on the pty fd in pty mode)
// holding ~150MB while it waits for a follow-up turn that one-shot
// autoworkers never send. Left unchecked these accumulate until OOM.
//
// "Quiet" is measured by the most recent event of ANY kind. Both stream
// output and OTel telemetry land in the events table — pty telemetry via
// /sidecar/event → BroadcastEvent → StoreEvent — so a single timestamp
// (LastActivityAt) covers events mode and pty mode alike.
func (s *Server) reapIdleTick() {
	if s.harness == nil {
		return
	}
	now := time.Now()
	for _, id := range s.harness.ListActiveSessions() {
		sess, err := s.store.GetSession(id)
		if err != nil || sess == nil {
			continue
		}

		// Last activity = most recent event of any kind. Zero means no
		// events yet (just spawned); the decision falls back to the row's
		// updated_at so a fresh process isn't reaped before its first event.
		lastAt, err := s.store.LastActivityAt(id)
		if err != nil {
			log.Printf("[reaper] %s: last-activity lookup failed: %v", id, err)
			continue
		}

		idle, reap := reapDecision(now, sess.Mode, msg.SessionType(sess.Type), msg.SessionState(sess.State), lastAt, sess.UpdatedAt, s.cfg.UnattendedIdleTimeout, s.cfg.AttendedIdleTimeout)
		if !reap {
			continue
		}

		log.Printf("[reaper] %s: idle %s (mode=%s type=%s state=%s) exceeds timeout; killing", id, idle.Round(time.Second), sess.Mode, sess.Type, sess.State)
		// Kill may fail if the process already exited — flip state anyway so
		// the row reflects reality and the watchdog won't try to resume it.
		_ = s.harness.Kill(id)
		if err := s.store.UpdateSessionState(id, string(msg.SessionAborted)); err != nil {
			log.Printf("[reaper] %s: state transition failed: %v", id, err)
		}
	}
}

// reapDecision is the pure idle-reaper policy, split out so the gating
// rules are unit-testable without a live harness. Returns how long the
// session has been idle and whether it should be killed.
//
// Rules:
//   - A session parked on a human (awaiting_user, awaiting_permission) is
//     NEVER reaped, at any age, in any mode. See msg.SessionState.
//     IsBlockedOnUser: that silence is a person deliberating, not abandoned
//     work. Reaping awaiting_permission would auto-deny a live question the
//     user never got to answer; reaping awaiting_user would discard the warm
//     process their reply was going to land in.
//   - Pick the timeout by who is watching, not by I/O contract: attended
//     sessions (pty, or events-mode SessionTypeInteractive like the dash
//     chat) get the longer AttendedIdleTimeout, because a human reading or
//     thinking emits no events. Everything unattended — autoworker fires,
//     healthcheck agents, the renamer, subagents — gets the short one.
//   - timeout <= 0 disables reaping for that class.
//   - An unattended session in an active state (model_generating, tool_running,
//     compacting, rate_limited, running, starting) is never reaped — BUT only
//     if it has actually emitted an event. A long tool call goes quiet while it
//     runs, so for it, state and not the timestamp is the guard. A session that
//     has emitted NOTHING, ever, has no such claim on us: its state row is
//     uncorroborated. The server stamps a session `running` the moment the
//     process spawns, so a session created and then never sent a prompt — a
//     caller that crashed, or healthcheck's alert path before it was fixed —
//     sits in `running` with zero events forever, and a purely state-gated
//     reaper skips it for good. That orphan holds ~300MB, and it is exactly
//     what a resource alert produces when the box is already in trouble.
//     Requiring corroboration costs a genuine long tool call nothing: it has
//     the turn's own events (the prompt, model_generating, the tool call)
//     behind it long before it goes quiet.
//   - Idle is measured from the last event, falling back to updatedAt when no
//     event has landed so freshly-spawned processes still get a grace gap.
func reapDecision(now time.Time, mode msg.SessionMode, sessionType msg.SessionType, state msg.SessionState, lastActivity, updatedAt time.Time, unattendedTimeout, attendedTimeout time.Duration) (time.Duration, bool) {
	if state.IsBlockedOnUser() {
		return 0, false
	}

	attended := mode == msg.SessionModePTY || sessionType == msg.SessionTypeInteractive
	timeout := unattendedTimeout
	if attended {
		timeout = attendedTimeout
	}
	if timeout <= 0 {
		return 0, false
	}
	hasEmittedAnEvent := !lastActivity.IsZero()
	if !attended && state.IsActive() && hasEmittedAnEvent {
		return 0, false
	}
	ref := lastActivity
	if !hasEmittedAnEvent {
		ref = updatedAt
	}
	idle := now.Sub(ref)
	return idle, idle >= timeout
}

// autoResume restarts a single session's harness process. Mirrors the flow in
// handleResumeSession minus the HTTP plumbing. If the previous turn was
// killed mid-flight (a user_message with no following result), the message
// text is re-sent once the harness is ready so the turn actually completes
// instead of sitting idle waiting for new input.
func (s *Server) autoResume(sess store.Session) {
	if sess.InstanceID == "" {
		// Every code path that creates a session populates instance_id (see
		// handleCreateSession; resolveInstance fails the request otherwise).
		// Discovered sessions can land with instance_id="" if no local instance
		// is enabled for their harness — those should never reach `running`,
		// so getting here means a real invariant break we want to see.
		log.Printf("[auto-resume] %s: ERROR instance_id empty — session cannot be resumed; skipping", sess.SessionID)
		return
	}
	inst, err := s.harnessStore.GetInstance(sess.InstanceID)
	if err != nil {
		log.Printf("[auto-resume] %s: instance %s not found: %v", sess.SessionID, sess.InstanceID, err)
		return
	}
	credID := resolveCredential(s.harnessStore, inst.ID)
	if _, err := s.startOnInstance(context.Background(), &sess, inst, credID); err != nil {
		log.Printf("[auto-resume] %s: start failed: %v", sess.SessionID, err)
		return
	}
	log.Printf("[auto-resume] %s: resumed", sess.SessionID)

	text, pending, err := s.harness.PendingTurnMessage(sess.SessionID)
	if err != nil {
		log.Printf("[auto-resume] %s: pending-turn check failed: %v", sess.SessionID, err)
		return
	}
	if !pending {
		return
	}
	// Harness subprocess needs a moment to finish its start handshake before
	// it will accept a message on stdin. 2s is enough for Claude Code's
	// resume-load; shorter risks writing before the pipe is being drained.
	time.Sleep(2 * time.Second)
	if err := s.harness.Send(sess.SessionID, text, nil); err != nil {
		log.Printf("[auto-resume] %s: replay send failed: %v", sess.SessionID, err)
		return
	}
	log.Printf("[auto-resume] %s: replayed interrupted turn", sess.SessionID)
}

// AutoDiscover runs session discovery for all harness types and imports them to the store.
// Called on startup to populate the session list with on-disk sessions.
func (s *Server) AutoDiscover() {
	go func() {
		ctx := context.Background()
		sessions, err := s.harness.DiscoverSessions(ctx, "")
		if err != nil {
			log.Printf("auto-discover: %v", err)
			return
		}

		// Build map of harness type → local instance ID.
		// Discovery runs the harness binary locally, so sessions belong to the local instance.
		localInstances := s.localInstancesByHarness([]msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex})

		var imported int
		for _, ds := range sessions {
			// Use prompt as display name - it's more useful for identifying sessions
			displayName := ds.Prompt
			if displayName == "" {
				displayName = ds.Project
			}
			if len(displayName) > 100 {
				displayName = displayName[:100]
			}

			instanceID := localInstances[ds.Harness]
			// Prefer the adapter's structural source tag (e.g. claudecode marks
			// Task()-spawned subagents from the on-disk layout) over our prompt-
			// prefix heuristic. Fall back to prefix inference only when the
			// adapter has no structural signal.
			source, folder := ds.Source, s.folderForSource(ds.Source)
			if source == "" {
				source, folder = s.discoverySourceFolder(ds.Prompt)
			}
			bridgeID, inserted, err := s.store.UpsertDiscoveredSession(ds.HarnessSessionID, ds.BridgeSessionID, displayName, string(ds.Harness), instanceID, source, folder, ds.CreatedAt, ds.UpdatedAt)
			if err == nil && inserted {
				imported++
				// Import history to log-store for new sessions
				go func(h msg.Harness, brID, sid string) {
					n, err := s.harness.ImportHistory(context.Background(), brID, h, sid)
					if err != nil {
						log.Printf("[auto-discover] failed to import history for %s: %v", sid, err)
					} else if n > 0 {
						log.Printf("[auto-discover] imported %d events for session %s", n, sid)
					}
				}(ds.Harness, bridgeID, ds.HarnessSessionID)
			}
		}
		if imported > 0 {
			log.Printf("[auto-discover] imported %d sessions", imported)
		}
	}()
}

// handleSeedBroadcast is the explicit "tell every runner to reconcile" trigger.
// Query params:
//
//	source — "agent-store" (default) or "skill-store"
//	reason — free-text label included in logs and the runner's snapshot event
func (s *Server) handleSeedBroadcast(w http.ResponseWriter, r *http.Request) {
	source := msg.SeedSource(r.URL.Query().Get("source"))
	if source == "" {
		source = msg.SeedSourceAgentStore
	}
	switch source {
	case msg.SeedSourceAgentStore, msg.SeedSourceSkillStore:
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

// proxyAgentStore and proxySkillStore forward seed-related GET/POSTs from
// runners to the standalone services on the bridge host. Runner auth
// (bearer token vs. machine row) is validated by checking the Authorization
// header against harness-store; if it doesn't match a known machine the
// request is rejected. Cookies/sessions are not forwarded — only seed
// endpoints accept this path.
func (s *Server) proxyAgentStore(w http.ResponseWriter, r *http.Request) {
	s.proxyToStore(w, r, "/api/agent-store", envOrDefault("AGENT_STORE_URL", "http://localhost:8300"))
}

func (s *Server) proxySkillStore(w http.ResponseWriter, r *http.Request) {
	s.proxyToStore(w, r, "/api/skill-store", envOrDefault("SKILL_STORE_URL", "http://localhost:8301"))
}

func (s *Server) proxyToStore(w http.ResponseWriter, r *http.Request, prefix, target string) {
	if !s.authorizeRunnerRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest := path.Clean("/" + r.URL.Path[len(prefix):])
	target = target + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[seed-proxy] %s: %v", target, err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	_, err := s.harnessStore.GetMachineByRunnerTokenHash(harnessstore.HashRunnerToken(token))
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
func (s *Server) broadcastSeedSnapshot(source msg.SeedSource, reason string) {
	if s.harness == nil {
		return
	}
	conns := s.harness.Runners().List()
	if len(conns) == 0 {
		return
	}
	m := &msg.RunnerMessage{
		Type: msg.RunnerMsgSeedSnapshot,
		SeedSnapshot: &msg.RunnerSeedSnapshot{
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
