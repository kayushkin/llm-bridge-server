package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the budget-store settings in this server. budget-store keeps its
// books in its module of database.postgres_url. Each is read once, at start.
//
// Its background work — syncing banks through Plaid, finding recurring
// bills, matching interests and matching spending to trips — writes into
// someone's books, so each pass is off until its own setting turns it on,
// and even then runs only for the owners named in
// budget.passes.owner_principal_ids, each in that owner's books.
const (
	SettingBudgetPlaidEnvironment              = "budget.plaid_environment"
	SettingBudgetPassesOwnerPrincipalIDs       = "budget.passes.owner_principal_ids"
	SettingBudgetPassesInterval                = "budget.passes.interval"
	SettingBudgetBankSyncEnabled               = "budget.bank_sync.enabled"
	SettingBudgetRecurringBillDetectionEnabled = "budget.recurring_bill_detection.enabled"
	SettingBudgetInterestMatchingEnabled       = "budget.interest_matching.enabled"
	SettingBudgetTripMatchingEnabled           = "budget.trip_matching.enabled"
	SettingBudgetTripMatchingHomeMarkers       = "budget.trip_matching.home_markers"
	SettingBudgetTripMatchingTimeZone          = "budget.trip_matching.time_zone"
	SettingBudgetTripMatchingLookbackDays      = "budget.trip_matching.lookback_days"
)

// budgetStoreSettingDefinitions declares the settings above.
func budgetStoreSettingDefinitions() []servicesettings.Definition {
	const (
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
		list     = msg.ServiceSettingValueTypeStringList
		integer  = msg.ServiceSettingValueTypeInteger
		boolean  = msg.ServiceSettingValueTypeBoolean
	)
	return []servicesettings.Definition{
		{Key: SettingBudgetPlaidEnvironment, EnvironmentVariable: "LLMBRIDGE_BUDGET_PLAID_ENVIRONMENT", Kind: behavior, ValueType: text, Default: "",
			Description: "Which Plaid keys budget-store uses: auth-store's credential of provider plaid whose account is this name (sandbox for test data, production for real banks), read with auth_store.url and auth_store.token, which also keep each linked bank's access token. Empty turns budget-store's Plaid routes off (503) and bank sync with them. Set with no auth_store.token refuses to start. Read at start."},
		{Key: SettingBudgetPassesOwnerPrincipalIDs, EnvironmentVariable: "LLMBRIDGE_BUDGET_PASSES_OWNER_PRINCIPAL_IDS", Kind: behavior, ValueType: list, Default: "",
			Description: "The principals whose books budget-store's passes run over, each pass once per owner and in that owner's books only. Empty runs the passes for nobody. Read at start."},
		{Key: SettingBudgetPassesInterval, EnvironmentVariable: "LLMBRIDGE_BUDGET_PASSES_INTERVAL", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "4h",
			Description: "How long budget-store's passes wait after one round (every pass that is on, for every owner) before the next. Read at start."},
		{Key: SettingBudgetBankSyncEnabled, EnvironmentVariable: "LLMBRIDGE_BUDGET_BANK_SYNC_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Each round, sync every linked bank through Plaid (POST /plaid/sync), then pair transfers (POST /transfers/match) and apply payee rules (POST /payees/apply-rules), as the standalone store's scheduler job did. Needs budget.plaid_environment. Off by default, while that scheduler job still runs against the standalone store. Read at start."},
		{Key: SettingBudgetRecurringBillDetectionEnabled, EnvironmentVariable: "LLMBRIDGE_BUDGET_RECURRING_BILL_DETECTION_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Each round, find recurring bills (POST /recurring-bills/detect): new ones are only suggestions an owner confirms. Off by default. Read at start."},
		{Key: SettingBudgetInterestMatchingEnabled, EnvironmentVariable: "LLMBRIDGE_BUDGET_INTEREST_MATCHING_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Each round, run every interest's rules again for new transactions (POST /interests/match). Off by default. Read at start."},
		{Key: SettingBudgetTripMatchingEnabled, EnvironmentVariable: "LLMBRIDGE_BUDGET_TRIP_MATCHING_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Each round, read the owner's journeys from location-store in this process and suggest the spending dated during each, as budget-match-trips does; every link is a suggestion the owner confirms or removes. Needs budget.trip_matching.home_markers and budget.trip_matching.time_zone. Off by default. Read at start."},
		{Key: SettingBudgetTripMatchingHomeMarkers, EnvironmentVariable: "LLMBRIDGE_BUDGET_TRIP_MATCHING_HOME_MARKERS", Kind: behavior, ValueType: list, Default: "",
			Description: "Text that marks a charge as made at home (a city and state as the bank prints them); such charges are never suggested for a trip. Required when trip matching is on. Read at start."},
		{Key: SettingBudgetTripMatchingTimeZone, EnvironmentVariable: "LLMBRIDGE_BUDGET_TRIP_MATCHING_TIME_ZONE", Kind: behavior, ValueType: text, Default: "",
			Description: "The IANA time zone a journey's start and end times are read in, to get the local days the bank dates charges by. Required when trip matching is on. Read at start."},
		{Key: SettingBudgetTripMatchingLookbackDays, EnvironmentVariable: "LLMBRIDGE_BUDGET_TRIP_MATCHING_LOOKBACK_DAYS", Kind: behavior, ValueType: integer, Default: "60",
			Description: "Trip matching looks only at journeys that ended in this many days before the round, as the scheduler job's -since did. Read at start."},
	}
}
