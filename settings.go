package grantstore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "grant-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something. The owners' addresses and
// kanban-store's token carry other prefixes, shared with every service that
// calls those owners, so they are declared here and not owned.
const OwnedEnvironmentVariablePrefix = "GRANT_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress              = "listen_address"
	SettingDataDirectory              = "data_directory"
	SettingServiceToken               = "service_token"
	SettingPrincipalStoreURL          = "principal_store_url"
	SettingLLMBridgeServerURL         = "llm_bridge_server_url"
	SettingSkillStoreURL              = "skill_store_url"
	SettingToolStoreURL               = "tool_store_url"
	SettingKanbanStoreURL             = "kanban_store_url"
	SettingKanbanStoreServiceToken    = "kanban_store_service_token"
	SettingReferenceStoreURL          = "reference_store_url"
	SettingReferenceStoreServiceToken = "reference_store_service_token"
	SettingLLMBridgeServiceToken      = "llm_bridge_service_token"
)

// DefaultListenAddress is where the service listens with nothing set. Loopback,
// deliberately, like principal-store: dash is the front door, and a wildcard
// bind would put grant editing on the network for anything that can route to
// this host.
const DefaultListenAddress = "127.0.0.1:8315"

// serviceTokenVariable holds the token an internal service presents in
// ServiceTokenHeader.
const serviceTokenVariable = "GRANT_STORE_SERVICE_TOKEN"

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds the repo to it, so a variable cannot
// be read without being declared here.
//
// The two tokens are strings on purpose: a string always parses, so no error
// from servicesettings.New can quote one.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "GRANT_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so the gateway and dash must be told the new address. Keep it on loopback: deploy.sh fails a bind anywhere else."},
		{Key: SettingDataDirectory, EnvironmentVariable: "GRANT_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds grant-store.db. Changing it starts the service on whatever database is there, or an empty one with no grants; the old grants stay where they were."},
		{Key: SettingServiceToken, EnvironmentVariable: serviceTokenVariable, Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The token an internal service presents in " + ServiceTokenHeader + " to act unrestricted. At least 32 characters. Changing it locks out every caller still holding the old one."},
		{Key: SettingPrincipalStoreURL, EnvironmentVariable: principalStoreURLVariable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultPrincipalStoreURL,
			Description: "Where principal-store answers. Every caller and every grant's principal is checked there; a wrong address makes those checks answer 502."},
		{Key: SettingLLMBridgeServerURL, EnvironmentVariable: llmBridgeServerURLVariable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultLLMBridgeServerURL,
			Description: "Where llm-bridge-server answers. Agents, harness instances and machines are checked there before a grant names one."},
		{Key: SettingSkillStoreURL, EnvironmentVariable: skillStoreURLVariable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultSkillStoreURL,
			Description: "Where skill-store answers. Skills are checked there before a grant names one."},
		{Key: SettingToolStoreURL, EnvironmentVariable: toolStoreURLVariable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultToolStoreURL,
			Description: "Where tool-store answers. Tools are checked there before a grant names one."},
		{Key: SettingKanbanStoreURL, EnvironmentVariable: kanbanStoreURLVariable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultKanbanStoreURL,
			Description: "Where kanban-store answers. Boards are checked there before a grant names one."},
		{Key: SettingKanbanStoreServiceToken, EnvironmentVariable: kanbanStoreServiceTokenVariable, Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token sent to kanban-store on board checks. Unset sends none, which is right only for a kanban-store that does not enforce principals; against one that does, every board check is a 401 reported as a 502."},
		{Key: SettingReferenceStoreURL, EnvironmentVariable: referenceStoreURLVariable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultReferenceStoreURL,
			Description: "Where reference-store answers. Reference types and records are checked there before a grant names one."},
		{Key: SettingReferenceStoreServiceToken, EnvironmentVariable: referenceStoreServiceTokenVariable, Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token sent to reference-store when a grant names one record. Unset sends none, and every such check is a 401 reported as a 502."},
		{Key: SettingLLMBridgeServiceToken, EnvironmentVariable: llmBridgeServiceTokenVariable, Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token sent to llm-bridge-server when a grant names an instance, a machine or a peer connection, whose routes are the bridge's operator routes. Unset sends none, and every such check is a 401 reported as a 502."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a set GRANT_STORE_ variable nobody declared. It does not fail on a missing
// service token: the server's main calls CheckRequired for that.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings to an
// unrestricted caller only: the service token or an administrator. PUT
// /settings/{key} is not mounted: no setting is Editable, so there is nothing
// a write could change.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry, directory PrincipalDirectory, enforcement PrincipalEnforcement) {
	if len(enforcement.ServiceToken) < 32 {
		panic("grant-store: needs a service token of at least 32 characters, or GET /settings would be open to every request that omits the header")
	}
	if directory == nil {
		panic("grant-store: RegisterSettingsHandler needs a PrincipalDirectory; without one an administrator cannot be told from anyone else")
	}
	gate := &handler{directory: directory, enforcement: &enforcement}
	settings := servicesettings.Handler(registry, "/settings")
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		caller, identified := gate.identifyCaller(w, r)
		if !identified {
			return
		}
		if !caller.unrestricted {
			writeErr(w, http.StatusForbidden, "only an administrator or the service token reads this service's settings")
			return
		}
		settings.ServeHTTP(w, r)
	})
}
