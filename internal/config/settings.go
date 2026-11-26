package config

import (
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this server's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "llm-bridge-server"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// server's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the server from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
const OwnedEnvironmentVariablePrefix = "LLMBRIDGE_"

// Keys of the behavior settings the server stores and reads at the time of
// use. Everything else is read once, from the environment, into Config.
const (
	SettingSignalClassifierModelRole         = "signal_classifier.model_role"
	SettingSignalClassifierTimeout           = "signal_classifier.timeout"
	SettingSignalClassifierMaximumCharacters = "signal_classifier.maximum_characters"
	SettingSignalClassifierOptOutHarnesses   = "signal_classifier.opt_out_harnesses"
	SettingQuestionTriageModelRole           = "question_triage.model_role"
	SettingPromptDriftTaggerModelRole        = "prompt_drift_tagger.model_role"
	SettingSessionRenamerModelRole           = "session_renamer.model_role"
	SettingSessionIdleTimeout                = "session.idle_timeout"
	SettingSessionPTYIdleTimeout             = "session.pty_idle_timeout"
	SettingExternalActivityPollInterval      = "external_activity.poll_interval"
	SettingOneShotInstanceByProvider         = "oneshot.instance_by_provider"
	SettingOperationsCompletionModelRole     = "operations.completion_model_role"
	SettingOperationsGrantEnforcement        = "operations.grant_enforcement"
	SettingSessionActionsReviewModelRole     = "session_actions.review_model_role"
	SettingImageGenerationInstanceID         = "image_generation.instance_id"
	SettingServerName                        = "server.name"
	SettingChatLinksReferencePageBaseURL     = "chat_links.reference_page_base_url"
	SettingSelfUpdateCommand                 = "self_update.command"
	SettingOrchestratorURL                   = "orchestrator.url"
	SettingKeepAwakeEnabled                  = "keep_awake.enabled"
	SettingKeepAwakeGraceMinutes             = "keep_awake.grace_minutes"
	SettingSessionHostHoldSessions           = "session_host.hold_sessions_without_server"
	SettingSessionHostHoldMinutes            = "session_host.hold_without_server_minutes"
	SettingPeerAttachmentMaximumBytes        = "peer_messages.attachment_maximum_bytes"
	SettingPeerAttachmentsMaximumTotalBytes  = "peer_messages.attachments_maximum_total_bytes"
)

// ModelRoleSettings are the stored settings whose value is a model-store role:
// each is checked with model-store on write.
var ModelRoleSettings = []string{
	SettingSignalClassifierModelRole,
	SettingQuestionTriageModelRole,
	SettingPromptDriftTaggerModelRole,
	SettingSessionRenamerModelRole,
	SettingOperationsCompletionModelRole,
	SettingSessionActionsReviewModelRole,
}

// RetiredStoredSettingKeys are service_settings rows an earlier build wrote
// under keys no setting has now. The store deletes them at start, after
// copying the one that was renamed (see RenamedStoredSettingKeys).
var RetiredStoredSettingKeys = []string{
	"signal_classifier.model",
	"signal_classifier.instance",
	"prompt_drift_tagger.instance",
	"prompt_drift_tagger.model",
	"operations.completion_model",
	"operations.completion_instance",
	"operations.completion_instances",
	"session_actions.review_model",
}

// RenamedStoredSettingKeys maps an old stored key to the key that carries its
// value now. The value moves only when the new key holds no row yet.
var RenamedStoredSettingKeys = map[string]string{
	"operations.completion_instances": SettingOneShotInstanceByProvider,
}

// HarnessProxyEnvironmentVariable is the variable that names the backend the
// /harness/{name}/ proxy forwards to for one harness.
func HarnessProxyEnvironmentVariable(harnessName string) string {
	return "LLMBRIDGE_HARNESS_PROXY_" + strings.ToUpper(strings.ReplaceAll(harnessName, "-", "_"))
}

// harnessProxyDefaults are the backends the harness proxy reaches with nothing
// set: the two harnesses that are HTTP services on this host.
var harnessProxyDefaults = map[string]string{
	"inber":  "http://localhost:8200",
	"hermes": "http://localhost:8500",
}

// HarnessProxyDefault is the backend the proxy uses for harnessName when its
// variable is unset; empty means the harness has no proxy.
func HarnessProxyDefault(harnessName string) string {
	return harnessProxyDefaults[strings.ToLower(harnessName)]
}

// SettingDefinitions declares every environment variable this process reads,
// once. Load reads Config from it; GET /settings describes the server from it;
// and a test holds every os.Getenv in the repo to it, so a variable cannot be
// read without being declared here.
func SettingDefinitions() []servicesettings.Definition {
	const (
		wiring   = msg.ServiceSettingKindWiring
		path     = msg.ServiceSettingKindPath
		secret   = msg.ServiceSettingKindSecret
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
		role     = msg.ServiceSettingValueTypeModelRole
	)
	definitions := []servicesettings.Definition{
		// Behavior the server stores: changed on the settings page, no restart.
		{Key: SettingSignalClassifierModelRole, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_MODEL_ROLE", Kind: behavior, ValueType: role, Editable: true, Default: "balanced",
			Description: "The model-store role the turn-end signal classifier asks. Its models are tried in order, each on the instance oneshot.instance_by_provider names for its provider. Empty switches the classifier off: no derived signals."},
		{Key: SettingQuestionTriageModelRole, EnvironmentVariable: "LLMBRIDGE_QUESTION_TRIAGE_MODEL_ROLE", Kind: behavior, ValueType: role, Editable: true, Default: "balanced",
			Description: "The model-store role question triage asks, deciding whether a surfacing worker's question needs a person and who. Apart from signal_classifier.model_role since 2026-10-20, so the classifier could move to the model an eval chose for it while triage kept its own. Empty switches triage off: a worker's question reaches its card untriaged."},
		{Key: SettingSignalClassifierTimeout, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_TIMEOUT", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "20s",
			Description: "How long one classify or triage call may take. On timeout the turn keeps the state the heuristic gave it and no signal is written."},
		{Key: SettingSignalClassifierMaximumCharacters, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_MAX_CHARS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Editable: true, Default: "6000",
			Description: "How much of a turn's final text is sent to the classifier, counted from the end, where a question or a sign-off lives."},
		{Key: SettingSignalClassifierOptOutHarnesses, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_OPT_OUT", Kind: behavior, ValueType: msg.ServiceSettingValueTypeStringList, Editable: true,
			Description: "Harnesses whose turns the classifier skips, comma-separated by harness id."},
		{Key: SettingPromptDriftTaggerModelRole, EnvironmentVariable: "LLMBRIDGE_PROMPT_DRIFT_TAGGER_MODEL_ROLE", Kind: behavior, ValueType: role, Editable: true, Default: "balanced",
			Description: "The model-store role that proposes tags for sections a prompt-file edit adds, one call per held drift. Empty switches labeling off: drifts are still detected and held, unlabeled."},
		{Key: SettingSessionRenamerModelRole, EnvironmentVariable: "LLMBRIDGE_SESSION_RENAMER_MODEL_ROLE", Kind: behavior, ValueType: role, Editable: true, Default: "efficient",
			Description: "The model-store role that writes a session's sidebar title from its recent turns. Empty switches automatic titles off."},
		{Key: SettingSessionIdleTimeout, EnvironmentVariable: "LLMBRIDGE_IDLE_TIMEOUT", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "15m",
			Description: "How long an events-mode session may sit with no new event before its harness process is stopped and the session marked aborted. 0s or less switches reaping off."},
		{Key: SettingSessionPTYIdleTimeout, EnvironmentVariable: "LLMBRIDGE_PTY_IDLE_TIMEOUT", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "60m",
			Description: "The same cutoff for pty-mode sessions, where a person reading output emits nothing. 0s or less switches reaping off."},
		{Key: SettingExternalActivityPollInterval, EnvironmentVariable: "LLMBRIDGE_EXTERNAL_ACTIVITY_POLL_INTERVAL", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "15s",
			Description: "How often the server asks each harness wrapper which session files changed, so a conversation run with claude or codex in a terminal reaches the dash while the server is up. 0s or less switches the watcher off; sessions are then found only at startup."},

		{Key: SettingOneShotInstanceByProvider, EnvironmentVariable: "LLMBRIDGE_ONESHOT_INSTANCE_BY_PROVIDER", Kind: behavior, ValueType: msg.ServiceSettingValueTypeStringMap, Editable: true, Default: "anthropic:inst-cc-local,openai:inst-codex-local",
			Description: "Which harness instance makes every one-shot call this server routes — the classifier, triage, the drift tagger, the renamer, the command reviewer, operations and POST /oneshot — for each model-store provider, as provider:instance pairs. A model whose provider is named nowhere here is an attempt that fails, and the role's next model is tried. Each instance must be enabled and its harness must implement -oneshot."},
		{Key: SettingImageGenerationInstanceID, EnvironmentVariable: "LLMBRIDGE_IMAGE_GENERATION_INSTANCE_ID", Kind: behavior, ValueType: msg.ServiceSettingValueTypeString, Editable: true, Default: "inst-codex-local",
			Description: "The harness instance that makes images for POST /images and POST /sessions/{id}/images. Its harness must implement -generate-image (llm-bridge-codex does, on the Codex login). Empty switches image generation off."},
		{Key: SettingOperationsCompletionModelRole, EnvironmentVariable: "LLMBRIDGE_OPERATIONS_COMPLETION_MODEL_ROLE", Kind: behavior, ValueType: role, Editable: true,
			Description: "The model-store role an llm.completion or classification.run uses when its input names no model. Empty means an input must name one: a role, a model id or an alias."},
		{Key: SettingSessionActionsReviewModelRole, EnvironmentVariable: "LLMBRIDGE_SESSION_ACTIONS_REVIEW_MODEL_ROLE", Kind: behavior, ValueType: role, Editable: true, Default: "balanced",
			Description: "The model-store role that reviews a run_command button's command when an agent offers it, and whose verdict the button shows. A command it rejects cannot be run; a role none of whose models answers refuses the offer."},
		{Key: SettingOperationsGrantEnforcement, EnvironmentVariable: "LLMBRIDGE_OPERATIONS_GRANT_ENFORCEMENT", Kind: behavior, ValueType: text, Editable: true, Default: OperationsGrantEnforcementLenient,
			Description: "How grant-store's can_run_operation grants are applied to a principal starting an operation. lenient: a principal holding no such grant may start any type, and one holding any may start only those. strict: a principal may start only the types its grants name. Administrators and the service token are never checked."},
		{Key: SettingChatLinksReferencePageBaseURL, EnvironmentVariable: "LLMBRIDGE_CHAT_LINKS_REFERENCE_PAGE_BASE_URL", Kind: behavior, ValueType: text, Editable: true,
			Description: "The site a chat link's reply links a record to, such as https://dash.example: a reference id in the reply that the person may see becomes the record's name linked to its page there (reference-store's page_url, or /ref/<id> for a record with no page of its own). A record whose page is on another site keeps that site. Empty leaves every id that would link here as it was written."},
		{Key: SettingServerName, EnvironmentVariable: "LLMBRIDGE_SERVER_NAME", Kind: behavior, ValueType: text, Editable: true,
			Description: "What this server calls itself: shown at the top of its own session list, and reported to every server it is connected to, which shows it as this server's name. Empty means it has none, and is shown as such."},
		{Key: SettingSelfUpdateCommand, EnvironmentVariable: "LLMBRIDGE_SELF_UPDATE_COMMAND", Kind: behavior, ValueType: text, Editable: true,
			Description: "The shell command this server runs, detached, when an administrator approves an \"update ready\" notice from a connected server: it pulls, builds, tests and restarts whatever this host runs. It is given LLMBRIDGE_SELF_UPDATE_RESULT_FILE, where it writes {\"result\":\"succeeded\"|\"failed\",\"detail\":…,\"versions\":[{\"repo\",\"commit\"}]} as JSON, LLMBRIDGE_SELF_UPDATE_OPERATION_ID, and LLMBRIDGE_SELF_UPDATE_COMMITS, the notice's commits as JSON [{\"repo\",\"commit\"}]. It must outlive a restart of this server: under systemd with the default KillMode, start the work in a unit of its own (systemd-run). Empty means this server takes no updates from a notice."},
		{Key: SettingOrchestratorURL, EnvironmentVariable: "LLMBRIDGE_ORCHESTRATOR_URL", Kind: behavior, ValueType: text, Editable: true,
			Description: "Where the orchestrator answers. An administrator may hand an approved request from a connected server to it instead of starting a session on an instance; it is sent to the orchestrator's POST /home-session/messages. Empty means no handoff is offered."},
		{Key: SettingKeepAwakeEnabled, EnvironmentVariable: "LLMBRIDGE_KEEP_AWAKE_ENABLED", Kind: behavior, ValueType: msg.ServiceSettingValueTypeBoolean, Editable: true, Default: "false",
			Description: "Hold the machine awake while any session on this server is running, and for keep_awake.grace_minutes after the last one stops or was last sent a message. It cannot wake a machine that is already asleep, or stop a closed lid from putting it to sleep."},
		{Key: SettingPeerAttachmentMaximumBytes, EnvironmentVariable: "LLMBRIDGE_PEER_ATTACHMENT_MAXIMUM_BYTES", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Editable: true, Default: "52428800",
			Description: "The largest file, in bytes, a message to or from a connected server may carry: an upload here over it is refused, and so is a message from the other server that describes one, before any of it is fetched."},
		{Key: SettingPeerAttachmentsMaximumTotalBytes, EnvironmentVariable: "LLMBRIDGE_PEER_ATTACHMENTS_MAXIMUM_TOTAL_BYTES", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Editable: true, Default: "209715200",
			Description: "The most bytes all the files of one message to or from a connected server may come to together."},
		{Key: SettingKeepAwakeGraceMinutes, EnvironmentVariable: "LLMBRIDGE_KEEP_AWAKE_GRACE_MINUTES", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Editable: true, Default: "30",
			Description: "How many minutes the machine is kept awake after the last session stops running or was last sent a message, when keep_awake.enabled is on. 0 releases the hold as soon as no session runs."},
		{Key: SettingSessionHostHoldSessions, EnvironmentVariable: "LLMBRIDGE_SESSION_HOST_HOLD_SESSIONS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeBoolean, Editable: true, Default: "false",
			Description: "Ask runners to keep their harness processes running when their connection to this server drops, a restart of this server included, and to hand them back when it returns. Off, a runner ends every process when the connection drops. Leave it off while a harness's permission gate can fail open with this server away: a held process could then run a tool that no rule checked. A runner learns the setting when it next connects."},
		{Key: SettingSessionHostHoldMinutes, EnvironmentVariable: "LLMBRIDGE_SESSION_HOST_HOLD_MINUTES", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Editable: true, Default: "30",
			Description: "How many minutes a held harness process may go on working with this server away, when session_host.hold_sessions_without_server is on. After that the runner interrupts its turn and keeps the process."},

		// Behavior read once at start.
		{Key: "session.purpose_folders", EnvironmentVariable: "LLMBRIDGE_PURPOSE_FOLDERS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeStringMap,
			Description: "purpose:folder pairs laid over the purpose registry's own folders, deciding where a new session is filed. The Settings page's source folders override both at runtime."},
		{Key: "pty.ring_buffer_bytes", EnvironmentVariable: "LLMBRIDGE_PTY_RING_BUFFER_BYTES", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "65536",
			Description: "How many bytes of recent pty output each session keeps to replay to a terminal that attaches late."},
		{Key: "operations.worker_count", EnvironmentVariable: "LLMBRIDGE_OPERATIONS_WORKERS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "4",
			Description: "How many operations run at once. More wait in the queue."},
		{Key: "operations.lease_duration", EnvironmentVariable: "LLMBRIDGE_OPERATIONS_LEASE", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "30s",
			Description: "How long a worker's claim on a running operation lasts without renewal. An operation whose worker stops renewing is reconciled once it runs out."},
		{Key: "demo_login.enabled", EnvironmentVariable: "LLMBRIDGE_DEMO_LOGIN_ENABLED", Kind: behavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "true",
			Description: "Whether POST /auth/demo-login signs anyone in. It takes a principal id and no password, so a deployment whose people sign in through Entra ID sets this false, and the route answers 404 demo_login_disabled."},
		{Key: "tests.allow_production_addresses", EnvironmentVariable: productiondefaults.AllowInTestsEnvironmentVariable, Kind: behavior, ValueType: text,
			Description: "Set to anything, lets a go test binary use this host's live addresses. Never set on a running server."},

		// Wiring.
		{Key: "discovery.principal_id", EnvironmentVariable: "LLMBRIDGE_DISCOVERY_PRINCIPAL_ID", Kind: wiring, ValueType: text,
			Description: "Explicit owner of local discovered sessions. Empty leaves imports unowned. Existing owners are never replaced."},
		{Key: "listen_address", EnvironmentVariable: "LLMBRIDGE_LISTEN_ADDR", Kind: wiring, ValueType: text, Default: productiondefaults.ListenAddr,
			Description: "The address this server listens on."},
		{Key: "public_url", EnvironmentVariable: "LLMBRIDGE_PUBLIC_URL", Kind: wiring, ValueType: text,
			Description: "The URL remote runners fetch harness binaries from. Empty makes a runner use the address it reached the server on."},
		{Key: "peer_connections.public_url", EnvironmentVariable: "LLMBRIDGE_PEER_CONNECTIONS_PUBLIC_URL", Kind: wiring, ValueType: text,
			Description: "The https address other llm-bridge-servers reach this one at, shown beside a connection passphrase so the administrator can give both to the other server. Nothing else reads it; public_url is the address runners and this server's own agents use."},
		{Key: "chat_links.nats_url", EnvironmentVariable: "LLMBRIDGE_NATS_URL", Kind: wiring, ValueType: text,
			Description: "NATS, whose messaging.inbound subject multichat publishes what people send our accounts on. Chat links read it through a durable consumer. Empty switches chat links off, and their routes answer 503."},
		{Key: "multichat.url", EnvironmentVariable: "LLMBRIDGE_MULTICHAT_URL", Kind: wiring, ValueType: text,
			Description: "multichat. A chat link asks it which accounts exist and sends a turn's final reply through it, with the token auth-store holds for provider multichat; sign-in-store sends a sign-in link to a person's own chat through it, with the token in sign_in.multichat_credential_id. Empty switches chat links off, and their routes answer 503, and a link request cannot be sent."},
		{Key: "sign_in.page_url", EnvironmentVariable: SignInPageURLEnvironmentVariable, Kind: wiring, ValueType: text,
			Description: "The page a sign-in link opens: a link is this URL, \"#\" and the link's secret, and the page posts the secret to POST /sign-in-store/redeem. It names a site, so it has no default; empty switches sign-in-store's routes off, and they answer 503 sign_in_off."},
		{Key: "sign_in.cookie_domain", EnvironmentVariable: SignInCookieDomainEnvironmentVariable, Kind: wiring, ValueType: text,
			Description: "The domain the sign_in_session cookie is set for, which includes every name under it, so one sign-in reaches every site there. No default; empty switches sign-in-store's routes off."},
		{Key: "sign_in.multichat_credential_id", EnvironmentVariable: "LLMBRIDGE_SIGN_IN_MULTICHAT_CREDENTIAL_ID", Kind: wiring, ValueType: text,
			Description: "The auth-store credential holding multichat's bearer token, read each time sign-in-store sends a link or a message to a person's chat. Empty makes each such send fail with that reason."},
		{Key: "sign_in.discord_application_id", EnvironmentVariable: "LLMBRIDGE_SIGN_IN_DISCORD_APPLICATION_ID", Kind: wiring, ValueType: text,
			Description: "The Discord application Sign in with Discord uses. Any of the three sign_in.discord_ settings empty switches Sign in with Discord off, and its routes answer 503."},
		{Key: "sign_in.discord_client_secret_credential_id", EnvironmentVariable: "LLMBRIDGE_SIGN_IN_DISCORD_CLIENT_SECRET_CREDENTIAL_ID", Kind: wiring, ValueType: text,
			Description: "The auth-store credential holding that application's OAuth2 client secret, read each time a code is traded."},
		{Key: "sign_in.discord_redirect_url", EnvironmentVariable: "LLMBRIDGE_SIGN_IN_DISCORD_REDIRECT_URL", Kind: wiring, ValueType: text,
			Description: "Where Discord sends the browser back: GET /sign-in-store/discord/callback as the browser reaches it. It must be on the application's OAuth2 redirect list on Discord's developer portal, or Discord refuses every sign-in."},
		{Key: "local_tools.pinchtab_url", EnvironmentVariable: "LLMBRIDGE_LOCAL_TOOLS_PINCHTAB_URL", Kind: wiring, ValueType: text, Default: productiondefaults.PinchtabURL,
			Description: "Where PinchTab answers. tool-store's in-process browser tool sends every action there; a wrong address makes each browser call report a connection error."},
		{Key: "healthcheck.url", EnvironmentVariable: "LLMBRIDGE_HEALTHCHECK_URL", Kind: wiring, ValueType: text, Default: productiondefaults.HealthcheckURL,
			Description: "healthcheck, whose status is the list of services the service inventory shows."},
		{Key: "entra.tenant_id", EnvironmentVariable: "LLMBRIDGE_ENTRA_TENANT_ID", Kind: wiring, ValueType: text,
			Description: "The Microsoft Entra ID tenant people sign in from, as a GUID. A token from any other tenant is refused. Entra sign-in is on only when this, the client id, the client secret and the redirect URL are all set."},
		{Key: "entra.client_id", EnvironmentVariable: "LLMBRIDGE_ENTRA_CLIENT_ID", Kind: wiring, ValueType: text,
			Description: "The application (client) id of this server's Entra ID app registration; an ID token must name it as its audience."},
		{Key: "entra.redirect_url", EnvironmentVariable: "LLMBRIDGE_ENTRA_REDIRECT_URL", Kind: wiring, ValueType: text,
			Description: "The absolute URL Entra ID sends the browser back to, which must reach GET /auth/entra/callback and be registered on the app exactly."},
		{Key: "entra.authority_url", EnvironmentVariable: "LLMBRIDGE_ENTRA_AUTHORITY_URL", Kind: wiring, ValueType: text, Default: "https://login.microsoftonline.com",
			Description: "The Entra ID login host. The issuer a token must carry is this, the tenant id and /v2.0. It exists so a test can point sign-in at a fake provider."},
		{Key: "auth_store.url", EnvironmentVariable: "AUTH_STORE_URL", Kind: wiring, ValueType: text, Default: "http://127.0.0.1:8303",
			Description: "auth-store, which resolves the credentials bound to a harness instance."},

		// Paths.
		{Key: "skill_store.data_directory", EnvironmentVariable: "LLMBRIDGE_SKILL_STORE_DATA_DIRECTORY", Kind: path, ValueType: text, Default: productiondefaults.SkillStoreDataDirectory(),
			Description: "The directory the skill-store module keeps skill files in (skills/<id>/, beside each SKILL.md) and clones its sources into (repos/<name>/). Its rows, SKILL.md text and seed file versions are in Postgres. Moving it leaves the files behind: copy them along, or every skill file read answers that the file is missing until each source is synced again."},
		{Key: "skill_store.install_directory", EnvironmentVariable: "LLMBRIDGE_SKILL_STORE_INSTALL_DIRECTORY", Kind: path, ValueType: text, Default: productiondefaults.SkillStoreInstallDirectory(),
			Description: "Where the skill-store module copies an installed skill, which is where Claude Code on this machine reads skills from. A local source may not overlap it."},
		{Key: "database.path", EnvironmentVariable: "LLMBRIDGE_DB_PATH", Kind: path, ValueType: text, Default: productiondefaults.BridgeDatabasePath(),
			Description: "Where this server's SQLite database was before its records moved to the bridge_core module in Postgres. The server opens no database here: it refuses to start on an empty bridge_core module while a file is at this path (copy it in with import-bridge-database), and the path's folder holds peer message attachments and self-update logs."},
		{Key: "snapshot_store.git_path", EnvironmentVariable: "LLMBRIDGE_SNAPSHOT_GIT", Kind: path, ValueType: text, Default: productiondefaults.SnapshotStoreGitPath(),
			Description: "The bare git repository snapshot-store keeps file contents in."},
		{Key: "images.directory", EnvironmentVariable: "LLMBRIDGE_IMAGES_DIR", Kind: path, ValueType: text, Default: "images",
			Description: "Where the harness logos served at /images/ are kept."},
		{Key: "session_files.directory", EnvironmentVariable: "LLMBRIDGE_SESSION_FILES_DIR", Kind: path, ValueType: text, Default: productiondefaults.SessionFilesDirectory(),
			Description: "Where a file shared into a session is copied for its agent to read, one directory per session and file."},
		{Key: "bridge_preferences.path", EnvironmentVariable: "LLMBRIDGE_BRIDGE_PREFS", Kind: path, ValueType: text, Default: productiondefaults.BridgePreferencesPath(),
			Description: "The bridge-prefs record: harness defaults, the default principal, permission mode and the chat's last-used state."},
		{Key: "conformance.path", EnvironmentVariable: "LLMBRIDGE_CONFORMANCE_PATH", Kind: path, ValueType: text, Default: productiondefaults.ConformancePath(),
			Description: "The stored harness conformance matrix."},
		{Key: "runner_assets.directory", EnvironmentVariable: "LLMBRIDGE_RUNNER_ASSETS_DIR", Kind: path, ValueType: text,
			Description: "The directory remote runners download harness binaries from. Empty uses the directory beside this binary."},
		{Key: "runner_assets.install_script", EnvironmentVariable: "LLMBRIDGE_RUNNER_INSTALL_SCRIPT", Kind: path, ValueType: text,
			Description: "The runner install script served to a new machine. Empty uses the one in the llm-bridge-runner repo."},

		// Secrets.
		{Key: "demo_login.signing_key", EnvironmentVariable: DemoLoginSigningKeyEnvironmentVariable, Kind: secret, ValueType: text, Required: true,
			Description: "Signs every session agent token and session posting token. The login cookie is an opaque token with a row in login_sessions and is not signed."},
		{Key: "entra.client_secret", EnvironmentVariable: EntraClientSecretEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "The client secret of this server's Entra ID app registration, sent with each authorization code it redeems. It never reaches a harness child."},
		{Key: "service_token", EnvironmentVariable: ServiceTokenEnvironmentVariable, Kind: secret, ValueType: text, Required: true,
			Description: "What an internal service presents in X-LLM-Bridge-Service-Token to reach the operator routes."},
		{Key: "grant_store.service_token", EnvironmentVariable: GrantStoreServiceTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "Not read: grant-store runs in this server. Declared because the shared token file this unit loads still sets it for the standalone grant-store, dash and kanban-store, and it gives unrestricted access to that store: so GET /settings says only whether it is set, and no harness child gets it."},
		{Key: "kanban_store.service_token", EnvironmentVariable: KanbanStoreServiceTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "Not read: kanban-store runs in this server, and the internal service reaches /kanban/ with this server's own service token. Declared because the shared token file this unit loads still sets it for the standalone kanban-store and its callers, and it gives unrestricted access to that store: so GET /settings says only whether it is set, and no harness child gets it."},
		{Key: "file_store.service_token", EnvironmentVariable: FileStoreServiceTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "Not read: file-store runs in this server. Declared because a drop-in may still load the standalone file-store's token file, and that token reads every file the standalone store holds: so GET /settings says only whether it is set, and no harness child gets it."},
		{Key: "database.postgres_url", EnvironmentVariable: PostgresURLEnvironmentVariable, Kind: secret, ValueType: text, Required: true,
			Description: "The Postgres database the modules moved into this server keep their records in: access (principal-store, grant-store, sign-in-store and permission-store), prediction-store, noteboard, project-store, tool-store, skill-store, bundle-store, repo-store, work-graph-store and file-store, each mounted under its own /<store>/ prefix. It is a URL naming a login that may create roles and schemas (CREATEROLE and CREATE on the database). At start the server makes each module's role and schema if they are missing, migrates it and opens it, and refuses to start if any step fails. It is required: every request's principal is read from access. It may carry a password, so it is a secret and never reaches a harness child."},
		{Key: "local_tools.pinchtab_token", EnvironmentVariable: LocalToolsPinchtabTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "The bearer token tool-store's in-process browser tool presents to PinchTab. Unset, it sends no Authorization header."},
		{Key: "local_tools.brave_api_key", EnvironmentVariable: LocalToolsBraveAPIKeyEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "The Brave Search API key tool-store's in-process web_search tool sends. Unset, every web_search call answers that the key is not set. It is not the key an MCP brave-search server gets: that one is resolved from auth-store when the tool is provisioned."},
		{Key: "auth_store.token", EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: secret, ValueType: text,
			Description: "auth-store's bearer token. sign-in-store presents it to read multichat's token and Discord's client secret."},
		{Key: "reference_store.service_token", EnvironmentVariable: ReferenceStoreServiceTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "Not read: reference-store runs in this server. Declared because the shared token file this unit loads still sets it for the standalone reference-store, where it lets its holder look up any record for nobody: so GET /settings says only whether it is set, and no harness child gets it."},
	}

	definitions = append(definitions, taskTrackingSettingDefinitions()...)
	definitions = append(definitions, repositoriesAndReferencesSettingDefinitions()...)
	definitions = append(definitions, sessionLogSettingDefinitions()...)
	definitions = append(definitions, kanbanStoreSettingDefinitions()...)
	definitions = append(definitions, usageAndEvalSettingDefinitions()...)
	definitions = append(definitions, mailSettingDefinitions()...)
	definitions = append(definitions, automationSettingDefinitions()...)
	definitions = append(definitions, photosAndLocationsSettingDefinitions()...)
	definitions = append(definitions, budgetStoreSettingDefinitions()...)
	definitions = append(definitions, possessionsSettingDefinitions()...)

	// One proxy backend per known harness. The variable is per harness, so the
	// declarations are read off the harness registry rather than listed.
	harnessNames := make([]string, 0, len(msg.AllHarnesses))
	for _, harness := range msg.AllHarnesses {
		harnessNames = append(harnessNames, string(harness))
	}
	sort.Strings(harnessNames)
	for _, harnessName := range harnessNames {
		definitions = append(definitions, servicesettings.Definition{
			Key:                 "harness_proxy." + harnessName + ".url",
			EnvironmentVariable: HarnessProxyEnvironmentVariable(harnessName),
			Kind:                wiring, ValueType: text,
			Default:     HarnessProxyDefault(harnessName),
			Description: "The backend /harness/" + harnessName + "/ forwards to. Empty means this harness has no proxy.",
		})
	}
	return definitions
}

// NewSettingsRegistry reads this server's settings from environment. It fails
// on a value that does not parse and on a set LLMBRIDGE_ variable nobody
// declared; whether the required ones are present is CheckRequired, which the
// server's main calls and its operator commands do not.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// StoredSettingSeeds is what each stored behavior setting starts from the
// first time this server runs with it editable: the value Config holds, which
// Load took from the environment or the default and a test wrote as a literal.
// The two values operations.grant_enforcement takes.
const (
	OperationsGrantEnforcementLenient = "lenient"
	OperationsGrantEnforcementStrict  = "strict"
)

// OperationsGrantEnforcementValues is every value operations.grant_enforcement
// accepts.
var OperationsGrantEnforcementValues = []string{OperationsGrantEnforcementLenient, OperationsGrantEnforcementStrict}

func (c *Config) StoredSettingSeeds() map[string]string {
	optOut := make([]string, 0, len(c.SignalClassifierOptOut))
	for harness, skipped := range c.SignalClassifierOptOut {
		if skipped {
			optOut = append(optOut, string(harness))
		}
	}
	sort.Strings(optOut)
	seeds := map[string]string{
		SettingSignalClassifierModelRole:       c.SignalClassifierModelRole,
		SettingQuestionTriageModelRole:         c.QuestionTriageModelRole,
		SettingSignalClassifierOptOutHarnesses: strings.Join(optOut, ","),
		SettingPromptDriftTaggerModelRole:      c.PromptDriftTaggerModelRole,
		SettingSessionRenamerModelRole:         c.SessionRenamerModelRole,
		SettingSignalClassifierTimeout:         c.SignalClassifierTimeout.String(),
		SettingSessionIdleTimeout:              c.IdleTimeout.String(),
		SettingSessionPTYIdleTimeout:           c.PTYIdleTimeout.String(),
		SettingExternalActivityPollInterval:    c.ExternalActivityPollInterval.String(),
	}
	// Seeded only when set, so a Config literal that leaves them out gets the
	// declared defaults rather than an empty value.
	for key, value := range map[string]string{
		SettingOperationsCompletionModelRole: c.OperationsCompletionModelRole,
		SettingOperationsGrantEnforcement:    c.OperationsGrantEnforcement,
		SettingSessionActionsReviewModelRole: c.SessionActionsReviewModelRole,
		SettingImageGenerationInstanceID:     c.ImageGenerationInstanceID,
		SettingServerName:                    c.ServerName,
		SettingChatLinksReferencePageBaseURL: c.ChatLinksReferencePageBaseURL,
		SettingSelfUpdateCommand:             c.SelfUpdateCommand,
		SettingOrchestratorURL:               c.OrchestratorURL,
	} {
		if value != "" {
			seeds[key] = value
		}
	}
	if c.KeepAwakeEnabled {
		seeds[SettingKeepAwakeEnabled] = "true"
	}
	if c.KeepAwakeGraceMinutes > 0 {
		seeds[SettingKeepAwakeGraceMinutes] = strconv.Itoa(c.KeepAwakeGraceMinutes)
	}
	if c.SessionHostHoldSessions {
		seeds[SettingSessionHostHoldSessions] = "true"
	}
	if c.SessionHostHoldMinutes > 0 {
		seeds[SettingSessionHostHoldMinutes] = strconv.Itoa(c.SessionHostHoldMinutes)
	}
	if c.PeerAttachmentMaximumBytes > 0 {
		seeds[SettingPeerAttachmentMaximumBytes] = strconv.Itoa(c.PeerAttachmentMaximumBytes)
	}
	if c.FileStoreMaximumFileBytes > 0 {
		seeds[SettingFileStoreMaximumFileBytes] = strconv.Itoa(c.FileStoreMaximumFileBytes)
	}
	if len(c.FileStoreInlineContentTypes) > 0 {
		seeds[SettingFileStoreInlineContentTypes] = strings.Join(c.FileStoreInlineContentTypes, ",")
	}
	if c.PeerAttachmentsMaximumTotalBytes > 0 {
		seeds[SettingPeerAttachmentsMaximumTotalBytes] = strconv.Itoa(c.PeerAttachmentsMaximumTotalBytes)
	}
	if len(c.OneShotInstanceByProvider) > 0 {
		providers := make([]string, 0, len(c.OneShotInstanceByProvider))
		for provider := range c.OneShotInstanceByProvider {
			providers = append(providers, provider)
		}
		sort.Strings(providers)
		pairs := make([]string, 0, len(providers))
		for _, provider := range providers {
			pairs = append(pairs, provider+":"+c.OneShotInstanceByProvider[provider])
		}
		seeds[SettingOneShotInstanceByProvider] = strings.Join(pairs, ",")
	}
	if c.SignalClassifierMaxChars > 0 {
		seeds[SettingSignalClassifierMaximumCharacters] = strconv.Itoa(c.SignalClassifierMaxChars)
	}
	return seeds
}
