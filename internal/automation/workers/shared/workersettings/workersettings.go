// Package workersettings declares every environment variable each job binary
// under internal/automation/workers reads, one registry per binary.
//
// The job binaries are not the scheduler. Each starts in `sh -c <command>`
// with the environment of the scheduler that runs it plus whatever its job's
// command sets, or in an agent's shell, reads its own variables and serves no
// HTTP, so none of them can put anything on the settings page. Each declares
// what it reads here and builds its own registry at start, as multichat's two
// client commands do. A test holds every environment read in
// workers/<name>/cmd/<name> and the packages it imports to that binary's list.
//
// Every store a binary reads and writes is a path under one address,
// llm-bridge-server's (LLM_BRIDGE_URL), and every call to it carries one
// credential, the service principal token the bridge issued for the binary's
// own principal (LLM_BRIDGE_SERVICE_PRINCIPAL_TOKEN). Package bridgestores
// names the paths and package bridgeauth puts the token on each request.
//
// A job's environment carries every other job's variables, so a binary owns
// only the names that are its alone: a set variable under one of them that the
// binary does not declare stops it, because it is a misspelling or a leftover.
// The shared ones (LLM_BRIDGE_URL, NATS_URL, …) are declared and owned by
// nobody.
package workersettings

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// JobBinary is one of the job binaries under internal/automation/workers.
type JobBinary struct {
	// Name is the folder under workers/ and under its cmd/, and the name in
	// the binary's settings errors.
	Name string
	// OwnedEnvironmentVariablePrefixes are the names that are this binary's
	// alone. A set variable carrying one that the binary does not declare stops
	// it from starting.
	OwnedEnvironmentVariablePrefixes []string
	settingDefinitions               func() []servicesettings.Definition
}

// SettingDefinitions declares every environment variable the binary reads.
func (binary JobBinary) SettingDefinitions() []servicesettings.Definition {
	return binary.settingDefinitions()
}

// NewSettingsRegistry reads the binary's settings from environment. It fails
// on a value that does not parse as its type, and on a set variable under an
// owned prefix that the binary does not declare.
func (binary JobBinary) NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(binary.Name, binary.OwnedEnvironmentVariablePrefixes, binary.settingDefinitions(), environment)
}

// Keys of the job binaries' settings. Each registry is its own namespace.
const (
	SettingBridgeURL                   = "llm_bridge_url"
	SettingBridgeServicePrincipalToken = "llm_bridge_service_principal_token"
	SettingNATSURL                     = "nats_url"
	SettingDashURL                     = "dash_url"
	SettingEmailBoardID                = "email_board_id"
	SettingRoleTags                    = "role_tags"
	SettingDefaultMaxBudgetUSD         = "default_max_budget_usd"
	SettingAskPath                     = "ask_path"
	SettingTimezone                    = "timezone"
	SettingRuntimeDirectory            = "runtime_directory"
	SettingSessionBusAddress           = "session_bus_address"
)

// The values in force with nothing set, where more than one binary uses them.
const (
	DefaultBridgeURL = "http://localhost:8160"
	DefaultNATSURL   = "nats://localhost:4222"
	DefaultDashURL   = "http://localhost:8101"
)

// DefaultReminderTimezone is the zone reminder-coordinator reads the digest
// hour in with REMINDER_TZ unset.
const DefaultReminderTimezone = "America/Los_Angeles"

// DefaultAskPath is the ask binary reminder-coordinator sends nudges through
// with ASK_BIN unset: ~/bin/ask. It is empty when the home directory is
// unknown, and the first nudge then fails to start the command, loudly.
func DefaultAskPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "bin", "ask")
}

// DefaultRuntimeDirectory and DefaultSessionBusAddress are where systemd puts
// this user's runtime directory and session bus. reminder-coordinator hands
// them to `systemctl --user` when the job's environment does not carry them.
func DefaultRuntimeDirectory() string { return fmt.Sprintf("/run/user/%d", os.Getuid()) }

func DefaultSessionBusAddress() string {
	return fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid())
}

// bridgeDefinitions are the address every store is reached under and the
// credential every call to it carries. Every binary declares both.
func bridgeDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingBridgeURL, EnvironmentVariable: "LLM_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultBridgeURL,
			Description: "Where llm-bridge-server answers. Every store the binary reads and writes is a path under it (/issues, /notes, /scheduler, /models, /usage, /principals, /mail), and its sessions are there too. The -bridge-url flag, where the binary has one, overrides it."},
		{Key: SettingBridgeServicePrincipalToken, EnvironmentVariable: "LLM_BRIDGE_SERVICE_PRINCIPAL_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The service principal token llm-bridge-server issued for this binary's own principal (POST /service-principal-tokens), sent as the bearer on every request to the bridge's host and no other. The bridge strips its own service token from the jobs it runs, so the job's command sets this from a host-local file. Unset, every call to the bridge is answered 401; a revoked token, the same."},
	}
}

func natsURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingNATSURL, EnvironmentVariable: "NATS_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultNATSURL,
		Description: "The NATS server the binary publishes to. The scheduler's environment carries it and its jobs inherit it."}
}

// Ask is workers/ask: it opens or continues a session with an alert or
// question for the user. Agents and other jobs run it from their own shells.
var Ask = JobBinary{Name: "ask", settingDefinitions: func() []servicesettings.Definition {
	return append(bridgeDefinitions(), natsURLDefinition())
}}

// Autoworker is workers/autoworker: it dispatches an unattended worker when a
// provider's weekly budget is under-used.
var Autoworker = JobBinary{Name: "autoworker", OwnedEnvironmentVariablePrefixes: []string{"AUTOWORKER_"}, settingDefinitions: func() []servicesettings.Definition {
	return append(bridgeDefinitions(),
		natsURLDefinition(),
		servicesettings.Definition{Key: SettingDefaultMaxBudgetUSD, EnvironmentVariable: "AUTOWORKER_DEFAULT_MAX_BUDGET_USD", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDecimal,
			Description: "The spend cap in USD for a dispatched session whose card has no ceiling of its own; unset or 0 is uncapped. A value that is not a number stops the binary rather than run uncapped. The -default-max-budget-usd flag overrides it."},
	)
}}

// AutoworkerReviewer is workers/autoworker-reviewer: it nudges or tags
// autoworker sessions whose cards have stalled.
var AutoworkerReviewer = JobBinary{Name: "autoworker-reviewer", settingDefinitions: bridgeDefinitions}

// DemoMailGenerator is workers/demo-mail-generator: it writes demo mail into a
// local mail account.
var DemoMailGenerator = JobBinary{Name: "demo-mail-generator", OwnedEnvironmentVariablePrefixes: []string{"DEMO_MAIL_"}, settingDefinitions: bridgeDefinitions}

// DemoWorker is workers/demo-worker: it promotes, dispatches and reviews the
// demo work board's cards.
var DemoWorker = JobBinary{Name: "demo-worker", settingDefinitions: bridgeDefinitions}

// EmailClassifier is workers/email-classifier: it files mail onto a board.
var EmailClassifier = JobBinary{Name: "email-classifier", OwnedEnvironmentVariablePrefixes: []string{"EMAIL_CLASSIFIER_", "EMAIL_BOARD_ID"}, settingDefinitions: func() []servicesettings.Definition {
	return append([]servicesettings.Definition{
		{Key: SettingEmailBoardID, EnvironmentVariable: "EMAIL_BOARD_ID", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The board the classifier files onto; the board's classifier setting says what it files with. The -board-id flag overrides it, and a run with neither is refused."},
	}, bridgeDefinitions()...)
}}

// KanbanClassifier is workers/kanban-classifier: it files agent sessions onto
// a board.
var KanbanClassifier = JobBinary{Name: "kanban-classifier", OwnedEnvironmentVariablePrefixes: []string{"KANBAN_CLASSIFIER_"}, settingDefinitions: bridgeDefinitions}

// KanbanCurator is workers/kanban-curator: it reaps and closes a board's cards.
var KanbanCurator = JobBinary{Name: "kanban-curator", settingDefinitions: bridgeDefinitions}

// KanbanDispatcher is workers/kanban-dispatcher: it starts sessions for a
// board's queued cards.
var KanbanDispatcher = JobBinary{Name: "kanban-dispatcher", settingDefinitions: func() []servicesettings.Definition {
	return append(bridgeDefinitions(), natsURLDefinition())
}}

// KanbanScoper is workers/kanban-scoper: it decomposes a board's parent cards.
var KanbanScoper = JobBinary{Name: "kanban-scoper", OwnedEnvironmentVariablePrefixes: []string{"KANBAN_SCOPER_"}, settingDefinitions: func() []servicesettings.Definition {
	return append(bridgeDefinitions(),
		servicesettings.Definition{Key: SettingRoleTags, EnvironmentVariable: "KANBAN_SCOPER_ROLE_TAGS", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Require a capability and a role in each decomposition and tag sub-cards capability:/role:. The -role-tags flag overrides it. A value that is not true or false stops the binary."},
	)
}}

// ReminderCoordinator is workers/reminder-coordinator: it materialises
// recurring todos and nudges the user about them.
var ReminderCoordinator = JobBinary{Name: "reminder-coordinator", OwnedEnvironmentVariablePrefixes: []string{"REMINDER_"}, settingDefinitions: func() []servicesettings.Definition {
	return append(bridgeDefinitions(),
		servicesettings.Definition{Key: SettingDashURL, EnvironmentVariable: "DASH_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDashURL,
			Description: "Where dash answers; the Google Calendar mirror goes through its authenticated proxy. The -dash-url flag overrides it."},
		servicesettings.Definition{Key: SettingAskPath, EnvironmentVariable: "ASK_BIN", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAskPath(),
			Description: "The ask binary every nudge is sent through. The -ask flag overrides it."},
		servicesettings.Definition{Key: SettingTimezone, EnvironmentVariable: "REMINDER_TZ", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultReminderTimezone,
			Description: "The IANA zone the digest hour and the working day are read in. The -tz flag overrides it."},
		servicesettings.Definition{Key: SettingRuntimeDirectory, EnvironmentVariable: "XDG_RUNTIME_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultRuntimeDirectory(),
			Description: "This user's runtime directory, handed to `systemctl --user` when it reads dash-server's token."},
		servicesettings.Definition{Key: SettingSessionBusAddress, EnvironmentVariable: "DBUS_SESSION_BUS_ADDRESS", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultSessionBusAddress(),
			Description: "This user's session bus, handed to `systemctl --user` when it reads dash-server's token."},
	)
}}

// SpendCeilingGuard is workers/spend-ceiling-guard: it holds a todo tree whose
// sessions have spent past its ceiling.
var SpendCeilingGuard = JobBinary{Name: "spend-ceiling-guard", settingDefinitions: bridgeDefinitions}

// JobBinaries is every job binary under internal/automation/workers. A test
// fails when a workers/<name>/cmd/<name> folder is not on it.
var JobBinaries = []JobBinary{
	Ask, Autoworker, AutoworkerReviewer, DemoMailGenerator, DemoWorker, EmailClassifier,
	KanbanClassifier, KanbanCurator, KanbanDispatcher, KanbanScoper, ReminderCoordinator, SpendCeilingGuard,
}
