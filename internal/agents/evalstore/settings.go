package evalstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "eval-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "EVAL_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress            = "listen_address"
	SettingDataDirectory            = "data_directory"
	SettingServerURL                = "server_url"
	SettingLLMBridgeURL             = "llm_bridge_url"
	SettingLLMBridgeServiceToken    = "llm_bridge_service_token"
	SettingOrganizationID           = "organization_id"
	SettingModelStoreURL            = "model_store_url"
	SettingUsageStoreURL            = "usage_store_url"
	SettingDefaultConcurrency       = "default_concurrency"
	SettingDefaultBudgetLimits      = "default_budget_limits"
	SettingBudgetCheckEveryCalls    = "budget_check_every_calls"
	SettingAnswerTimeout            = "answer_timeout"
	SettingJudgeTimeout             = "judge_timeout"
	SettingOperationPollingInterval = "operation_polling_interval"
)

// DefaultDataDir is where the database lives when nothing says otherwise.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "eval-store")
}

// SettingDefinitions declares every environment variable this process reads,
// once. Addresses have no default: the unit names them, so no port is written
// into the code. Nothing is Editable: GET /settings is as open as every other
// route and there is no operator gate to put a write behind.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "EVAL_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The address the HTTP server listens on. Required by serve; the unit sets it. Changing it moves the service."},
		{Key: SettingDataDirectory, EnvironmentVariable: "EVAL_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds eval-store.db, created mode 700. Cases hold real prompts, so it must stay outside any repository."},
		{Key: SettingServerURL, EnvironmentVariable: "EVAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where the eval-store command line finds the server, e.g. http://localhost:<port of EVAL_STORE_ADDR>. The server itself does not read it."},
		{Key: SettingLLMBridgeURL, EnvironmentVariable: "EVAL_STORE_LLM_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "llm-bridge-server's base URL. Every model call is an llm.completion operation there. Without it a run cannot start and cases cannot be imported from operations."},
		{Key: SettingLLMBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "llm-bridge-server's internal service token, sent as X-LLM-Bridge-Service-Token alone, so eval-store calls as an internal service and acts as no principal, as scheduler does. Loaded from the shared principal-gating tokens file, as scheduler loads it."},
		{Key: SettingOrganizationID, EnvironmentVariable: "EVAL_STORE_ORGANIZATION_ID", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The principal-store group a run's calls are billed to when the run names none. Its monthly operation budget pays for them. A run with no organization refuses to start."},
		{Key: SettingModelStoreURL, EnvironmentVariable: "EVAL_STORE_MODEL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "model-store's base URL. A new run reads every model's provider and list price from GET /api/models there and freezes them on the run."},
		{Key: SettingUsageStoreURL, EnvironmentVariable: "EVAL_STORE_USAGE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "usage-store's base URL. Before each batch of calls a run reads GET /api/usage/limits there and stops when a subscription is past the run's limit."},
		{Key: SettingDefaultConcurrency, EnvironmentVariable: "EVAL_STORE_DEFAULT_CONCURRENCY", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "4",
			Description: "How many model calls a run makes at once when it names no concurrency."},
		{Key: SettingDefaultBudgetLimits, EnvironmentVariable: "EVAL_STORE_DEFAULT_BUDGET_LIMITS", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeStringMap, Default: "codex.weekly:60,anthropic.seven_day:40",
			Description: "subscription.window:percent pairs a run gets when it names no budget limits. The run stops, resumable, once usage-store reports any of them above its percent."},
		{Key: SettingBudgetCheckEveryCalls, EnvironmentVariable: "EVAL_STORE_BUDGET_CHECK_EVERY_CALLS", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "8",
			Description: "How many calls a run makes between budget readings. A run may overshoot a limit by this many calls."},
		{Key: SettingAnswerTimeout, EnvironmentVariable: "EVAL_STORE_ANSWER_TIMEOUT", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "10m",
			Description: "How long a candidate's call may take before the operation is asked to stop and the answer is recorded as failed."},
		{Key: SettingJudgeTimeout, EnvironmentVariable: "EVAL_STORE_JUDGE_TIMEOUT", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "15m",
			Description: "How long a judge's call may take before the operation is asked to stop and the verdict is recorded as failed."},
		{Key: SettingOperationPollingInterval, EnvironmentVariable: "EVAL_STORE_OPERATION_POLLING_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "2s",
			Description: "How often a waiting call reads its operation's receipt."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set EVAL_STORE_ variable nobody
// declared. Required settings are checked by the caller that needs them
// (serve), so the command line runs without a listen address.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings. No setting is
// Editable, so no write route is mounted.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
