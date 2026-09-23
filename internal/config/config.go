package config

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

type Config struct {
	// Settings is the declared settings this Config was read from: what GET
	// /settings describes, and where the stored behaviour settings are read
	// at the time of use. Nil in a Config built as a literal, which is how
	// tests build one; server.New then builds a registry with no environment
	// and seeds the stored settings from the literal's own fields.
	Settings   *servicesettings.Registry
	ListenAddr string
	DBPath     string
	// OperationsDBPath is the operations database (internal/operationstore).
	OperationsDBPath string
	// OperationsWorkerCount and OperationsLeaseDuration bound the operations
	// coordinator; see internal/operations.
	OperationsWorkerCount   int
	OperationsLeaseDuration time.Duration
	AgentStoreDB            string
	MemoryStoreDB           string
	HarnessStoreDB          string
	HookStoreDB             string
	ModelStoreDB            string
	ModelStoreURL           string
	AgentStoreURL           string
	ImagesDir               string
	BridgePrefsPath         string
	ConformancePath         string
	LogStoreURL             string
	// PublicURL is the externally-reachable bridge URL that runners use
	// to fetch backend binaries listed in HarnessService.BinaryURL. Empty
	// → manifests fall back to the runner's own server_url, which works
	// when the runner is reaching the bridge over a tunnel on
	// localhost:port (the WSL-via-SSH-tunnel case).
	PublicURL    string
	ToolStoreURL string
	// PermissionStoreURL is the base URL of the permission-store service
	// consulted by the PreToolUse permission-prehook handler. Defaults to
	// localhost:8304.
	PermissionStoreURL string
	// GrantStoreURL is the base URL of grant-store, read at spawn for the
	// effective grants of a session's principal — which tools it may be
	// offered — and proxied at /grant-store/ as the calling principal.
	// Required, via LLMBRIDGE_GRANT_STORE_URL.
	GrantStoreURL string
	// PrincipalStoreURL is the base URL of principal-store: asked at session
	// creation whether a principal_id names a real principal, and asked on
	// every gated request whether the caller is active and an administrator.
	// Required, via LLMBRIDGE_PRINCIPAL_STORE_URL.
	PrincipalStoreURL string
	// BundleStoreURL is the base URL of bundle-store, asked at session
	// creation whether a bundle_id names a real bundle and at spawn what it
	// resolves to. Configured via LLMBRIDGE_BUNDLE_STORE_URL.
	BundleStoreURL string
	// KanbanStoreURL is the base URL of the kanban-store service, which owns
	// the session↔noteboard-todo link a signal propagates to, and which
	// /kanban/ proxies to as the calling principal. Required, via
	// LLMBRIDGE_KANBAN_STORE_URL.
	KanbanStoreURL string
	// MailstackURL is the base URL of mailstack, asked for the sender and
	// subject of the mail behind a card when question triage drafts a reply
	// to the customer. Configured via LLMBRIDGE_MAILSTACK_URL. MailstackToken
	// is its bearer token, via LLMBRIDGE_MAILSTACK_TOKEN; mailstack answers
	// 401 without one, so an empty token switches the lookup off and every
	// draft is minted with an empty To rather than a guessed address.
	MailstackURL   string
	MailstackToken string
	// HealthcheckURL is the base URL of healthcheck, whose /api/status is the
	// list of services the Services page shows and the only source of their
	// up/down state. Configured via LLMBRIDGE_HEALTHCHECK_URL.
	HealthcheckURL   string
	SnapshotStoreDB  string
	SnapshotStoreGit string
	// PurposeFolders maps CreateSessionRequest.Purpose values to the folder a
	// newly created session should be auto-filed into. Defaults come from the
	// purpose registry (msg.KnownPurposes); LLMBRIDGE_PURPOSE_FOLDERS overlays
	// them (format: "purpose:folder,purpose:folder"). Any purpose not in the
	// map results in no auto-filing.
	PurposeFolders map[string]string
	// PTYRingBufferBytes is the per-session ring buffer size (in bytes)
	// of recent pty output. Late attachers receive a replay of this
	// buffer on connect so xterm.js can paint the current screen state
	// without a full clear-and-redraw. Configured via
	// LLMBRIDGE_PTY_RING_BUFFER_BYTES; defaults to 65536 (64 KiB).
	PTYRingBufferBytes int
	// IdleTimeout is how long an events-mode session may sit with no new
	// events — stream output OR telemetry, both land in the events table —
	// before the watchdog kills its harness process and marks it aborted.
	// Reaping reclaims the ~150MB a warm claude subprocess holds while it
	// waits on stdin for a follow-up turn that one-shot autoworkers never
	// send. Configured via LLMBRIDGE_IDLE_TIMEOUT (Go duration, e.g.
	// "15m"); <=0 disables reaping for events-mode sessions.
	IdleTimeout time.Duration
	// PTYIdleTimeout is the same cutoff for pty-mode (interactive)
	// sessions. PTY session state is not derived from telemetry (it stays
	// "running"), so the activity timestamp is the sole liveness signal —
	// and a human reading output between prompts emits nothing — so this
	// defaults much higher than IdleTimeout. Configured via
	// LLMBRIDGE_PTY_IDLE_TIMEOUT; <=0 disables reaping for pty sessions.
	PTYIdleTimeout time.Duration
	// SignalClassifierModel is the cheap model the turn-end signal
	// classifier calls to sort a finished turn into question |
	// notification | neither. Configured via
	// LLMBRIDGE_SIGNAL_CLASSIFIER_MODEL; empty turns the classifier off
	// everywhere, leaving the looksLikeQuestion heuristic as the only
	// awaiting_user signal and minting no derived signals.
	SignalClassifierModel string
	// SignalClassifierOptOut is the set of harnesses the classifier skips
	// — the per-harness escape hatch the on-by-default decision was taken
	// with. Configured via LLMBRIDGE_SIGNAL_CLASSIFIER_OPT_OUT as a
	// comma-separated list of harness names.
	SignalClassifierOptOut map[msg.Harness]bool
	// SignalClassifierTimeout bounds one classify call. On timeout the turn
	// keeps whatever state the heuristic gave it and no signal is written.
	// Configured via LLMBRIDGE_SIGNAL_CLASSIFIER_TIMEOUT.
	SignalClassifierTimeout time.Duration
	// SignalClassifierMaxChars caps how much of a turn's final text is sent
	// to the classifier. A turn ending in a huge dump is still classified
	// from its tail, which is where a question or a sign-off lives.
	// Configured via LLMBRIDGE_SIGNAL_CLASSIFIER_MAX_CHARS.
	SignalClassifierMaxChars int
	// SignalClassifierInstance is the harness instance the classifier runs its
	// call on. It must be a claude_code instance with no bound credential —
	// that is what puts the call on the Claude Code subscription login instead
	// of on an API key. Configured via LLMBRIDGE_SIGNAL_CLASSIFIER_INSTANCE.
	SignalClassifierInstance string
	// PromptDriftTaggerInstance is the harness instance that labels the
	// sections a prompt-file edit adds (one single-shot call per held drift),
	// and PromptDriftTaggerModel the model it asks for. An empty instance
	// turns labelling off: drifts are still detected and held, unlabelled.
	PromptDriftTaggerInstance string
	PromptDriftTaggerModel    string
	// DemoLoginSigningKey is the HMAC-SHA256 key that signs the demo login
	// cookie and the session agent tokens, from
	// LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY. Required, and at least
	// DemoLoginSigningKeyMinimumBytes long: without it nothing can be signed
	// and no caller can be identified.
	DemoLoginSigningKey string
	// ServiceToken is the credential an internal service presents in
	// X-LLM-Bridge-Service-Token to call this server unrestricted, from
	// LLMBRIDGE_SERVICE_TOKEN. Required, and at least ServiceTokenMinimumBytes
	// long: every route is gated, and an internal service reaches the operator
	// routes only by presenting it.
	ServiceToken string
	// GrantStoreServiceToken is sent to grant-store as
	// X-Grant-Store-Service-Token on every call this server makes as itself
	// (the spawn-time effective-grants read), from
	// GRANT_STORE_SERVICE_TOKEN. Optional: empty sends no token,
	// which a grant-store that enforces per-principal access answers with 401.
	GrantStoreServiceToken string
}

// Names of the environment variables that hold this server's own secrets.
// They are declared once here because two things read them: Load, and
// SecretEnvironmentVariableNames, which the child-process environment builder
// (internal/childprocessenv) strips from every process this server spawns.
const (
	DemoLoginSigningKeyEnvironmentVariable = "LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY"
	ServiceTokenEnvironmentVariable        = "LLMBRIDGE_SERVICE_TOKEN"
	// The two store tokens carry the store's own name for them, with no
	// LLMBRIDGE_ prefix: grant-store, kanban-store, dash and the scheduler all
	// read GRANT_STORE_SERVICE_TOKEN and KANBAN_STORE_SERVICE_TOKEN from one
	// host-local file (~/.config/principal-gating-tokens.env), and this server's
	// unit loads the same file. Until 2026-09-18 these two constants named
	// LLMBRIDGE_-prefixed variables nothing sets, which failed twice over: the
	// grant token was read as empty, so grant-store answered every spawn-time
	// read with 401, and the real variables were not on the strip list below,
	// so every harness child inherited both tokens.
	GrantStoreServiceTokenEnvironmentVariable  = "GRANT_STORE_SERVICE_TOKEN"
	KanbanStoreServiceTokenEnvironmentVariable = kanbanclient.ServiceTokenEnvironmentVariable
)

// SecretEnvironmentVariableNames lists every environment variable that must
// never reach a process this server spawns. A harness child runs an agent that
// can execute shell commands; whatever is in its environment the agent can
// read. The demo login signing key would let it mint a login cookie for any
// principal, the service token would make it the unrestricted internal caller,
// and a store service token would let it bypass that store's per-principal
// enforcement. The kanban token is read by internal/kanbanclient rather than
// by Load, which is why its name is taken from that package: one spelling,
// read in one place and stripped in another.
func SecretEnvironmentVariableNames() []string {
	return []string{
		DemoLoginSigningKeyEnvironmentVariable,
		ServiceTokenEnvironmentVariable,
		GrantStoreServiceTokenEnvironmentVariable,
		KanbanStoreServiceTokenEnvironmentVariable,
	}
}

// DemoLoginSigningKeyMinimumBytes is the shortest signing key accepted: the
// size of an HMAC-SHA256 output, so the key is not the weaker half of the MAC.
const DemoLoginSigningKeyMinimumBytes = 32

// ServiceTokenMinimumBytes is the shortest LLMBRIDGE_SERVICE_TOKEN accepted.
const ServiceTokenMinimumBytes = 32

// ValidateRequestAuthorizationCredentials checks the two secrets this server
// cannot identify a caller without: the key that signs the login cookie and
// the session agent tokens, and the token an internal service presents. Every
// request is authorized before it reaches a handler, so a server that starts
// without either would gate nothing while looking as though it did.
//
// server.New calls this and refuses to build a server that fails it.
// ValidateRequestAuthorizationSettings is the whole startup check.
func (c *Config) ValidateRequestAuthorizationCredentials() error {
	if c.DemoLoginSigningKey == "" {
		return fmt.Errorf("%s is unset: it signs the login cookie and every session agent token, and every request to this server is authorized",
			DemoLoginSigningKeyEnvironmentVariable)
	}
	if len(c.DemoLoginSigningKey) < DemoLoginSigningKeyMinimumBytes {
		return fmt.Errorf("%s is %d bytes; it must be at least %d",
			DemoLoginSigningKeyEnvironmentVariable, len(c.DemoLoginSigningKey), DemoLoginSigningKeyMinimumBytes)
	}
	if c.ServiceToken == "" {
		return fmt.Errorf("%s is unset: every route is gated, and internal services reach the operator routes only by presenting it",
			ServiceTokenEnvironmentVariable)
	}
	if len(c.ServiceToken) < ServiceTokenMinimumBytes {
		return fmt.Errorf("%s is %d bytes; it must be at least %d",
			ServiceTokenEnvironmentVariable, len(c.ServiceToken), ServiceTokenMinimumBytes)
	}
	return nil
}

// ValidateRequestAuthorizationSettings is every setting request authorization
// needs: the two credentials above, and the three stores a gated request is
// answered from — principal-store, which says whether the caller is active and
// an administrator, and the two stores mounted behind the identity-carrying
// proxies. None of them has a default; a missing one is a startup error naming
// the variable, because guessing an address here would send a principal's
// boards to whatever answers on that port.
//
// main calls this and refuses to start on an error.
func (c *Config) ValidateRequestAuthorizationSettings() error {
	if err := c.ValidateRequestAuthorizationCredentials(); err != nil {
		return err
	}
	for _, required := range []struct {
		environmentVariable string
		value               string
		purpose             string
	}{
		{"LLMBRIDGE_PRINCIPAL_STORE_URL", c.PrincipalStoreURL, "the principal-store every caller's principal is read from"},
		{"LLMBRIDGE_KANBAN_STORE_URL", c.KanbanStoreURL, "the kanban-store /kanban/ forwards to"},
		{"LLMBRIDGE_GRANT_STORE_URL", c.GrantStoreURL, "the grant-store /grant-store/ forwards to"},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("%s is unset: it is %s, and it has no default", required.environmentVariable, required.purpose)
		}
	}
	return nil
}

// Load reads the process environment through the declared settings
// (SettingDefinitions), falling back to the addresses in
// internal/productiondefaults for anything unset.
//
// It stops the process on a setting that cannot be read: a value that does not
// parse as its type, or a set LLMBRIDGE_ variable that nothing declares. Both
// used to pass in silence — a malformed timeout became the default, and a
// misspelled variable did nothing while its author believed otherwise.
//
// The fallbacks are what make the ordinary case work with no configuration,
// and they are also what would let a test open the live databases and write to
// the live event log. Load therefore ends by asking productiondefaults whether
// any of them survived, and panics if one did inside a `go test` binary. The
// check is inert in a real gateway process.
func Load() *Config {
	cfg, err := LoadFrom(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	return cfg
}

// LoadFrom is Load over a given environment.
func LoadFrom(environment servicesettings.Environment) (*Config, error) {
	settings, err := NewSettingsRegistry(environment)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Settings:                  settings,
		ListenAddr:                settings.String("listen_address"),
		DBPath:                    settings.String("database.path"),
		OperationsDBPath:          settings.String("operations.database_path"),
		OperationsWorkerCount:     settings.Integer("operations.worker_count"),
		OperationsLeaseDuration:   settings.Duration("operations.lease_duration"),
		AgentStoreDB:              settings.String("agent_store.database_path"),
		MemoryStoreDB:             settings.String("memory_store.database_path"),
		HarnessStoreDB:            settings.String("harness_store.database_path"),
		HookStoreDB:               settings.String("hook_store.database_path"),
		ModelStoreDB:              settings.String("model_store.database_path"),
		ModelStoreURL:             settings.String("model_store.url"),
		AgentStoreURL:             settings.String("agent_store.url"),
		ImagesDir:                 settings.String("images.directory"),
		BridgePrefsPath:           settings.String("bridge_preferences.path"),
		ConformancePath:           settings.String("conformance.path"),
		LogStoreURL:               settings.String("log_store.url"),
		PublicURL:                 settings.String("public_url"),
		ToolStoreURL:              settings.String("tool_store.url"),
		PermissionStoreURL:        settings.String("permission_store.url"),
		GrantStoreURL:             settings.String("grant_store.url"),
		PrincipalStoreURL:         settings.String("principal_store.url"),
		BundleStoreURL:            settings.String("bundle_store.url"),
		KanbanStoreURL:            settings.String("kanban_store.url"),
		MailstackURL:              settings.String("mailstack.url"),
		MailstackToken:            settings.String("mailstack.token"),
		HealthcheckURL:            settings.String("healthcheck.url"),
		SnapshotStoreDB:           settings.String("snapshot_store.database_path"),
		SnapshotStoreGit:          settings.String("snapshot_store.git_path"),
		PurposeFolders:            purposeFoldersOver(settings.StringMap("session.purpose_folders")),
		PTYRingBufferBytes:        settings.Integer("pty.ring_buffer_bytes"),
		IdleTimeout:               settings.Duration(SettingSessionIdleTimeout),
		PTYIdleTimeout:            settings.Duration(SettingSessionPTYIdleTimeout),
		SignalClassifierModel:     settings.String(SettingSignalClassifierModel),
		SignalClassifierOptOut:    harnessSetOf(settings.StringList(SettingSignalClassifierOptOutHarnesses)),
		SignalClassifierInstance:  settings.String(SettingSignalClassifierInstance),
		PromptDriftTaggerInstance: settings.String(SettingPromptDriftTaggerInstance),
		PromptDriftTaggerModel:    settings.String(SettingPromptDriftTaggerModel),
		SignalClassifierTimeout:   settings.Duration(SettingSignalClassifierTimeout),
		SignalClassifierMaxChars:  settings.Integer(SettingSignalClassifierMaximumCharacters),
		DemoLoginSigningKey:       settings.String("demo_login.signing_key"),
		ServiceToken:              settings.String("service_token"),
		GrantStoreServiceToken:    settings.String("grant_store.service_token"),
	}
	productiondefaults.PanicIfUsedUnderTest(cfg.GuardedAddresses())
	return cfg, nil
}

// GuardedAddresses reports the value this config holds for every field
// internal/productiondefaults knows a production address for, keyed by field
// name. Callers hand it to productiondefaults.PanicIfUsedUnderTest.
//
// A Config built as a literal — which is how every test in this repo builds
// one — never passes through Load, so this is also the way such a test can opt
// itself into the same check.
func (c *Config) GuardedAddresses() map[string]string {
	return map[string]string{
		"ListenAddr":         c.ListenAddr,
		"DBPath":             c.DBPath,
		"AgentStoreDB":       c.AgentStoreDB,
		"MemoryStoreDB":      c.MemoryStoreDB,
		"HarnessStoreDB":     c.HarnessStoreDB,
		"HookStoreDB":        c.HookStoreDB,
		"ModelStoreDB":       c.ModelStoreDB,
		"BridgePrefsPath":    c.BridgePrefsPath,
		"ConformancePath":    c.ConformancePath,
		"LogStoreURL":        c.LogStoreURL,
		"ToolStoreURL":       c.ToolStoreURL,
		"PermissionStoreURL": c.PermissionStoreURL,
		"MailstackURL":       c.MailstackURL,
		"HealthcheckURL":     c.HealthcheckURL,
		"SnapshotStoreDB":    c.SnapshotStoreDB,
		"SnapshotStoreGit":   c.SnapshotStoreGit,
		"OperationsDBPath":   c.OperationsDBPath,
	}
}

// harnessSetOf turns harness ids into a set. Names are passed through
// unchanged, so a name that matches no harness simply excludes nothing.
func harnessSetOf(names []string) map[msg.Harness]bool {
	out := make(map[msg.Harness]bool, len(names))
	for _, name := range names {
		out[msg.Harness(name)] = true
	}
	return out
}

// purposeFolderDefaults returns the purpose→folder map built from the purpose
// registry in llm-bridge/msg.
//
// This used to be a hardcoded default string listing eight purposes, which is
// how it drifted: `scheduled-task`, `dispatcher` and `herald` were all live
// purposes that never appeared in it, so those sessions silently never filed,
// while its `healthcheck` key mapped a purpose no caller ever sent. Two places
// held the vocabulary and only one of them was maintained.
//
// Reading it off the registry means adding a purpose files it by construction.
func purposeFolderDefaults() map[string]string {
	out := make(map[string]string)
	for _, p := range msg.KnownPurposes() {
		if p.Folder != "" {
			out[p.Name] = p.Folder
		}
	}
	return out
}

// purposeFoldersOver lays the operator's purpose:folder pairs over the
// registry defaults. A malformed pair never reaches here: the settings
// registry refuses LLMBRIDGE_PURPOSE_FOLDERS at startup, where the old parser
// skipped the pair and filed those sessions nowhere.
func purposeFoldersOver(pairs map[string]string) map[string]string {
	out := purposeFolderDefaults()
	for purpose, folder := range pairs {
		out[purpose] = folder
	}
	return out
}
