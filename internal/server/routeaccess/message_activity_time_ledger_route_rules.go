package routeaccess

import (
	"fmt"
	"sort"
)

// The rules and descriptions of the message-store, activity-store and
// time-ledger-store routes (message_activity_time_ledger_routes.go). They are
// added to RouteAccessRules, routeDescriptions and the README's group order
// at start, so the shared tables gain them in one place. Each store's routes
// share one rule: the store narrows every read and write to the caller's own
// records, so any principal may call it.

var messageStoreRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "message-store as the caller's principal: every conversation, message and day has an owner, and the store narrows every read and write to the caller's own, or every owner's for an administrator and for the internal service acting as itself"}

var activityStoreRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "activity-store as the caller's principal: every activity has an owner, and the store narrows every read and write to the caller's own activities, or every owner's for an administrator and for the internal service acting as itself"}

var timeLedgerStoreRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "time-ledger-store as the caller's principal: every time entry, kind and review has an owner, and the store narrows every read and write to the caller's own, or every owner's for an administrator and for the internal service acting as itself"}

const (
	routeGroupMessageStore    routeGroup = "message-store, mounted in this process"
	routeGroupActivityStore   routeGroup = "activity-store, mounted in this process"
	routeGroupTimeLedgerStore routeGroup = "time-ledger-store, mounted in this process"
)

// messageStoreRouteSummaries is every message-store route and what it does.
var messageStoreRouteSummaries = map[string]string{
	"GET /messages/health":                                     "`{\"status\":\"ok\"}` when the database answers",
	"GET /messages/vocabulary":                                 "Platforms, message kinds, account kinds, merge rules, day involvements, activity groupings and intervals",
	"GET /messages/summary":                                    "The caller's conversations and messages per platform, with first and last times",
	"GET /messages/messages":                                   "The caller's messages in time order by `conversation`, `person`, `platform`, `source`, `source_ref`, `after`, `before`, `q`, `order`, `limit` and `cursor`",
	"GET /messages/messages/{id}":                              "One message; a superseded id is a 301 to the message that holds it now",
	"GET /messages/messages/{id}/revisions":                    "Every earlier version of a message, oldest first",
	"GET /messages/superseded-messages":                        "Retired message rows, newest first, by `superseded_by` and `limit`",
	"GET /messages/accounts":                                   "The caller's accounts on platforms, its own first, then its bots",
	"POST /messages/accounts":                                  "The caller's account with that platform and handle, made if new",
	"GET /messages/accounts/{id}":                              "One account",
	"GET /messages/conversations":                              "The caller's conversations, most recently active first, by `account`, `person`, `platform`, `q` and `limit`",
	"GET /messages/conversations/{id}":                         "One conversation with participants and a message summary; a merged id is a 301 to the one it merged into",
	"GET /messages/conversation-merges":                        "Merged conversations, newest first, by `merged_into` and `limit`",
	"PATCH /messages/conversations/{id}/participants/{handle}": "Name a participant's people-store person, or clear it",
	"GET /messages/conversation-days":                          "Conversation days by `conversation`, `journal_entry`, `person`, `from`, `to`, `involved` and `limit`",
	"GET /messages/conversation-days/{id}":                     "One conversation day, its conversation and its messages",
	"POST /messages/conversation-days/journal-entries":         "Record which journal entry held each day's transcript",
	"GET /messages/activity":                                   "Message counts per time bucket, by `group_by`, `interval`, `time_zone`, `person`, `after`, `before` and `involved`",
	"GET /messages/imports":                                    "The caller's import runs",
	"POST /messages/imports":                                   "Start an import run owned by the caller",
	"GET /messages/imports/{id}":                               "One import run and what it wrote",
	"POST /messages/imports/{id}/batches":                      "Write conversations, messages, days, supersessions and merges in one transaction",
	"POST /messages/imports/{id}/finish":                       "End an import run",
}

// activityStoreRouteSummaries is every activity-store route and what it does.
var activityStoreRouteSummaries = map[string]string{
	"GET /online-activity/health":                    "`{status, counts}`, counted over the caller's own activities",
	"GET /online-activity/vocabularies":              "Actions, action phrases, sources, import kinds and revision reasons",
	"GET /online-activity/stats":                     "The caller's live activities by product, source and action",
	"GET /online-activity/activities":                "The caller's activities by `from`, `to`, `product`, `action`, `source`, `q`, `include_deleted`, `order`, `limit` and `offset`",
	"GET /online-activity/activities/{id}":           "One activity with its sightings and attachments",
	"DELETE /online-activity/activities/{id}":        "Delete an activity, reversibly; 204",
	"POST /online-activity/activities/{id}/restore":  "Undo a delete; a purged activity is 409",
	"POST /online-activity/activities/{id}/purge":    "Blank an activity's words and destroy its attachments, keeping the row so no import brings it back",
	"GET /online-activity/activities/{id}/revisions": "An activity as it was before each delete, restore and purge, newest first",
	"GET /online-activity/attachments/{id}":          "An attachment's bytes, kept in file-store; a purged one is 410",
	"GET /online-activity/days/{date}":               "One local day (`tz` required) summarized by product",
	"GET /online-activity/imports":                   "The caller's imports, newest first",
	"POST /online-activity/imports":                  "Start an import owned by the caller, or answer the caller's one with that hash",
	"GET /online-activity/imports/{id}":              "One import",
	"POST /online-activity/imports/{id}/batches":     "Write activities in one transaction, folding what the caller already holds",
	"POST /online-activity/imports/{id}/finish":      "End an import",
}

// timeLedgerStoreRouteSummaries is every time-ledger-store route and what it
// does.
var timeLedgerStoreRouteSummaries = map[string]string{
	"GET /time-ledger/health":                                 "`{status, counts}`, counted over the caller's own entries",
	"GET /time-ledger/vocabularies":                           "Link types, bears-on values, actor kinds and outcomes",
	"GET /time-ledger/kinds":                                  "The caller's kinds, by name",
	"POST /time-ledger/kinds":                                 "Make a kind owned by the caller; 201",
	"GET /time-ledger/kinds/{id}":                             "One kind",
	"GET /time-ledger/time-entries":                           "The caller's entries by `from`, `to`, `ended_before`, `parent`, `kind_id`, `outcome`, `q`, `needs_review`, `include_deleted`, `limit` and `offset`, earliest first",
	"POST /time-ledger/time-entries":                          "Make an entry owned by the caller, with kinds and links, in one transaction; 201",
	"GET /time-ledger/time-entries/{id}":                      "One entry with its links, children and reviews",
	"PATCH /time-ledger/time-entries/{id}":                    "Change an entry's title, span, place or parent, by who may",
	"DELETE /time-ledger/time-entries/{id}":                   "Delete an entry, reversibly, keeping its links; 204",
	"GET /time-ledger/time-entries/{id}/revisions":            "An entry as it was before each change, newest first",
	"PUT /time-ledger/time-entries/{id}/kinds/{kind_id}":      "Give an entry a kind",
	"DELETE /time-ledger/time-entries/{id}/kinds/{kind_id}":   "Take a kind off an entry; 204",
	"POST /time-ledger/time-entries/{id}/links/{link_type}":   "Link a record of another store to an entry; 201",
	"DELETE /time-ledger/time-entries/{id}/links/{link_type}": "Take a link off an entry, keeping it as removed; 204",
	"GET /time-ledger/links/{link_type}":                      "The caller's entries' links to one record, by `ref`, `occurrence_starts_at` and `include_removed`",
	"GET /time-ledger/time-entries/{id}/reviews":              "An entry's reviews, newest first",
	"POST /time-ledger/time-entries/{id}/reviews":             "Judge whether an entry happened; 201",
	"GET /time-ledger/reviews/{id}":                           "One review",
}

// Each store's route patterns, as registerMessageActivityAndTimeLedgerRoutes
// mounts them.
var (
	MessageStoreRoutePatterns    = sortedPatterns(messageStoreRouteSummaries)
	ActivityStoreRoutePatterns   = sortedPatterns(activityStoreRouteSummaries)
	TimeLedgerStoreRoutePatterns = sortedPatterns(timeLedgerStoreRouteSummaries)
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
		rule      RouteAccessRule
		group     routeGroup
		summaries map[string]string
	}{
		{messageStoreRoute, routeGroupMessageStore, messageStoreRouteSummaries},
		{activityStoreRoute, routeGroupActivityStore, activityStoreRouteSummaries},
		{timeLedgerStoreRoute, routeGroupTimeLedgerStore, timeLedgerStoreRouteSummaries},
	} {
		for pattern, summary := range store.summaries {
			if _, taken := RouteAccessRules[pattern]; taken {
				panic(fmt.Sprintf("route %s has two access rules", pattern))
			}
			RouteAccessRules[pattern] = store.rule
			if _, taken := routeDescriptions[pattern]; taken {
				panic(fmt.Sprintf("route %s has two descriptions", pattern))
			}
			routeDescriptions[pattern] = routeDescription{store.group, summary}
		}
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupMessageStore, routeGroupActivityStore, routeGroupTimeLedgerStore)
}
