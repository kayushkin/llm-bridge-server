// Package config declares every environment variable usage-store-server reads,
// once, with llm-bridge servicesettings. The command reads its configuration
// from the registry built here, GET /settings describes the service from it,
// and a test holds every os.Getenv in the repo to it.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "usage-store"

// OwnedEnvironmentVariablePrefixes are the prefixes of the variables that are
// this service's alone. A set variable carrying one that SettingDefinitions
// does not declare stops the service from starting: it is a misspelling or a
// leftover, and either way someone believes it does something.
//
// They stop short of "USAGE_STORE_" on purpose. USAGE_STORE_URL is how dash,
// the scheduler and llm-bridge-server find this service, and one shared
// environment file carrying it would stop the service over a variable that was
// never meant for it. So USAGE_STORE_LISTEN_ADDRESS and
// USAGE_STORE_LIMITS_REFRESH are refused, and USAGE_STORE_URL is not.
var OwnedEnvironmentVariablePrefixes = []string{
	"USAGE_STORE_LISTEN",
	"USAGE_STORE_DB",
	"USAGE_STORE_TOKENS",
	"USAGE_STORE_LIMITS",
	"USAGE_STORE_CODEX",
	"USAGE_STORE_SPEND",
}

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress                = "listen_address"
	SettingLimitsDatabasePath           = "limits_database_path"
	SettingTokensDatabasePath           = "tokens_database_path"
	SettingLimitsIdleRefreshInterval    = "limits_idle_refresh_interval"
	SettingLimitsWatchedRefreshInterval = "limits_watched_refresh_interval"
	SettingLimitsWatcherExpiry          = "limits_watcher_expiry"
	SettingCodexCommand                 = "codex_command"
	SettingCodexTimeout                 = "codex_timeout"
	SettingAuthStoreURL                 = "auth_store_url"
	SettingAuthStoreToken               = "auth_store_token"
	SettingSpendRefreshInterval         = "spend_refresh_interval"
)

// The values in force with nothing set.
const (
	DefaultListenAddress                = ":8185"
	DefaultLimitsIdleRefreshInterval    = 15 * time.Minute
	DefaultLimitsWatchedRefreshInterval = 2 * time.Minute
	DefaultLimitsWatcherExpiry          = 3 * time.Minute
	DefaultCodexTimeout                 = 30 * time.Second
	DefaultAuthStoreURL                 = "http://127.0.0.1:8303"
	DefaultSpendRefreshInterval         = time.Hour

	// LimitsStaleAfterIdleIntervals is how many idle intervals old a limits
	// snapshot may be before responses mark it stale. It is not a setting.
	LimitsStaleAfterIdleIntervals = 3
)

type Config struct {
	ListenAddr   string
	LimitsDBPath string // limit_snapshots DB (canonical for usage-store)
	TokensDBPath string // token aggregation DB (read-only; currently model-store's)
	// Subscription limits are read every LimitsIdleRefreshInterval, and every
	// LimitsWatchedRefreshInterval while the usage page is open. The page marks
	// itself open about once a minute; a mark counts for LimitsWatcherExpiry.
	// A snapshot is stale after LimitsStaleAfterIdleIntervals idle intervals.
	LimitsIdleRefreshInterval     time.Duration
	LimitsWatchedRefreshInterval  time.Duration
	LimitsWatcherExpiry           time.Duration
	LimitsStaleAfterIdleIntervals int
	// CodexCommand is the codex executable (a path or a name on PATH) that
	// usage-store starts as `codex app-server` to read the live limits. Empty
	// turns codex limits off. CodexTimeout bounds one read.
	CodexCommand string
	CodexTimeout time.Duration

	// Spend (per-API-key cost via admin endpoints). Admin keys are
	// auto-discovered in auth-store by api_key prefix — no per-provider env
	// var to set. AUTH_STORE_TOKEN is the only thing needed; without it,
	// spend collection stays off and the rest of the service still runs.
	AuthStoreURL         string
	AuthStoreToken       string
	SpendRefreshInterval time.Duration
}

// DefaultLimitsDatabasePath is the database the service opens with
// USAGE_STORE_DB unset: ~/.config/usage-store/usage.db. It is empty when the
// home directory is unknown, which leaves the setting unset, and CheckRequired
// then refuses to start rather than open /.config/usage-store/usage.db.
func DefaultLimitsDatabasePath() string {
	return underHome(".config/usage-store/usage.db")
}

// DefaultTokensDatabasePath is the database GET /api/usage reads with
// USAGE_STORE_TOKENS_DB unset: model-store's ~/.config/model-store/store.db.
// It is empty when the home directory is unknown, as DefaultLimitsDatabasePath is.
func DefaultTokensDatabasePath() string {
	return underHome(".config/model-store/store.db")
}

func underHome(relativePath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, relativePath)
}

// SettingDefinitions declares every environment variable this process reads.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
//
// AUTH_STORE_TOKEN stays a string. servicesettings.New quotes a value it cannot
// parse, so a secret of any other type could reach a log through its own error.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "USAGE_STORE_LISTEN_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so every USAGE_STORE_URL (dash, the scheduler's autoworker, llm-bridge-server) must be told the new address."},
		{Key: SettingLimitsDatabasePath, EnvironmentVariable: "USAGE_STORE_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultLimitsDatabasePath(), Required: true,
			Description: "The SQLite file that holds the limit snapshots, per-key spend and top-ups. A leading ~ is the home directory. Changing it starts the service on whatever database is there, or a new empty one; the old rows stay where they were."},
		{Key: SettingTokensDatabasePath, EnvironmentVariable: "USAGE_STORE_TOKENS_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultTokensDatabasePath(), Required: true,
			Description: "The SQLite file GET /api/usage reads token usage from, read-only. It is model-store's database, not this service's. A leading ~ is the home directory. Changing it changes only what that one route answers."},
		{Key: SettingLimitsIdleRefreshInterval, EnvironmentVariable: "USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: DefaultLimitsIdleRefreshInterval.String(),
			Description: "How often every provider's subscription limits are read while nobody has the usage page open. A snapshot is marked stale after three of these, and the autoworker skips a provider whose snapshot is stale. Shorter spends the rate limit Anthropic's usage endpoint shares with every Claude Code process on the login."},
		{Key: SettingLimitsWatchedRefreshInterval, EnvironmentVariable: "USAGE_STORE_LIMITS_WATCHED_REFRESH_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: DefaultLimitsWatchedRefreshInterval.String(),
			Description: "How often the limits are read while the usage page is open. Shorter makes the page fresher and spends the same shared rate limit faster."},
		{Key: SettingLimitsWatcherExpiry, EnvironmentVariable: "USAGE_STORE_LIMITS_WATCHER_EXPIRY", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: DefaultLimitsWatcherExpiry.String(),
			Description: "How long one POST /api/usage/limits/watch keeps the watched interval in force. The page calls about once a minute, so a value under a minute drops back to the idle interval between calls."},
		{Key: SettingCodexCommand, EnvironmentVariable: "USAGE_STORE_CODEX_COMMAND", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The codex executable, a path or a name on PATH, started as `codex app-server` to read the Codex limits. Unset turns Codex limits off, and the start-up log says so."},
		{Key: SettingCodexTimeout, EnvironmentVariable: "USAGE_STORE_CODEX_TIMEOUT", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: DefaultCodexTimeout.String(),
			Description: "How long one read of the Codex limits may take before it is abandoned and counted as a failed read."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAuthStoreURL,
			Description: "Where auth-store answers. The admin key that per-key spend is read with is found and resolved there; a wrong address leaves spend collection off."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token auth-store takes. Unset turns spend collection off; the limits half of the service runs either way."},
		{Key: SettingSpendRefreshInterval, EnvironmentVariable: "USAGE_STORE_SPEND_REFRESH_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: DefaultSpendRefreshInterval.String(),
			Description: "How often each provider's per-key spend is read through its admin endpoint. Shorter makes the spend table fresher and calls the admin API more."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a set variable under an owned prefix that nobody declared, and on an
// interval or timeout that is not a duration.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariablePrefixes, SettingDefinitions(), environment)
}

// Load reads the command's configuration from registry. It fails when a
// database path starts with ~ and the home directory is unknown.
func Load(registry *servicesettings.Registry) (Config, error) {
	limitsDBPath, err := expandHome(registry.String(SettingLimitsDatabasePath))
	if err != nil {
		return Config{}, fmt.Errorf("USAGE_STORE_DB: %w", err)
	}
	tokensDBPath, err := expandHome(registry.String(SettingTokensDatabasePath))
	if err != nil {
		return Config{}, fmt.Errorf("USAGE_STORE_TOKENS_DB: %w", err)
	}
	return Config{
		ListenAddr:                    registry.String(SettingListenAddress),
		LimitsDBPath:                  limitsDBPath,
		TokensDBPath:                  tokensDBPath,
		LimitsIdleRefreshInterval:     registry.Duration(SettingLimitsIdleRefreshInterval),
		LimitsWatchedRefreshInterval:  registry.Duration(SettingLimitsWatchedRefreshInterval),
		LimitsWatcherExpiry:           registry.Duration(SettingLimitsWatcherExpiry),
		LimitsStaleAfterIdleIntervals: LimitsStaleAfterIdleIntervals,
		CodexCommand:                  registry.String(SettingCodexCommand),
		CodexTimeout:                  registry.Duration(SettingCodexTimeout),
		AuthStoreURL:                  registry.String(SettingAuthStoreURL),
		AuthStoreToken:                registry.String(SettingAuthStoreToken),
		SpendRefreshInterval:          registry.Duration(SettingSpendRefreshInterval),
	}, nil
}

// expandHome replaces a leading ~ with the home directory.
func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%q starts with ~ and the home directory is unknown: %w", path, err)
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}
