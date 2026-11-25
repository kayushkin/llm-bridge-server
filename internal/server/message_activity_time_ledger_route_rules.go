package server

import (
	"fmt"
	"sort"
)

// The rules and descriptions of the message-store, activity-store and
// time-ledger-store routes (message_activity_time_ledger_routes.go). They are
// added to routeAccessRules, routeDescriptions and the README's group order
// at start, so the shared tables gain them in one place. Each store's routes
// share one rule: the store narrows every read and write to the caller's own
// records, so any principal may call it.

var messageStoreRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "message-store as the caller's principal: every conversation, message and day has an owner, and the store narrows every read and write to the caller's own, or every owner's for an administrator and for the internal service acting as itself"}

var activityStoreRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "activity-store as the caller's principal: every activity has an owner, and the store narrows every read and write to the caller's own activities, or every owner's for an administrator and for the internal service acting as itself"}

var timeLedgerStoreRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "time-ledger-store as the caller's principal: every time entry, kind and review has an owner, and the store narrows every read and write to the caller's own, or every owner's for an administrator and for the internal service acting as itself"}

const (
	routeGroupMessageStore    routeGroup = "message-store, mounted in this process"
	routeGroupActivityStore   routeGroup = "activity-store, mounted in this process"
	routeGroupTimeLedgerStore routeGroup = "time-ledger-store, mounted in this process"
)

// messageStoreRouteSummaries is every message-store route and what it does.
var messageStoreRouteSummaries = map[string]string{
	"GET /message-store/health":                                     "`{\"status\":\"ok\"}` when the database answers",
	"GET /message-store/vocabulary":                                 "Platforms, message kinds, account kinds, merge rules, day involvements, activity groupings and intervals",
	"GET /message-store/summary":                                    "The caller's conversations and messages per platform, with first and last times",
	"GET /message-store/messages":                                   "The caller's messages in time order by `conversation`, `person`, `platform`, `source`, `source_ref`, `after`, `before`, `q`, `order`, `limit` and `cursor`",
	"GET /message-store/messages/{id}":                              "One message; a superseded id is a 301 to the message that holds it now",
	"GET /message-store/messages/{id}/revisions":                    "Every earlier version of a message, oldest first",
	"GET /message-store/superseded-messages":                        "Retired message rows, newest first, by `superseded_by` and `limit`",
	"GET /message-store/accounts":                                   "The caller's accounts on platforms, its own first, then its bots",
	"POST /message-store/accounts":                                  "The caller's account with that platform and handle, made if new",
	"GET /message-store/accounts/{id}":                              "One account",
	"GET /message-store/conversations":                              "The caller's conversations, most recently active first, by `account`, `person`, `platform`, `q` and `limit`",
	"GET /message-store/conversations/{id}":                         "One conversation with participants and a message summary; a merged id is a 301 to the one it merged into",
	"GET /message-store/conversation-merges":                        "Merged conversations, newest first, by `merged_into` and `limit`",
	"PATCH /message-store/conversations/{id}/participants/{handle}": "Name a participant's people-store person, or clear it",
	"GET /message-store/conversation-days":                          "Conversation days by `conversation`, `journal_entry`, `person`, `from`, `to`, `involved` and `limit`",
	"GET /message-store/conversation-days/{id}":                     "One conversation day, its conversation and its messages",
	"POST /message-store/conversation-days/journal-entries":         "Record which journal entry held each day's transcript",
	"GET /message-store/activity":                                   "Message counts per time bucket, by `group_by`, `interval`, `time_zone`, `person`, `after`, `before` and `involved`",
	"GET /message-store/imports":                                    "The caller's import runs",
	"POST /message-store/imports":                                   "Start an import run owned by the caller",
	"GET /message-store/imports/{id}":                               "One import run and what it wrote",
	"POST /message-store/imports/{id}/batches":                      "Write conversations, messages, days, supersessions and merges in one transaction",
	"POST /message-store/imports/{id}/finish":                       "End an import run",
}

// activityStoreRouteSummaries is every activity-store route and what it does.
var activityStoreRouteSummaries = map[string]string{
	"GET /activity-store/health":                    "`{status, counts}`, counted over the caller's own activities",
	"GET /activity-store/vocabularies":              "Actions, action phrases, sources, import kinds and revision reasons",
	"GET /activity-store/stats":                     "The caller's live activities by product, source and action",
	"GET /activity-store/activities":                "The caller's activities by `from`, `to`, `product`, `action`, `source`, `q`, `include_deleted`, `order`, `limit` and `offset`",
	"GET /activity-store/activities/{id}":           "One activity with its sightings and attachments",
	"DELETE /activity-store/activities/{id}":        "Delete an activity, reversibly; 204",
	"POST /activity-store/activities/{id}/restore":  "Undo a delete; a purged activity is 409",
	"POST /activity-store/activities/{id}/purge":    "Blank an activity's words and destroy its attachments, keeping the row so no import brings it back",
	"GET /activity-store/activities/{id}/revisions": "An activity as it was before each delete, restore and purge, newest first",
	"GET /activity-store/attachments/{id}":          "An attachment's bytes, kept in file-store; a purged one is 410",
	"GET /activity-store/days/{date}":               "One local day (`tz` required) summarized by product",
	"GET /activity-store/imports":                   "The caller's imports, newest first",
	"POST /activity-store/imports":                  "Start an import owned by the caller, or answer the caller's one with that hash",
	"GET /activity-store/imports/{id}":              "One import",
	"POST /activity-store/imports/{id}/batches":     "Write activities in one transaction, folding what the caller already holds",
	"POST /activity-store/imports/{id}/finish":      "End an import",
}

// timeLedgerStoreRouteSummaries is every time-ledger-store route and what it
// does.
var timeLedgerStoreRouteSummaries = map[string]string{
	"GET /time-ledger-store/health":                                 "`{status, counts}`, counted over the caller's own entries",
	"GET /time-ledger-store/vocabularies":                           "Link types, bears-on values, actor kinds and outcomes",
	"GET /time-ledger-store/kinds":                                  "The caller's kinds, by name",
	"POST /time-ledger-store/kinds":                                 "Make a kind owned by the caller; 201",
	"GET /time-ledger-store/kinds/{id}":                             "One kind",
	"GET /time-ledger-store/time-entries":                           "The caller's entries by `from`, `to`, `ended_before`, `parent`, `kind_id`, `outcome`, `q`, `needs_review`, `include_deleted`, `limit` and `offset`, earliest first",
	"POST /time-ledger-store/time-entries":                          "Make an entry owned by the caller, with kinds and links, in one transaction; 201",
	"GET /time-ledger-store/time-entries/{id}":                      "One entry with its links, children and reviews",
	"PATCH /time-ledger-store/time-entries/{id}":                    "Change an entry's title, span, place or parent, by who may",
	"DELETE /time-ledger-store/time-entries/{id}":                   "Delete an entry, reversibly, keeping its links; 204",
	"GET /time-ledger-store/time-entries/{id}/revisions":            "An entry as it was before each change, newest first",
	"PUT /time-ledger-store/time-entries/{id}/kinds/{kind_id}":      "Give an entry a kind",
	"DELETE /time-ledger-store/time-entries/{id}/kinds/{kind_id}":   "Take a kind off an entry; 204",
	"POST /time-ledger-store/time-entries/{id}/links/{link_type}":   "Link a record of another store to an entry; 201",
	"DELETE /time-ledger-store/time-entries/{id}/links/{link_type}": "Take a link off an entry, keeping it as removed; 204",
	"GET /time-ledger-store/links/{link_type}":                      "The caller's entries' links to one record, by `ref`, `occurrence_starts_at` and `include_removed`",
	"GET /time-ledger-store/time-entries/{id}/reviews":              "An entry's reviews, newest first",
	"POST /time-ledger-store/time-entries/{id}/reviews":             "Judge whether an entry happened; 201",
	"GET /time-ledger-store/reviews/{id}":                           "One review",
}

// Each store's route patterns, as registerMessageActivityAndTimeLedgerRoutes
// mounts them.
var (
	messageStoreRoutePatterns    = sortedPatterns(messageStoreRouteSummaries)
	activityStoreRoutePatterns   = sortedPatterns(activityStoreRouteSummaries)
	timeLedgerStoreRoutePatterns = sortedPatterns(timeLedgerStoreRouteSummaries)
)

func sortedPatterns(summaries map[string]string) []string {
	patterns := make([]string, 0, len(summaries))
	for pattern := range summaries {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	return patterns
}

func init() {
	for _, store := range []struct {
		rule      routeAccessRule
		group     routeGroup
		summaries map[string]string
	}{
		{messageStoreRoute, routeGroupMessageStore, messageStoreRouteSummaries},
		{activityStoreRoute, routeGroupActivityStore, activityStoreRouteSummaries},
		{timeLedgerStoreRoute, routeGroupTimeLedgerStore, timeLedgerStoreRouteSummaries},
	} {
		for pattern, summary := range store.summaries {
			if _, taken := routeAccessRules[pattern]; taken {
				panic(fmt.Sprintf("route %s has two access rules", pattern))
			}
			routeAccessRules[pattern] = store.rule
			if _, taken := routeDescriptions[pattern]; taken {
				panic(fmt.Sprintf("route %s has two descriptions", pattern))
			}
			routeDescriptions[pattern] = routeDescription{store.group, summary}
		}
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupMessageStore, routeGroupActivityStore, routeGroupTimeLedgerStore)
}
