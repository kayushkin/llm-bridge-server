package referencestore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "reference-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "REFERENCE_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress           = "listen_address"
	SettingServiceURLs             = "service_urls"
	SettingSiteURLs                = "site_urls"
	SettingPrincipalStoreURL       = "principal_store_url"
	SettingGrantStoreURL           = "grant_store_url"
	SettingAuthStoreURL            = "auth_store_url"
	SettingAuthStoreToken          = "auth_store_token"
	SettingAuthStoreProviders      = "auth_store_providers"
	SettingBridgeServiceToken      = "bridge_service_token"
	SettingServiceToken            = "service_token"
	SettingRecordCacheSeconds      = "record_cache_seconds"
	SettingMissCacheSeconds        = "miss_cache_seconds"
	SettingAccessCacheSeconds      = "access_cache_seconds"
	SettingLookupTimeoutSeconds    = "lookup_timeout_seconds"
	SettingLookupConcurrencyPerAsk = "lookup_concurrency_per_ask"
)

// DefaultListenAddress is loopback, deliberately: the service trusts
// X-Principal-Id, so only llm-bridge-server's gateway may reach it.
const DefaultListenAddress = "127.0.0.1:8343"

// ServiceTokenHeader carries the service token that lifts the access check,
// for internal callers that act for nobody (grant-store checking that a
// record exists). llm-bridge-server's gateway deletes it from every request
// it forwards.
const ServiceTokenHeader = "X-Reference-Store-Service-Token"

// PrincipalIDHeader is who the caller acts for, set by llm-bridge-server's
// gateway from a checked login and by nothing else.
const PrincipalIDHeader = "X-Principal-Id"

// SettingDefinitions declares every environment variable the service reads.
func SettingDefinitions() []servicesettings.Definition {
	wiring := msg.ServiceSettingKindWiring
	behavior := msg.ServiceSettingKindBehavior
	secret := msg.ServiceSettingKindSecret
	text := msg.ServiceSettingValueTypeString
	pairs := msg.ServiceSettingValueTypeStringMap
	integer := msg.ServiceSettingValueTypeInteger
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "REFERENCE_STORE_ADDR", Kind: wiring, ValueType: text, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Keep it on loopback: the service trusts X-Principal-Id."},
		{Key: SettingServiceURLs, EnvironmentVariable: "REFERENCE_STORE_SERVICE_URLS", Kind: wiring, ValueType: pairs,
			Description: "service:url pairs for services configured on this deployment. A lookup or search for an unconfigured service returns an explicit error."},
		{Key: SettingSiteURLs, EnvironmentVariable: "REFERENCE_STORE_SITE_URLS", Kind: wiring, ValueType: pairs,
			Description: "site:url pairs for record pages served somewhere other than the page that draws the chip, e.g. music:https://example.com. A type whose page names a site missing here has no page link."},
		{Key: SettingPrincipalStoreURL, EnvironmentVariable: "REFERENCE_STORE_PRINCIPAL_STORE_URL", Kind: wiring, ValueType: text, Required: true,
			Description: "principal-store, asked whether the caller is an administrator, who sees every record."},
		{Key: SettingGrantStoreURL, EnvironmentVariable: "REFERENCE_STORE_GRANT_STORE_URL", Kind: wiring, ValueType: text, Required: true,
			Description: "grant-store, asked for the caller's can_see_references grants."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "REFERENCE_STORE_AUTH_STORE_URL", Kind: wiring, ValueType: text,
			Description: "auth-store, asked for the credential of a service named in auth_store_providers."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "REFERENCE_STORE_AUTH_STORE_TOKEN", Kind: secret, ValueType: text,
			Description: "Bearer token for auth-store."},
		{Key: SettingAuthStoreProviders, EnvironmentVariable: "REFERENCE_STORE_AUTH_STORE_PROVIDERS", Kind: wiring, ValueType: pairs,
			Description: "service:provider pairs: the auth-store provider whose api_key is sent as a bearer token to that service, e.g. multichat:multichat."},
		{Key: SettingBridgeServiceToken, EnvironmentVariable: "REFERENCE_STORE_BRIDGE_SERVICE_TOKEN", Kind: secret, ValueType: text,
			Description: "llm-bridge-server's service token, sent with X-Principal-Id so the bridge decides which sessions the caller may see."},
		{Key: SettingServiceToken, EnvironmentVariable: "REFERENCE_STORE_SERVICE_TOKEN", Kind: secret, ValueType: text, Required: true,
			Description: "The token an internal caller sends in X-Reference-Store-Service-Token to look records up for nobody, unchecked."},
		{Key: SettingRecordCacheSeconds, EnvironmentVariable: "REFERENCE_STORE_RECORD_CACHE_SECONDS", Kind: behavior, ValueType: integer, Default: "60",
			Description: "How long a record a store answered is reused before the store is asked again."},
		{Key: SettingMissCacheSeconds, EnvironmentVariable: "REFERENCE_STORE_MISS_CACHE_SECONDS", Kind: behavior, ValueType: integer, Default: "15",
			Description: "How long an id no store knows is remembered as unknown."},
		{Key: SettingAccessCacheSeconds, EnvironmentVariable: "REFERENCE_STORE_ACCESS_CACHE_SECONDS", Kind: behavior, ValueType: integer, Default: "30",
			Description: "How long a caller's administrator flag and grants are reused before principal-store and grant-store are asked again."},
		{Key: SettingLookupTimeoutSeconds, EnvironmentVariable: "REFERENCE_STORE_LOOKUP_TIMEOUT_SECONDS", Kind: behavior, ValueType: integer, Default: "5",
			Description: "How long one store is given to answer for one id."},
		{Key: SettingLookupConcurrencyPerAsk, EnvironmentVariable: "REFERENCE_STORE_LOOKUP_CONCURRENCY_PER_ASK", Kind: behavior, ValueType: integer, Default: "8",
			Description: "How many stores one POST /resolve asks at once."},
	}
}

// NewSettingsRegistry reads this service's settings from environment.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
