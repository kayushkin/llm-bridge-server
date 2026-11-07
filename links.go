package timeledgerstore

import (
	"regexp"
	"sort"
)

// LinkType is one kind of record a time entry can link to. Each has its own
// table, keyed by the id the owning store gave the record; the columns past
// the shared ones are named by the Has… fields.
type LinkType struct {
	Name          string `json:"name"`
	Table         string `json:"table"`
	RefColumn     string `json:"ref_column"`
	OwningStore   string `json:"owning_store"`
	CanBeSource   bool   `json:"can_be_source"`
	HasOccurrence bool   `json:"has_occurrence"`
	HasOverlap    bool   `json:"has_overlap"`
	HasAmount     bool   `json:"has_amount"`
	refPattern    *regexp.Regexp
}

// LinkTypes is the one definition of what an entry can link to. The table and
// column names in it are the only ones the link SQL is built from.
var LinkTypes = map[string]LinkType{
	"calendar_events": {Name: "calendar_events", Table: "time_entry_calendar_events", RefColumn: "calendar_event_id", OwningStore: "calendar-store",
		CanBeSource: true, HasOccurrence: true, refPattern: regexp.MustCompile(`^calendar_event_\d{6,}$`)},
	"discord_events": {Name: "discord_events", Table: "time_entry_discord_events", RefColumn: "discord_event_id", OwningStore: "discord-signup-store",
		CanBeSource: true, HasOccurrence: true, refPattern: regexp.MustCompile(`^discord_event_\d{6,}$`)},
	"event_store_events": {Name: "event_store_events", Table: "time_entry_event_store_events", RefColumn: "event_store_event_id", OwningStore: "event-store",
		CanBeSource: true, refPattern: regexp.MustCompile(`^[1-9]\d*$`)},
	"chat_messages": {Name: "chat_messages", Table: "time_entry_chat_messages", RefColumn: "chat_message_id", OwningStore: "message-store",
		CanBeSource: true, refPattern: regexp.MustCompile(`^chat_message_\d{6,}$`)},
	"trips": {Name: "trips", Table: "time_entry_trips", RefColumn: "trip_id", OwningStore: "location-store",
		CanBeSource: true, HasOverlap: true, refPattern: regexp.MustCompile(`^trip_\d{6,}$`)},
	"visits": {Name: "visits", Table: "time_entry_visits", RefColumn: "visit_id", OwningStore: "location-store",
		HasOverlap: true, refPattern: regexp.MustCompile(`^visit_\d{6,}$`)},
	"photos": {Name: "photos", Table: "time_entry_photos", RefColumn: "photo_id", OwningStore: "photo-store",
		refPattern: regexp.MustCompile(`^photo_\d{6,}$`)},
	"transactions": {Name: "transactions", Table: "time_entry_transactions", RefColumn: "transaction_id", OwningStore: "budget-store",
		HasAmount: true, refPattern: regexp.MustCompile(`^transaction_\d{6,}$`)},
	"discord_attendances": {Name: "discord_attendances", Table: "time_entry_discord_attendances", RefColumn: "discord_attendance_id", OwningStore: "discord-signup-store",
		refPattern: regexp.MustCompile(`^discord_attendance_\d{6,}$`)},
	"activities": {Name: "activities", Table: "time_entry_activities", RefColumn: "activity_id", OwningStore: "activity-store",
		refPattern: regexp.MustCompile(`^activity_\d{6,}$`)},
}

// LinkTypeNames lists LinkTypes' names in order.
func LinkTypeNames() []string {
	names := make([]string, 0, len(LinkTypes))
	for name := range LinkTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// What a link is to its entry.
const (
	BearsOnSource  = "source"
	BearsOnDone    = "done"
	BearsOnSkipped = "skipped"
	BearsOnCost    = "cost"
	BearsOnContext = "context"
)

// BearsOn is GET /vocabularies' bears_on.
var BearsOn = []string{BearsOnSource, BearsOnDone, BearsOnSkipped, BearsOnCost, BearsOnContext}

// Who did something: the operator, a model, or a program following fixed
// rules. A value set by one kind is changed only by the same kind or a higher
// one, so a rerun of an importer never undoes what a model or the operator
// decided, and a model never undoes the operator.
const (
	ActorOperator = "operator"
	ActorModel    = "model"
	ActorProgram  = "program"
)

// ActorKinds is GET /vocabularies' actor_kinds, lowest rank first.
var ActorKinds = []string{ActorProgram, ActorModel, ActorOperator}

var actorRank = map[string]int{ActorProgram: 1, ActorModel: 2, ActorOperator: 3}

// Outcomes of a time entry.
const (
	OutcomeDone    = "done"
	OutcomeSkipped = "skipped"
	OutcomeUnknown = "unknown"
)

// Outcomes is GET /vocabularies' outcomes.
var Outcomes = []string{OutcomeDone, OutcomeSkipped, OutcomeUnknown}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
