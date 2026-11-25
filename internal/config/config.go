package config

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

type Config struct {
	// Settings is the declared settings this Config was read from: what GET
	// /settings describes, and where the stored behavior settings are read
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
	// The stored one-shot and operations settings, as the environment seeded
	// them. Read them at the time of use from Settings, never from here.
	OneShotInstanceByProvider     map[string]string
	OperationsCompletionModelRole string
	// SessionActionsReviewModelRole seeds session_actions.review_model_role.
	SessionActionsReviewModelRole string
	// ImageGenerationInstanceID seeds image_generation.instance_id.
	ImageGenerationInstanceID string
	// SessionRenamerModelRole seeds session_renamer.model_role; empty in a
	// Config literal switches automatic titles off.
	SessionRenamerModelRole    string
	OperationsGrantEnforcement string
	// ServerName seeds server.name; KeepAwakeEnabled and
	// KeepAwakeGraceMinutes seed keep_awake.enabled and
	// keep_awake.grace_minutes. Each is seeded only when set.
	ServerName string
	// ChatLinksReferencePageBaseURL seeds chat_links.reference_page_base_url.
	ChatLinksReferencePageBaseURL string
	// SelfUpdateCommand and OrchestratorURL seed self_update.command and
	// orchestrator.url; both are read from the settings record when used.
	SelfUpdateCommand     string
	OrchestratorURL       string
	KeepAwakeEnabled      bool
	KeepAwakeGraceMinutes int
	// SessionHostHoldSessions and SessionHostHoldMinutes seed
	// session_host.hold_sessions_without_server and
	// session_host.hold_without_server_minutes, each only when set.
	SessionHostHoldSessions bool
	SessionHostHoldMinutes  int
	// PeerAttachmentMaximumBytes and PeerAttachmentsMaximumTotalBytes seed
	// the two limits on files in messages between servers, when set.
	PeerAttachmentMaximumBytes       int
	PeerAttachmentsMaximumTotalBytes int
	// FileStoreMaximumFileBytes and FileStoreInlineContentTypes seed
	// file_store.maximum_file_bytes and file_store.inline_content_types, the
	// first start each key exists; the stored row decides after.
	FileStoreMaximumFileBytes   int
	FileStoreInlineContentTypes []string
	HarnessStoreDB              string
	HookStoreDB                 string
	ImagesDir                   string
	// SessionFilesDir is where a file shared into a session is copied for its
	// agent to read: <dir>/<session id>/<file id>/<filename>.
	SessionFilesDir string
	// SchedulerURL is the scheduler's base URL, which runs a scheduler job
	// action. Empty refuses those actions.
	SchedulerURL    string
	BridgePrefsPath string
	ConformancePath string
	// PublicURL is the externally-reachable bridge URL that runners use
	// to fetch backend binaries listed in HarnessService.BinaryURL. Empty
	// → manifests fall back to the runner's own server_url, which works
	// when the runner is reaching the bridge over a tunnel on
	// localhost:port (the WSL-via-SSH-tunnel case).
	PublicURL string
	// DiscoveryPrincipalID explicitly owns local imports; empty leaves them unowned.
	DiscoveryPrincipalID string
	// SkillStoreDataDirectory holds the files of every skill the skill-store
	// module records (skills/<id>/) and the sources it clones (repos/<name>/);
	// the rows are in Postgres. SkillStoreInstallDirectory is where an
	// installed skill is copied. LLMBRIDGE_SKILL_STORE_DATA_DIRECTORY and
	// LLMBRIDGE_SKILL_STORE_INSTALL_DIRECTORY.
	SkillStoreDataDirectory    string
	SkillStoreInstallDirectory string
	// LocalToolsPinchtabURL, LocalToolsPinchtabToken, LocalToolsBraveAPIKey
	// and LocalToolsSchedulerToken are what tool-store's in-process browser,
	// web_search and scheduler tools are built with; the scheduler tool calls
	// SchedulerURL.
	LocalToolsPinchtabURL    string
	LocalToolsPinchtabToken  string
	LocalToolsBraveAPIKey    string
	LocalToolsSchedulerToken string
	// PostgresURL is the Postgres the modules keep their records in, as the
	// login that provisions them. Empty switches those modules off, and their
	// routes answer 503. LLMBRIDGE_POSTGRES_URL.
	PostgresURL string
	// ChatLinksNATSURL and MultichatURL switch chat links on: NATS carries
	// what people send our accounts, multichat sends the replies. Either
	// empty turns them off. Configured via LLMBRIDGE_NATS_URL and
	// LLMBRIDGE_MULTICHAT_URL. sign-in-store sends sign-in links through the
	// same multichat.
	ChatLinksNATSURL string
	MultichatURL     string
	// PeopleStoreURL is people-store, which says who owns the account a chat
	// link message came from, and which person owns a handle typed on the
	// sign-in page. Chat links need it too.
	PeopleStoreURL string
	// AuthStoreURL and AuthStoreToken reach auth-store, which sign-in-store
	// reads multichat's token and Discord's client secret from.
	AuthStoreURL   string
	AuthStoreToken string
	// Sign-in-store's wiring (sign_in.*). SignInPageURL and
	// SignInCookieDomain empty switch its routes off.
	SignInPageURL                         string
	SignInCookieDomain                    string
	SignInMultichatCredentialID           string
	SignInDiscordApplicationID            string
	SignInDiscordClientSecretCredentialID string
	SignInDiscordRedirectURL              string
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
	// ExternalActivityPollInterval seeds external_activity.poll_interval:
	// how often the server looks for session files a terminal run changed.
	ExternalActivityPollInterval time.Duration
	// SignalClassifierModelRole is the model-store role the turn-end signal
	// classifier asks to sort a finished turn into question | notification |
	// neither. Empty turns the classifier off everywhere, leaving the
	// looksLikeQuestion heuristic as the only awaiting_user signal and
	// minting no derived signals.
	SignalClassifierModelRole string
	// QuestionTriageModelRole is the model-store role question triage asks.
	// Empty turns triage off, and a worker's question reaches its card
	// untriaged.
	QuestionTriageModelRole string
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
	// PromptDriftTaggerModelRole is the model-store role that labels the
	// sections a prompt-file edit adds (one single-shot call per held drift).
	// Empty turns labeling off: drifts are still detected and held,
	// unlabeled.
	PromptDriftTaggerModelRole string
	// DemoLoginSigningKey is the HMAC-SHA256 key that signs the session agent
	// tokens and session posting tokens a harness child is given, from
	// LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY. The variable keeps its old name: until
	// 2026-10-19 it also signed the login cookie, which is now an opaque token
	// with a row in login_sessions. Required, and at least
	// DemoLoginSigningKeyMinimumBytes long: without it no agent can be
	// identified.
	DemoLoginSigningKey string
	// DemoLoginEnabled is whether POST /auth/demo-login signs anyone in, from
	// LLMBRIDGE_DEMO_LOGIN_ENABLED (default true). A deployment whose people
	// sign in through Entra ID sets it false, and the route then answers 404.
	DemoLoginEnabled bool
	// Entra ID sign-in. All four of tenant, client id, client secret and
	// redirect URL set switch it on; none set leaves it off; some set is a
	// startup error (ValidateEntraSignInSettings). EntraAuthorityURL is the
	// login host, https://login.microsoftonline.com unless a test points it
	// at a fake provider.
	EntraTenantID     string
	EntraClientID     string
	EntraClientSecret string
	EntraRedirectURL  string
	EntraAuthorityURL string
	// ServiceToken is the credential an internal service presents in
	// X-LLM-Bridge-Service-Token to call this server unrestricted, from
	// LLMBRIDGE_SERVICE_TOKEN. Required, and at least ServiceTokenMinimumBytes
	// long: every route is gated, and an internal service reaches the operator
	// routes only by presenting it.
	ServiceToken string
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
	//
	// grant-store and kanban-store now run in this process and this server
	// reads neither token, but the shared file still carries both for the
	// standalone stores and their callers until they move, and this unit still
	// loads that file. They stay on the strip list so no harness child
	// inherits them.
	GrantStoreServiceTokenEnvironmentVariable  = "GRANT_STORE_SERVICE_TOKEN"
	KanbanStoreServiceTokenEnvironmentVariable = "KANBAN_STORE_SERVICE_TOKEN"
	// reference-store's own name for the token it takes from an internal
	// service, in the same shared file. Not read: reference-store runs in
	// this server. The file still sets it for the standalone reference-store
	// until that stops, and whoever holds it looks any record up there for
	// nobody, so it stays on the strip list.
	ReferenceStoreServiceTokenEnvironmentVariable = "REFERENCE_STORE_SERVICE_TOKEN"
	// file-store's token, in its own host-local file
	// (~/.config/file-store-tokens.env). Not read: file-store runs in this
	// server. A unit drop-in may still load the file for the standalone
	// file-store until that stops, and the token reads every file there, so
	// it stays on the strip list.
	FileStoreServiceTokenEnvironmentVariable = "FILE_STORE_SERVICE_TOKEN"
	// The Entra ID client secret proves to Microsoft that a token request
	// comes from this application; an agent holding it could redeem any
	// authorization code meant for this server.
	EntraClientSecretEnvironmentVariable = "LLMBRIDGE_ENTRA_CLIENT_SECRET"
	// The Postgres URL names the login the modules' records are kept under,
	// and may carry its password. An agent holding it could read and rewrite
	// every module's tables past every rule this server enforces.
	PostgresURLEnvironmentVariable = "LLMBRIDGE_POSTGRES_URL"
	// The credentials tool-store's in-process tools present to PinchTab, Brave
	// Search and the scheduler. They are this server's, not a harness's.
	LocalToolsPinchtabTokenEnvironmentVariable  = "LLMBRIDGE_LOCAL_TOOLS_PINCHTAB_TOKEN"
	LocalToolsBraveAPIKeyEnvironmentVariable    = "LLMBRIDGE_LOCAL_TOOLS_BRAVE_API_KEY"
	LocalToolsSchedulerTokenEnvironmentVariable = "LLMBRIDGE_LOCAL_TOOLS_SCHEDULER_TOKEN"
)

// The two sign-in settings that switch sign-in-store's routes on, named here
// so the message that says they are off names them too.
const (
	SignInPageURLEnvironmentVariable      = "LLMBRIDGE_SIGN_IN_PAGE_URL"
	SignInCookieDomainEnvironmentVariable = "LLMBRIDGE_SIGN_IN_COOKIE_DOMAIN"
)

// SecretEnvironmentVariableNames lists every environment variable that must
// never reach a process this server spawns. A harness child runs an agent that
// can execute shell commands; whatever is in its environment the agent can
// read. The demo login signing key would let it mint a session agent token
// for any principal, the service token would make it the unrestricted
// internal caller, a store service token would let it bypass that store's
// per-principal enforcement, and the Entra client secret would let it redeem
// an authorization code meant for this server, and the Postgres URL would
// let it reach the modules' tables directly, and the local tools' credentials
// are this server's own.
func SecretEnvironmentVariableNames() []string {
	return []string{
		DemoLoginSigningKeyEnvironmentVariable,
		ServiceTokenEnvironmentVariable,
		GrantStoreServiceTokenEnvironmentVariable,
		KanbanStoreServiceTokenEnvironmentVariable,
		ReferenceStoreServiceTokenEnvironmentVariable,
		FileStoreServiceTokenEnvironmentVariable,
		EntraClientSecretEnvironmentVariable,
		PostgresURLEnvironmentVariable,
		LocalToolsPinchtabTokenEnvironmentVariable,
		LocalToolsBraveAPIKeyEnvironmentVariable,
		LocalToolsSchedulerTokenEnvironmentVariable,
	}
}

// DemoLoginSigningKeyMinimumBytes is the shortest signing key accepted: the
// size of an HMAC-SHA256 output, so the key is not the weaker half of the MAC.
const DemoLoginSigningKeyMinimumBytes = 32

// ServiceTokenMinimumBytes is the shortest LLMBRIDGE_SERVICE_TOKEN accepted.
const ServiceTokenMinimumBytes = 32

// ValidateRequestAuthorizationCredentials checks the two secrets this server
// cannot identify a caller without: the key that signs the session agent and
// posting tokens, and the token an internal service presents. Every request is
// authorized before it reaches a handler, so a server that starts without
// either would gate nothing while looking as though it did.
//
// server.New calls this and refuses to build a server that fails it.
// ValidateRequestAuthorizationSettings is the whole startup check.
func (c *Config) ValidateRequestAuthorizationCredentials() error {
	if c.DemoLoginSigningKey == "" {
		return fmt.Errorf("%s is unset: it signs every session agent token and session posting token, and every request to this server is authorized",
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
// needs: the two credentials above, the Postgres the access stores and
// kanban-store keep their records in, and reference-store, mounted behind the
// identity-carrying proxy. None of them has a default; a missing one is a
// startup error naming the variable, because guessing an address here would
// send a principal's records to whatever answers on that port.
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
		{PostgresURLEnvironmentVariable, c.PostgresURL, "the Postgres every caller's principal and grants are read from (access)"},
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
		Settings:                              settings,
		ListenAddr:                            settings.String("listen_address"),
		DBPath:                                settings.String("database.path"),
		OperationsDBPath:                      settings.String("operations.database_path"),
		OperationsWorkerCount:                 settings.Integer("operations.worker_count"),
		OperationsLeaseDuration:               settings.Duration("operations.lease_duration"),
		OneShotInstanceByProvider:             settings.StringMap(SettingOneShotInstanceByProvider),
		OperationsCompletionModelRole:         settings.ModelRole(SettingOperationsCompletionModelRole),
		SessionActionsReviewModelRole:         settings.ModelRole(SettingSessionActionsReviewModelRole),
		ImageGenerationInstanceID:             settings.String(SettingImageGenerationInstanceID),
		SessionRenamerModelRole:               settings.ModelRole(SettingSessionRenamerModelRole),
		OperationsGrantEnforcement:            settings.String(SettingOperationsGrantEnforcement),
		ServerName:                            settings.String(SettingServerName),
		ChatLinksReferencePageBaseURL:         settings.String(SettingChatLinksReferencePageBaseURL),
		SelfUpdateCommand:                     settings.String(SettingSelfUpdateCommand),
		OrchestratorURL:                       settings.String(SettingOrchestratorURL),
		KeepAwakeEnabled:                      settings.Boolean(SettingKeepAwakeEnabled),
		KeepAwakeGraceMinutes:                 settings.Integer(SettingKeepAwakeGraceMinutes),
		SessionHostHoldSessions:               settings.Boolean(SettingSessionHostHoldSessions),
		SessionHostHoldMinutes:                settings.Integer(SettingSessionHostHoldMinutes),
		HarnessStoreDB:                        settings.String("harness_store.database_path"),
		HookStoreDB:                           settings.String("hook_store.database_path"),
		ImagesDir:                             settings.String("images.directory"),
		SessionFilesDir:                       settings.String("session_files.directory"),
		SchedulerURL:                          settings.String("scheduler.url"),
		BridgePrefsPath:                       settings.String("bridge_preferences.path"),
		ConformancePath:                       settings.String("conformance.path"),
		PublicURL:                             settings.String("public_url"),
		AuthStoreURL:                          settings.String("auth_store.url"),
		AuthStoreToken:                        settings.String("auth_store.token"),
		SignInPageURL:                         settings.String("sign_in.page_url"),
		SignInCookieDomain:                    settings.String("sign_in.cookie_domain"),
		SignInMultichatCredentialID:           settings.String("sign_in.multichat_credential_id"),
		SignInDiscordApplicationID:            settings.String("sign_in.discord_application_id"),
		SignInDiscordClientSecretCredentialID: settings.String("sign_in.discord_client_secret_credential_id"),
		SignInDiscordRedirectURL:              settings.String("sign_in.discord_redirect_url"),
		DiscoveryPrincipalID:                  settings.String("discovery.principal_id"),
		SkillStoreDataDirectory:               settings.String("skill_store.data_directory"),
		SkillStoreInstallDirectory:            settings.String("skill_store.install_directory"),
		LocalToolsPinchtabURL:                 settings.String("local_tools.pinchtab_url"),
		LocalToolsPinchtabToken:               settings.String("local_tools.pinchtab_token"),
		LocalToolsBraveAPIKey:                 settings.String("local_tools.brave_api_key"),
		LocalToolsSchedulerToken:              settings.String("local_tools.scheduler_token"),
		PostgresURL:                           settings.String("database.postgres_url"),
		ChatLinksNATSURL:                      settings.String("chat_links.nats_url"),
		MultichatURL:                          settings.String("multichat.url"),
		PeopleStoreURL:                        settings.String("people_store.url"),
		MailstackURL:                          settings.String("mailstack.url"),
		MailstackToken:                        settings.String("mailstack.token"),
		HealthcheckURL:                        settings.String("healthcheck.url"),
		SnapshotStoreDB:                       settings.String("snapshot_store.database_path"),
		SnapshotStoreGit:                      settings.String("snapshot_store.git_path"),
		PurposeFolders:                        purposeFoldersOver(settings.StringMap("session.purpose_folders")),
		PTYRingBufferBytes:                    settings.Integer("pty.ring_buffer_bytes"),
		IdleTimeout:                           settings.Duration(SettingSessionIdleTimeout),
		PTYIdleTimeout:                        settings.Duration(SettingSessionPTYIdleTimeout),
		ExternalActivityPollInterval:          settings.Duration(SettingExternalActivityPollInterval),
		SignalClassifierModelRole:             settings.ModelRole(SettingSignalClassifierModelRole),
		QuestionTriageModelRole:               settings.ModelRole(SettingQuestionTriageModelRole),
		SignalClassifierOptOut:                harnessSetOf(settings.StringList(SettingSignalClassifierOptOutHarnesses)),
		PromptDriftTaggerModelRole:            settings.ModelRole(SettingPromptDriftTaggerModelRole),
		SignalClassifierTimeout:               settings.Duration(SettingSignalClassifierTimeout),
		SignalClassifierMaxChars:              settings.Integer(SettingSignalClassifierMaximumCharacters),
		DemoLoginSigningKey:                   settings.String("demo_login.signing_key"),
		DemoLoginEnabled:                      settings.Boolean("demo_login.enabled"),
		EntraTenantID:                         settings.String("entra.tenant_id"),
		EntraClientID:                         settings.String("entra.client_id"),
		EntraClientSecret:                     settings.String("entra.client_secret"),
		EntraRedirectURL:                      settings.String("entra.redirect_url"),
		EntraAuthorityURL:                     settings.String("entra.authority_url"),
		ServiceToken:                          settings.String("service_token"),
		PeerAttachmentMaximumBytes:            settings.Integer(SettingPeerAttachmentMaximumBytes),
		PeerAttachmentsMaximumTotalBytes:      settings.Integer(SettingPeerAttachmentsMaximumTotalBytes),
		FileStoreMaximumFileBytes:             settings.Integer(SettingFileStoreMaximumFileBytes),
		FileStoreInlineContentTypes:           settings.StringList(SettingFileStoreInlineContentTypes),
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
		"ListenAddr":       c.ListenAddr,
		"DBPath":           c.DBPath,
		"HarnessStoreDB":   c.HarnessStoreDB,
		"HookStoreDB":      c.HookStoreDB,
		"BridgePrefsPath":  c.BridgePrefsPath,
		"ConformancePath":  c.ConformancePath,
		"MailstackURL":     c.MailstackURL,
		"HealthcheckURL":   c.HealthcheckURL,
		"SnapshotStoreDB":  c.SnapshotStoreDB,
		"SnapshotStoreGit": c.SnapshotStoreGit,
		"OperationsDBPath": c.OperationsDBPath,
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
