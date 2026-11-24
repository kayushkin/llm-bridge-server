package signinstore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "sign-in-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
const OwnedEnvironmentVariablePrefix = "SIGN_IN_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress        = "listen_address"
	SettingDataDirectory        = "data_directory"
	SettingServiceToken         = "service_token"
	SettingSignInPageURL        = "sign_in_page_url"
	SettingCookieDomain         = "cookie_domain"
	SettingPrincipalStoreURL    = "principal_store_url"
	SettingPeopleStoreURL       = "people_store_url"
	SettingMultichatURL         = "multichat_url"
	SettingMultichatCredential  = "multichat_credential_id"
	SettingAuthStoreURL         = "auth_store_url"
	SettingAuthStoreToken       = "auth_store_token"
	SettingDiscordApplicationID = "discord_application_id"
	SettingDiscordSecretID      = "discord_client_secret_credential_id"
	SettingDiscordRedirectURL   = "discord_redirect_url"
	MinimumServiceTokenLength   = 32
	DefaultListenAddress        = "127.0.0.1:8328"
	defaultPrincipalStoreURL    = "http://127.0.0.1:8314"
	defaultPeopleStoreURL       = "http://127.0.0.1:8321"
	defaultMultichatURL         = "http://127.0.0.1:8402"
	defaultAuthStoreURL         = "http://127.0.0.1:8303"
	defaultSignInPageURL        = "https://kayushkin.com/signin"
	defaultCookieDomain         = "kayushkin.com"
)

// SettingDefinitions declares every environment variable the service reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it, never showing a secret; and a test holds every
// os.Getenv in the repo to it. Nothing is Editable.
func SettingDefinitions() []servicesettings.Definition {
	text := msg.ServiceSettingValueTypeString
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "SIGN_IN_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Loopback: nginx is the only way in from outside, and it passes only the browser routes."},
		{Key: SettingDataDirectory, EnvironmentVariable: "SIGN_IN_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: text, Default: DefaultDataDir(),
			Description: "The directory that holds sign-in-store.db: sign-in links and sessions, each kept only as a hash of its secret."},
		{Key: SettingServiceToken, EnvironmentVariable: "SIGN_IN_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: text, Required: true,
			Description: "What dash and the operator's scripts present in X-Sign-In-Store-Service-Token, with X-Principal-Id naming an administrator, to make links and list or revoke sessions. At least 32 characters."},
		{Key: SettingSignInPageURL, EnvironmentVariable: "SIGN_IN_STORE_SIGN_IN_PAGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: defaultSignInPageURL,
			Description: "The page a sign-in link opens. A link is this URL, \"#\" and the link's secret; the page posts the secret to POST /redeem."},
		{Key: SettingCookieDomain, EnvironmentVariable: "SIGN_IN_STORE_COOKIE_DOMAIN", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: defaultCookieDomain,
			Description: "The domain the session cookie is set for, which includes every name under it: one sign-in reaches dash, the site and the film player."},
		{Key: SettingPrincipalStoreURL, EnvironmentVariable: "PRINCIPAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: defaultPrincipalStoreURL,
			Description: "principal-store, which says whether a principal exists, is enabled, is a person and is an administrator, read on every session check."},
		{Key: SettingPeopleStoreURL, EnvironmentVariable: "PEOPLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: defaultPeopleStoreURL,
			Description: "people-store, which turns a Discord username or phone number into a person, their principal and their chat accounts."},
		{Key: SettingMultichatURL, EnvironmentVariable: "MULTICHAT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: defaultMultichatURL,
			Description: "multichat, which carries a sign-in link to a person's one-to-one chat."},
		{Key: SettingMultichatCredential, EnvironmentVariable: "SIGN_IN_STORE_MULTICHAT_CREDENTIAL_ID", Kind: msg.ServiceSettingKindBehavior, ValueType: text, Required: true,
			Description: "The auth-store credential that holds multichat's bearer token, read when a link is sent."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: text, Default: defaultAuthStoreURL,
			Description: "auth-store, where multichat's token is read from."},
		{Key: SettingDiscordApplicationID, EnvironmentVariable: "SIGN_IN_STORE_DISCORD_APPLICATION_ID", Kind: msg.ServiceSettingKindWiring, ValueType: text,
			Description: "The Discord application Sign in with Discord uses (the operator's Event-Manager app, shared with discord-signup-store). Unset switches Sign in with Discord off: its routes answer 503."},
		{Key: SettingDiscordSecretID, EnvironmentVariable: "SIGN_IN_STORE_DISCORD_CLIENT_SECRET_CREDENTIAL_ID", Kind: msg.ServiceSettingKindWiring, ValueType: text,
			Description: "The auth-store credential holding that application's OAuth2 client secret, read each time a code is traded."},
		{Key: SettingDiscordRedirectURL, EnvironmentVariable: "SIGN_IN_STORE_DISCORD_REDIRECT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: text,
			Description: "Where Discord sends the browser back: this service's GET /discord/callback as the browser reaches it. It must be in the application's OAuth2 redirect list on Discord's developer portal, or Discord refuses every sign-in."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: text, Required: true,
			Description: "The bearer auth-store asks for."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set SIGN_IN_STORE_ variable nobody
// declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings. Secrets are
// described as set or unset, never shown.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
