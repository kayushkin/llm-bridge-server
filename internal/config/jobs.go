package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The job binaries in cmd/ are not the service. Each starts in `sh -c <command>`
// with the scheduler's environment plus whatever its job's command sets, or in
// an agent's shell, reads its own variables and serves no HTTP, so none of them
// can put anything on the settings page. Each declares what it reads here and
// builds its own registry at start, as multichat's two client commands do. A
// test holds every environment read in cmd/<name> and the packages it imports
// to that binary's list.
//
// A job's environment carries every other job's variables, so a binary owns
// only the names that are its alone: a set variable under one of them that the
// binary does not declare stops it, because it is a misspelling or a leftover.
// The shared addresses (KANBAN_STORE_URL, LLM_BRIDGE_URL, …) are declared and
// owned by nobody.

// JobBinary is one of the cron-job binaries in cmd/.
type JobBinary struct {
	// Name is the directory under cmd/ and the name in the binary's settings
	// errors.
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

// Keys of the job binaries' settings. SettingNATSURL, SettingBridgeURL and
// SettingNoteboardURL are the service's keys, reused: each registry is its own
// namespace. A job reads the bridge's address from LLM_BRIDGE_URL, where the
// service reads LLMBRIDGE_URL.
const (
	SettingKanbanStoreURL          = "kanban_store_url"
	SettingKanbanStoreServiceToken = "kanban_store_service_token"
	SettingBridgeServiceToken      = "llm_bridge_service_token"
	SettingSchedulerURL            = "scheduler_url"
	SettingUsageStoreURL           = "usage_store_url"
	SettingPrincipalStoreURL       = "principal_store_url"
	SettingMailstackURL            = "mailstack_url"
	SettingMailstackToken          = "mailstack_token"
	SettingDashURL                 = "dash_url"
	SettingEmailBoardID            = "email_board_id"
	SettingRoleTags                = "role_tags"
	SettingDefaultMaxBudgetUSD     = "default_max_budget_usd"
	SettingAskPath                 = "ask_path"
	SettingTimezone                = "timezone"
	SettingRuntimeDirectory        = "runtime_directory"
	SettingSessionBusAddress       = "session_bus_address"
)

// The values in force with nothing set, where more than one binary uses them.
const (
	DefaultKanbanStoreURL    = "http://localhost:8305"
	DefaultSchedulerURL      = "http://localhost:8092"
	DefaultUsageStoreURL     = "http://localhost:8185"
	DefaultPrincipalStoreURL = "http://127.0.0.1:8314"
	DefaultMailstackURL      = "http://localhost:8195"
	DefaultDashURL           = "http://localhost:8101"
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

func kanbanStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingKanbanStoreURL, EnvironmentVariable: "KANBAN_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultKanbanStoreURL,
		Description: "Where kanban-store answers. The binary reads and writes its board there; the -kanban-url flag, where it has one, overrides it."}
}

func kanbanStoreServiceTokenDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingKanbanStoreServiceToken, EnvironmentVariable: "KANBAN_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
		Description: "The token the binary presents to kanban-store as an internal service. kanban-store gates every route, so unset, every call to it is answered 401. The scheduler unit carries it and its jobs inherit it."}
}

func bridgeURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingBridgeURL, EnvironmentVariable: "LLM_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultBridgeURL,
		Description: "Where llm-bridge-server answers. The -bridge-url flag, where the binary has one, overrides it."}
}

func bridgeServiceTokenDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
		Description: "The token the binary presents to llm-bridge-server as an internal service, on requests to the bridge's host only. The bridge gates every route, so unset, every call to it is answered 401. The scheduler unit carries it and its jobs inherit it."}
}

func schedulerURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingSchedulerURL, EnvironmentVariable: "SCHEDULER_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultSchedulerURL,
		Description: "Where the scheduler's API answers. -register creates or updates the binary's own cron job there, and a binary that calls a model reads its model role from the scheduler's stored settings there each time it starts."}
}

func natsURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingNATSURL, EnvironmentVariable: "NATS_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultNATSURL,
		Description: "The NATS server the binary publishes to. The scheduler unit sets it and its jobs inherit it."}
}

func noteboardURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingNoteboardURL, EnvironmentVariable: "NOTEBOARD_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultNoteboardURL,
		Description: "Where noteboard answers. The -noteboard-url flag, where the binary has one, overrides it."}
}

func mailstackDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingMailstackURL, EnvironmentVariable: "MAILSTACK_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultMailstackURL,
			Description: "Where mailstack answers. The -mail-url flag overrides it."},
		{Key: SettingMailstackToken, EnvironmentVariable: "MAILSTACK_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "mailstack's bearer token. The -mail-token flag overrides it."},
	}
}

// Ask is cmd/ask: it opens or continues a session with an alert or question
// for the user. Agents and other jobs run it from their own shells.
var Ask = JobBinary{Name: "ask", settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{bridgeURLDefinition(), bridgeServiceTokenDefinition(), natsURLDefinition()}
}}

// Autoworker is cmd/autoworker: it dispatches an unattended worker when a
// provider's weekly budget is under-used.
var Autoworker = JobBinary{Name: "autoworker", OwnedEnvironmentVariablePrefixes: []string{"AUTOWORKER_"}, settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{
		natsURLDefinition(), kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(), noteboardURLDefinition(),
		{Key: SettingUsageStoreURL, EnvironmentVariable: "USAGE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultUsageStoreURL,
			Description: "Where usage-store answers. The weekly limits and what has been used against them come from there, so a wrong address dispatches nothing."},
		schedulerURLDefinition(),
		{Key: SettingDefaultMaxBudgetUSD, EnvironmentVariable: "AUTOWORKER_DEFAULT_MAX_BUDGET_USD", Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeDecimal,
			Description: "The spend cap in USD for a dispatched session whose card has no ceiling of its own; unset or 0 is uncapped. A value that is not a number stops the binary rather than run uncapped. The -default-max-budget-usd flag overrides it."},
	}
}}

// AutoworkerReviewer is cmd/autoworker-reviewer: it nudges or tags autoworker
// sessions whose cards have stalled.
var AutoworkerReviewer = JobBinary{Name: "autoworker-reviewer", settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(), schedulerURLDefinition()}
}}

// DemoMailGenerator is cmd/demo-mail-generator: it writes demo mail into a
// local mailstack account.
var DemoMailGenerator = JobBinary{Name: "demo-mail-generator", OwnedEnvironmentVariablePrefixes: []string{"DEMO_MAIL_"}, settingDefinitions: func() []servicesettings.Definition {
	return append(mailstackDefinitions(),
		bridgeURLDefinition(), bridgeServiceTokenDefinition(),
		schedulerURLDefinition(),
	)
}}

// DemoWorker is cmd/demo-worker: it promotes, dispatches and reviews the demo
// work board's cards.
var DemoWorker = JobBinary{Name: "demo-worker", settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), noteboardURLDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition()}
}}

// EmailClassifier is cmd/email-classifier: it files mail onto a kanban board.
var EmailClassifier = JobBinary{Name: "email-classifier", OwnedEnvironmentVariablePrefixes: []string{"EMAIL_CLASSIFIER_", "EMAIL_BOARD_ID"}, settingDefinitions: func() []servicesettings.Definition {
	return append([]servicesettings.Definition{
		{Key: SettingEmailBoardID, EnvironmentVariable: "EMAIL_BOARD_ID", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The kanban board the classifier files onto; the board's classifier setting says what it files with. The -board-id flag overrides it, and a run with neither is refused."},
		kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(),
	}, append(mailstackDefinitions(),
		bridgeURLDefinition(), bridgeServiceTokenDefinition(),
		noteboardURLDefinition(),
		servicesettings.Definition{Key: SettingPrincipalStoreURL, EnvironmentVariable: "PRINCIPAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultPrincipalStoreURL,
			Description: "Where principal-store answers. A sender's address becomes the contact a ticket names as its requester there. The -principal-store-url flag overrides it."},
		schedulerURLDefinition(),
	)...)
}}

// KanbanClassifier is cmd/kanban-classifier: it files agent sessions onto a
// kanban board.
var KanbanClassifier = JobBinary{Name: "kanban-classifier", OwnedEnvironmentVariablePrefixes: []string{"KANBAN_CLASSIFIER_"}, settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{
		kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(),
		schedulerURLDefinition(),
	}
}}

// KanbanCurator is cmd/kanban-curator: it reaps and closes a board's cards.
var KanbanCurator = JobBinary{Name: "kanban-curator", settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(), schedulerURLDefinition()}
}}

// KanbanDispatcher is cmd/kanban-dispatcher: it starts sessions for a board's
// queued cards.
var KanbanDispatcher = JobBinary{Name: "kanban-dispatcher", settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(), natsURLDefinition(), schedulerURLDefinition()}
}}

// KanbanScoper is cmd/kanban-scoper: it decomposes a board's parent cards.
var KanbanScoper = JobBinary{Name: "kanban-scoper", OwnedEnvironmentVariablePrefixes: []string{"KANBAN_SCOPER_"}, settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{
		kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(),
		{Key: SettingRoleTags, EnvironmentVariable: "KANBAN_SCOPER_ROLE_TAGS", Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Require a capability and a role in each decomposition and tag sub-cards capability:/role:. The -role-tags flag overrides it. A value that is not true or false stops the binary."},
		schedulerURLDefinition(),
	}
}}

// ReminderCoordinator is cmd/reminder-coordinator: it materialises recurring
// todos and nudges the user about them.
var ReminderCoordinator = JobBinary{Name: "reminder-coordinator", OwnedEnvironmentVariablePrefixes: []string{"REMINDER_"}, settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{
		noteboardURLDefinition(),
		{Key: SettingDashURL, EnvironmentVariable: "DASH_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDashURL,
			Description: "Where dash answers; the Google Calendar mirror goes through its authenticated proxy. The -dash-url flag overrides it."},
		{Key: SettingAskPath, EnvironmentVariable: "ASK_BIN", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAskPath(),
			Description: "The ask binary every nudge is sent through. The -ask flag overrides it."},
		{Key: SettingTimezone, EnvironmentVariable: "REMINDER_TZ", Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultReminderTimezone,
			Description: "The IANA zone the digest hour and the working day are read in. The -tz flag overrides it."},
		{Key: SettingRuntimeDirectory, EnvironmentVariable: "XDG_RUNTIME_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultRuntimeDirectory(),
			Description: "This user's runtime directory, handed to `systemctl --user` when it reads dash-server's token."},
		{Key: SettingSessionBusAddress, EnvironmentVariable: "DBUS_SESSION_BUS_ADDRESS", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultSessionBusAddress(),
			Description: "This user's session bus, handed to `systemctl --user` when it reads dash-server's token."},
	}
}}

// SpendCeilingGuard is cmd/spend-ceiling-guard: it holds a todo tree whose
// sessions have spent past its ceiling.
var SpendCeilingGuard = JobBinary{Name: "spend-ceiling-guard", settingDefinitions: func() []servicesettings.Definition {
	return []servicesettings.Definition{kanbanStoreURLDefinition(), kanbanStoreServiceTokenDefinition(), bridgeURLDefinition(), bridgeServiceTokenDefinition(), noteboardURLDefinition()}
}}

// JobBinaries is every binary in cmd/ but the service. A test fails when a
// directory under cmd/ is in neither this list nor the service.
var JobBinaries = []JobBinary{
	Ask, Autoworker, AutoworkerReviewer, DemoMailGenerator, DemoWorker, EmailClassifier,
	KanbanClassifier, KanbanCurator, KanbanDispatcher, KanbanScoper, ReminderCoordinator, SpendCeilingGuard,
}
