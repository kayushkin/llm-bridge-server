package config

import (
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
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

// Keys of the behaviour settings the server stores and reads at the time of
// use. Everything else is read once, from the environment, into Config.
const (
	SettingSignalClassifierModel             = "signal_classifier.model"
	SettingSignalClassifierInstance          = "signal_classifier.instance"
	SettingSignalClassifierTimeout           = "signal_classifier.timeout"
	SettingSignalClassifierMaximumCharacters = "signal_classifier.maximum_characters"
	SettingSignalClassifierOptOutHarnesses   = "signal_classifier.opt_out_harnesses"
	SettingPromptDriftTaggerInstance         = "prompt_drift_tagger.instance"
	SettingPromptDriftTaggerModel            = "prompt_drift_tagger.model"
	SettingSessionIdleTimeout                = "session.idle_timeout"
	SettingSessionPTYIdleTimeout             = "session.pty_idle_timeout"
)

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
		wiring    = msg.ServiceSettingKindWiring
		path      = msg.ServiceSettingKindPath
		secret    = msg.ServiceSettingKindSecret
		behaviour = msg.ServiceSettingKindBehaviour
		text      = msg.ServiceSettingValueTypeString
	)
	definitions := []servicesettings.Definition{
		// Behaviour the server stores: changed on the settings page, no restart.
		{Key: SettingSignalClassifierModel, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_MODEL", Kind: behaviour, ValueType: text, Editable: true, Default: "claude-haiku-4-5",
			Description: "The model the turn-end signal classifier and question triage ask for. Empty switches both off: no derived signals, and a worker's question reaches its card untriaged."},
		{Key: SettingSignalClassifierInstance, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_INSTANCE", Kind: behaviour, ValueType: text, Editable: true, Default: "inst-cc-local",
			Description: "The harness instance the classifier and triage run their one-shot calls on. It must be an enabled instance; a claude_code instance with no bound credential puts the calls on the subscription login rather than an API key."},
		{Key: SettingSignalClassifierTimeout, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_TIMEOUT", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "20s",
			Description: "How long one classify or triage call may take. On timeout the turn keeps the state the heuristic gave it and no signal is written."},
		{Key: SettingSignalClassifierMaximumCharacters, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_MAX_CHARS", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeInteger, Editable: true, Default: "6000",
			Description: "How much of a turn's final text is sent to the classifier, counted from the end, where a question or a sign-off lives."},
		{Key: SettingSignalClassifierOptOutHarnesses, EnvironmentVariable: "LLMBRIDGE_SIGNAL_CLASSIFIER_OPT_OUT", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeStringList, Editable: true,
			Description: "Harnesses whose turns the classifier skips, comma-separated by harness id."},
		{Key: SettingPromptDriftTaggerInstance, EnvironmentVariable: "LLMBRIDGE_PROMPT_DRIFT_TAGGER_INSTANCE", Kind: behaviour, ValueType: text, Editable: true, Default: "inst-cc-local",
			Description: "The harness instance that proposes tags for sections a prompt-file edit adds, one call per held drift. Empty switches labelling off: drifts are still detected and held, unlabelled."},
		{Key: SettingPromptDriftTaggerModel, EnvironmentVariable: "LLMBRIDGE_PROMPT_DRIFT_TAGGER_MODEL", Kind: behaviour, ValueType: text, Editable: true, Default: "claude-haiku-4-5",
			Description: "The model the prompt drift tagger asks for."},
		{Key: SettingSessionIdleTimeout, EnvironmentVariable: "LLMBRIDGE_IDLE_TIMEOUT", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "15m",
			Description: "How long an events-mode session may sit with no new event before its harness process is stopped and the session marked aborted. 0s or less switches reaping off."},
		{Key: SettingSessionPTYIdleTimeout, EnvironmentVariable: "LLMBRIDGE_PTY_IDLE_TIMEOUT", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeDuration, Editable: true, Default: "60m",
			Description: "The same cutoff for pty-mode sessions, where a person reading output emits nothing. 0s or less switches reaping off."},

		// Behaviour read once at start.
		{Key: "session.purpose_folders", EnvironmentVariable: "LLMBRIDGE_PURPOSE_FOLDERS", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeStringMap,
			Description: "purpose:folder pairs laid over the purpose registry's own folders, deciding where a new session is filed. The Settings page's source folders override both at runtime."},
		{Key: "pty.ring_buffer_bytes", EnvironmentVariable: "LLMBRIDGE_PTY_RING_BUFFER_BYTES", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeInteger, Default: "65536",
			Description: "How many bytes of recent pty output each session keeps to replay to a terminal that attaches late."},
		{Key: "operations.worker_count", EnvironmentVariable: "LLMBRIDGE_OPERATIONS_WORKERS", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeInteger, Default: "4",
			Description: "How many operations run at once. More wait in the queue."},
		{Key: "operations.lease_duration", EnvironmentVariable: "LLMBRIDGE_OPERATIONS_LEASE", Kind: behaviour, ValueType: msg.ServiceSettingValueTypeDuration, Default: "30s",
			Description: "How long a worker's claim on a running operation lasts without renewal. An operation whose worker stops renewing is reconciled once it runs out."},
		{Key: "tests.allow_production_addresses", EnvironmentVariable: productiondefaults.AllowInTestsEnvironmentVariable, Kind: behaviour, ValueType: text,
			Description: "Set to anything, lets a go test binary use this host's live addresses. Never set on a running server."},

		// Wiring.
		{Key: "listen_address", EnvironmentVariable: "LLMBRIDGE_LISTEN_ADDR", Kind: wiring, ValueType: text, Default: productiondefaults.ListenAddr,
			Description: "The address this server listens on."},
		{Key: "public_url", EnvironmentVariable: "LLMBRIDGE_PUBLIC_URL", Kind: wiring, ValueType: text,
			Description: "The URL remote runners fetch harness binaries from. Empty makes a runner use the address it reached the server on."},
		{Key: "principal_store.url", EnvironmentVariable: "LLMBRIDGE_PRINCIPAL_STORE_URL", Kind: wiring, ValueType: text, Required: true,
			Description: "principal-store, which every caller's principal is read from. It has no default: a guessed address would answer who is an administrator."},
		{Key: "grant_store.url", EnvironmentVariable: "LLMBRIDGE_GRANT_STORE_URL", Kind: wiring, ValueType: text, Required: true,
			Description: "grant-store, read at spawn for a principal's grants and mounted at /grant-store/. It has no default."},
		{Key: "kanban_store.url", EnvironmentVariable: "LLMBRIDGE_KANBAN_STORE_URL", Kind: wiring, ValueType: text, Required: true,
			Description: "kanban-store, which owns the session-to-card link and is mounted at /kanban/. It has no default."},
		{Key: "bundle_store.url", EnvironmentVariable: "LLMBRIDGE_BUNDLE_STORE_URL", Kind: wiring, ValueType: text, Default: productiondefaults.BundleStoreURL,
			Description: "bundle-store, asked whether a session's bundle exists and what it resolves to."},
		{Key: "tool_store.url", EnvironmentVariable: "LLMBRIDGE_TOOL_STORE_URL", Kind: wiring, ValueType: text, Default: productiondefaults.ToolStoreURL,
			Description: "tool-store, which provisions the tools a session is offered."},
		{Key: "permission_store.url", EnvironmentVariable: "LLMBRIDGE_PERMISSION_STORE_URL", Kind: wiring, ValueType: text, Default: productiondefaults.PermissionStoreURL,
			Description: "permission-store, which the tool-call prehook asks for a verdict."},
		{Key: "log_store.url", EnvironmentVariable: "LLMBRIDGE_LOG_STORE_URL", Kind: wiring, ValueType: text, Default: productiondefaults.LogStoreURL,
			Description: "log-store, which keeps every session event."},
		{Key: "mailstack.url", EnvironmentVariable: "LLMBRIDGE_MAILSTACK_URL", Kind: wiring, ValueType: text, Default: productiondefaults.MailstackURL,
			Description: "mailstack, asked for the sender and subject behind a card when triage drafts a customer reply."},
		{Key: "healthcheck.url", EnvironmentVariable: "LLMBRIDGE_HEALTHCHECK_URL", Kind: wiring, ValueType: text, Default: productiondefaults.HealthcheckURL,
			Description: "healthcheck, whose status is the list of services the service inventory shows."},
		{Key: "model_store.url", EnvironmentVariable: "LLMBRIDGE_MODEL_STORE_URL", Kind: wiring, ValueType: text,
			Description: "A model-store service to ask instead of opening its database. Empty opens the database."},
		{Key: "agent_store.url", EnvironmentVariable: "LLMBRIDGE_AGENT_STORE_URL", Kind: wiring, ValueType: text,
			Description: "An agent-store service to ask instead of embedding it. Empty embeds it, which is how this host runs."},
		{Key: "agent_store_proxy.url", EnvironmentVariable: "AGENT_STORE_URL", Kind: wiring, ValueType: text, Default: "http://localhost:8300",
			Description: "Where /api/agent-store/ forwards to."},
		{Key: "skill_store_proxy.url", EnvironmentVariable: "SKILL_STORE_URL", Kind: wiring, ValueType: text, Default: "http://localhost:8301",
			Description: "Where /api/skill-store/ forwards to."},
		{Key: "auth_store.url", EnvironmentVariable: "AUTH_STORE_URL", Kind: wiring, ValueType: text, Default: "http://127.0.0.1:8303",
			Description: "auth-store, which resolves the credentials bound to a harness instance."},

		// Paths.
		{Key: "database.path", EnvironmentVariable: "LLMBRIDGE_DB_PATH", Kind: path, ValueType: text, Default: productiondefaults.BridgeDatabasePath(),
			Description: "This server's own database: sessions, folders, signals and the stored settings on this page."},
		{Key: "agent_store.database_path", EnvironmentVariable: "LLMBRIDGE_AGENT_DB", Kind: path, ValueType: text, Default: productiondefaults.AgentStoreDatabasePath(),
			Description: "The embedded agent-store's database, which also holds the host prompt."},
		{Key: "memory_store.database_path", EnvironmentVariable: "LLMBRIDGE_MEMORY_DB", Kind: path, ValueType: text, Default: productiondefaults.MemoryStoreDatabasePath(),
			Description: "The embedded memory-store's database."},
		{Key: "harness_store.database_path", EnvironmentVariable: "LLMBRIDGE_HARNESS_DB", Kind: path, ValueType: text, Default: productiondefaults.HarnessStoreDatabasePath(),
			Description: "The embedded harness-store's database: instances, machines, credential bindings."},
		{Key: "hook_store.database_path", EnvironmentVariable: "LLMBRIDGE_HOOK_DB", Kind: path, ValueType: text, Default: productiondefaults.HookStoreDatabasePath(),
			Description: "The embedded hook-store's database."},
		{Key: "model_store.database_path", EnvironmentVariable: "LLMBRIDGE_MODEL_STORE_DB", Kind: path, ValueType: text, Default: productiondefaults.ModelStoreDatabasePath(),
			Description: "model-store's database, opened when no model-store URL is set."},
		{Key: "snapshot_store.database_path", EnvironmentVariable: "LLMBRIDGE_SNAPSHOT_DB", Kind: path, ValueType: text, Default: productiondefaults.SnapshotStoreDatabasePath(),
			Description: "The embedded snapshot-store's metadata database."},
		{Key: "snapshot_store.git_path", EnvironmentVariable: "LLMBRIDGE_SNAPSHOT_GIT", Kind: path, ValueType: text, Default: productiondefaults.SnapshotStoreGitPath(),
			Description: "The bare git repository snapshot-store keeps file contents in."},
		{Key: "images.directory", EnvironmentVariable: "LLMBRIDGE_IMAGES_DIR", Kind: path, ValueType: text, Default: "images",
			Description: "Where images attached to a message are kept."},
		{Key: "operations.database_path", EnvironmentVariable: "LLMBRIDGE_OPERATIONS_DB", Kind: path, ValueType: text, Default: productiondefaults.OperationsDatabasePath(),
			Description: "The operations database: every operation's intent, receipt, events and lease."},
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
			Description: "Signs the login cookie and every session agent token."},
		{Key: "service_token", EnvironmentVariable: ServiceTokenEnvironmentVariable, Kind: secret, ValueType: text, Required: true,
			Description: "What an internal service presents in X-LLM-Bridge-Service-Token to reach the operator routes."},
		{Key: "grant_store.service_token", EnvironmentVariable: GrantStoreServiceTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "Sent to grant-store on the calls this server makes as itself. Unset, grant-store answers them 401."},
		{Key: "kanban_store.service_token", EnvironmentVariable: kanbanclient.ServiceTokenEnvironmentVariable, Kind: secret, ValueType: text,
			Description: "Sent to kanban-store on the calls this server makes as itself."},
		{Key: "mailstack.token", EnvironmentVariable: "LLMBRIDGE_MAILSTACK_TOKEN", Kind: secret, ValueType: text,
			Description: "mailstack's bearer token. Unset switches the sender lookup off, and a customer reply draft carries no recipient."},
		{Key: "auth_store.token", EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: secret, ValueType: text,
			Description: "auth-store's bearer token."},
	}

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

// StoredSettingSeeds is what each stored behaviour setting starts from the
// first time this server runs with it editable: the value Config holds, which
// Load took from the environment or the default and a test wrote as a literal.
func (c *Config) StoredSettingSeeds() map[string]string {
	optOut := make([]string, 0, len(c.SignalClassifierOptOut))
	for harness, skipped := range c.SignalClassifierOptOut {
		if skipped {
			optOut = append(optOut, string(harness))
		}
	}
	sort.Strings(optOut)
	seeds := map[string]string{
		SettingSignalClassifierModel:           c.SignalClassifierModel,
		SettingSignalClassifierInstance:        c.SignalClassifierInstance,
		SettingSignalClassifierOptOutHarnesses: strings.Join(optOut, ","),
		SettingPromptDriftTaggerInstance:       c.PromptDriftTaggerInstance,
		SettingPromptDriftTaggerModel:          c.PromptDriftTaggerModel,
		SettingSignalClassifierTimeout:         c.SignalClassifierTimeout.String(),
		SettingSessionIdleTimeout:              c.IdleTimeout.String(),
		SettingSessionPTYIdleTimeout:           c.PTYIdleTimeout.String(),
	}
	if c.SignalClassifierMaxChars > 0 {
		seeds[SettingSignalClassifierMaximumCharacters] = strconv.Itoa(c.SignalClassifierMaxChars)
	}
	return seeds
}
