package server

import "fmt"

// The rules and descriptions of the connected_records routes
// (connected_records_routes.go). They are kept here and added to
// routeAccessRules, routeDescriptions and the README's group order at start,
// so the shared tables gain them in one place.

// peopleStoreRoute is the rule of every people-store route.
var peopleStoreRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "people-store as the caller's principal: every person has an owner, and the store narrows every read and write to the caller's own people, or every owner's for an administrator and for the internal service acting as itself"}

// calendarStoreRoute is the rule of every calendar-store route.
var calendarStoreRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "calendar-store as the caller's principal: every calendar has an owner, and the store narrows every read and write to the caller's own calendars, or every owner's for an administrator and for the internal service acting as itself"}

// journalStoreRoute is the rule of every journal-store route.
var journalStoreRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "journal-store as the caller's principal: every entry has an owner, and the store answers only the owner and an administrator; the internal service acting as nobody is refused, so the service token reaches it only beside the person it acts for"}

var connectedRecordsRouteAccessRules = map[string]routeAccessRule{
	"GET /people-store/health":                      peopleStoreRoute,
	"GET /people-store/identity-kinds":              peopleStoreRoute,
	"GET /people-store/relationship-kinds":          peopleStoreRoute,
	"GET /people-store/link-sources":                peopleStoreRoute,
	"GET /people-store/persons":                     peopleStoreRoute,
	"POST /people-store/persons":                    peopleStoreRoute,
	"GET /people-store/persons/{id}":                peopleStoreRoute,
	"PATCH /people-store/persons/{id}":              peopleStoreRoute,
	"DELETE /people-store/persons/{id}":             peopleStoreRoute,
	"POST /people-store/persons/{id}/restore":       peopleStoreRoute,
	"GET /people-store/persons/{id}/revisions":      peopleStoreRoute,
	"POST /people-store/persons/{id}/identities":    peopleStoreRoute,
	"POST /people-store/persons/{id}/relationships": peopleStoreRoute,
	"GET /people-store/identities":                  peopleStoreRoute,
	"GET /people-store/identities/resolve":          peopleStoreRoute,
	"GET /people-store/refusals":                    peopleStoreRoute,
	"DELETE /people-store/identities/{id}":          peopleStoreRoute,
	"DELETE /people-store/relationships/{id}":       peopleStoreRoute,
	"GET /people-store/suggestions":                 peopleStoreRoute,
	"POST /people-store/suggestions":                peopleStoreRoute,
	"GET /people-store/suggestions/{id}":            peopleStoreRoute,
	"POST /people-store/suggestions/{id}/accept":    peopleStoreRoute,
	"POST /people-store/suggestions/{id}/reject":    peopleStoreRoute,
	"PUT /people-store/handle-activity":             peopleStoreRoute,
	"GET /people-store/reviews/pending":             peopleStoreRoute,
	"POST /people-store/reviews/claim":              peopleStoreRoute,
	"POST /people-store/reviews":                    peopleStoreRoute,

	"GET /calendar-store/health":                               calendarStoreRoute,
	"GET /calendar-store/vocabularies":                         calendarStoreRoute,
	"GET /calendar-store/calendars":                            calendarStoreRoute,
	"POST /calendar-store/calendars":                           calendarStoreRoute,
	"POST /calendar-store/calendars/{id}/imports":              calendarStoreRoute,
	"POST /calendar-store/calendars/{id}/google-event-imports": calendarStoreRoute,
	"GET /calendar-store/imports":                              calendarStoreRoute,
	"GET /calendar-store/events":                               calendarStoreRoute,
	"GET /calendar-store/events/{id}":                          calendarStoreRoute,
	"GET /calendar-store/events/{id}/revisions":                calendarStoreRoute,
	"POST /calendar-store/events/{id}/people":                  calendarStoreRoute,
	"DELETE /calendar-store/events/{id}/people/{person_id}":    calendarStoreRoute,

	"GET /journal-store/health":                        journalStoreRoute,
	"GET /journal-store/entry-kinds":                   journalStoreRoute,
	"GET /journal-store/visibilities":                  journalStoreRoute,
	"GET /journal-store/entries":                       journalStoreRoute,
	"POST /journal-store/entries":                      journalStoreRoute,
	"GET /journal-store/entries/{id}":                  journalStoreRoute,
	"PATCH /journal-store/entries/{id}":                journalStoreRoute,
	"DELETE /journal-store/entries/{id}":               journalStoreRoute,
	"POST /journal-store/entries/{id}/restore":         journalStoreRoute,
	"GET /journal-store/entries/{id}/revisions":        journalStoreRoute,
	"POST /journal-store/entries/{id}/promote-todo":    journalStoreRoute,
	"GET /journal-store/mentions":                      journalStoreRoute,
	"GET /journal-store/published":                     journalStoreRoute,
	"GET /journal-store/published/{slug}":              journalStoreRoute,
	"POST /journal-store/imports":                      journalStoreRoute,
	"GET /journal-store/time-precisions":               journalStoreRoute,
	"GET /journal-store/timelines/{id}/items":          journalStoreRoute,
	"POST /journal-store/timelines/{id}/items":         journalStoreRoute,
	"GET /journal-store/timeline-items":                journalStoreRoute,
	"GET /journal-store/timeline-items/{id}":           journalStoreRoute,
	"PATCH /journal-store/timeline-items/{id}":         journalStoreRoute,
	"DELETE /journal-store/timeline-items/{id}":        journalStoreRoute,
	"POST /journal-store/timeline-items/{id}/restore":  journalStoreRoute,
	"GET /journal-store/timeline-items/{id}/revisions": journalStoreRoute,
}

const (
	routeGroupPeopleStore   routeGroup = "people-store, mounted in this process"
	routeGroupCalendarStore routeGroup = "calendar-store, mounted in this process"
	routeGroupJournalStore  routeGroup = "journal-store, mounted in this process"
)

var connectedRecordsRouteDescriptions = map[string]routeDescription{
	"GET /people-store/health":                      {routeGroupPeopleStore, "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own people"},
	"GET /people-store/identity-kinds":              {routeGroupPeopleStore, "The handle kinds and how each is normalized"},
	"GET /people-store/relationship-kinds":          {routeGroupPeopleStore, "The relationship kinds and their inverses"},
	"GET /people-store/link-sources":                {routeGroupPeopleStore, "Who may link a handle, and whether each overrides refusals"},
	"GET /people-store/persons":                     {routeGroupPeopleStore, "The caller's people by `q`, `tag`, `principal_id`, `include_deleted`, `limit` and `offset`, by name"},
	"POST /people-store/persons":                    {routeGroupPeopleStore, "Make a person owned by the caller, with handles; 201"},
	"GET /people-store/persons/{id}":                {routeGroupPeopleStore, "One person with handles and relationships; a deleted one answers with `deleted_at`"},
	"PATCH /people-store/persons/{id}":              {routeGroupPeopleStore, "Change a person's name, birthday, how met, tags or principal"},
	"DELETE /people-store/persons/{id}":             {routeGroupPeopleStore, "Delete a person, reversibly"},
	"POST /people-store/persons/{id}/restore":       {routeGroupPeopleStore, "Undo a delete"},
	"GET /people-store/persons/{id}/revisions":      {routeGroupPeopleStore, "A person as they were before each change, oldest first"},
	"POST /people-store/persons/{id}/identities":    {routeGroupPeopleStore, "Link a handle to a person, saying who linked it and why; 201"},
	"POST /people-store/persons/{id}/relationships": {routeGroupPeopleStore, "Record how two of one owner's people are related; 201"},
	"GET /people-store/identities":                  {routeGroupPeopleStore, "Every handle of a `kind`, in the order linked"},
	"GET /people-store/identities/resolve":          {routeGroupPeopleStore, "The person holding a `kind` and `value`; 404 when nobody does"},
	"GET /people-store/refusals":                    {routeGroupPeopleStore, "Handles refused for a person, by `person_id`, `kind` and `value`"},
	"DELETE /people-store/identities/{id}":          {routeGroupPeopleStore, "Remove a handle, refusing it for that person"},
	"DELETE /people-store/relationships/{id}":       {routeGroupPeopleStore, "Remove a relationship"},
	"GET /people-store/suggestions":                 {routeGroupPeopleStore, "Guesses about who is who, by `status`, `q`, `sort`, `limit` and `offset`"},
	"POST /people-store/suggestions":                {routeGroupPeopleStore, "Propose that some handles are one person; 201, or 200 when none is new"},
	"GET /people-store/suggestions/{id}":            {routeGroupPeopleStore, "One suggestion"},
	"POST /people-store/suggestions/{id}/accept":    {routeGroupPeopleStore, "Accept a suggestion into a new person or `person_id`"},
	"POST /people-store/suggestions/{id}/reject":    {routeGroupPeopleStore, "Reject a suggestion, refusing its handles for the people it named"},
	"PUT /people-store/handle-activity":             {routeGroupPeopleStore, "Report how much the caller talks with each handle"},
	"GET /people-store/reviews/pending":             {routeGroupPeopleStore, "Handles the caller talks with that nobody has reviewed"},
	"POST /people-store/reviews/claim":              {routeGroupPeopleStore, "Take handles off the review queue for 30 minutes"},
	"POST /people-store/reviews":                    {routeGroupPeopleStore, "Mark handles reviewed"},

	"GET /calendar-store/health":                               {routeGroupCalendarStore, "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own calendars"},
	"GET /calendar-store/vocabularies":                         {routeGroupCalendarStore, "The link sources and revision reasons"},
	"GET /calendar-store/calendars":                            {routeGroupCalendarStore, "The caller's calendars, in the order made"},
	"POST /calendar-store/calendars":                           {routeGroupCalendarStore, "Make a calendar owned by the caller; 201, or 200 for the caller's one with that `source` and `source_ref`"},
	"POST /calendar-store/calendars/{id}/imports":              {routeGroupCalendarStore, "Import an .ics file into a calendar, in one transaction; 201"},
	"POST /calendar-store/calendars/{id}/google-event-imports": {routeGroupCalendarStore, "Import Google Calendar event resources into a calendar; 201"},
	"GET /calendar-store/imports":                              {routeGroupCalendarStore, "The caller's imports, newest first"},
	"GET /calendar-store/events":                               {routeGroupCalendarStore, "Events by `calendar_id`, `from`, `to`, `q`, `person_id`, `limit` and `offset`, oldest first"},
	"GET /calendar-store/events/{id}":                          {routeGroupCalendarStore, "One event, with its raw VEVENT"},
	"GET /calendar-store/events/{id}/revisions":                {routeGroupCalendarStore, "An event as it was before each import changed it, newest first"},
	"POST /calendar-store/events/{id}/people":                  {routeGroupCalendarStore, "Link one of the event owner's people to an event; 201"},
	"DELETE /calendar-store/events/{id}/people/{person_id}":    {routeGroupCalendarStore, "Unlink a person from an event"},

	"GET /journal-store/health":                        {routeGroupJournalStore, "`{\"status\":…,\"counts\":…}`, counted over the caller's own entries"},
	"GET /journal-store/entry-kinds":                   {routeGroupJournalStore, "The entry kinds"},
	"GET /journal-store/visibilities":                  {routeGroupJournalStore, "The visibilities"},
	"GET /journal-store/entries":                       {routeGroupJournalStore, "The caller's entries by `q`, `kind`, `tag`, `visibility`, `about_ref`, `source`, `from`, `to`, `limit` and `offset`, newest first"},
	"POST /journal-store/entries":                      {routeGroupJournalStore, "Write an entry owned by the caller; 201"},
	"GET /journal-store/entries/{id}":                  {routeGroupJournalStore, "One entry with its mentions"},
	"PATCH /journal-store/entries/{id}":                {routeGroupJournalStore, "Change an entry"},
	"DELETE /journal-store/entries/{id}":               {routeGroupJournalStore, "Delete an entry, reversibly; 204"},
	"POST /journal-store/entries/{id}/restore":         {routeGroupJournalStore, "Undo a delete"},
	"GET /journal-store/entries/{id}/revisions":        {routeGroupJournalStore, "An entry as it was before each change, newest first"},
	"POST /journal-store/entries/{id}/promote-todo":    {routeGroupJournalStore, "Make a checklist `line` a noteboard todo; an administrator's own entries only"},
	"GET /journal-store/mentions":                      {routeGroupJournalStore, "The caller's entries that mention `ref`"},
	"GET /journal-store/published":                     {routeGroupJournalStore, "The caller's published entries, newest first"},
	"GET /journal-store/published/{slug}":              {routeGroupJournalStore, "One of the caller's published entries by slug"},
	"POST /journal-store/imports":                      {routeGroupJournalStore, "Upsert entries by `source` and `source_ref`, in one transaction; `dry_run=true` writes nothing"},
	"GET /journal-store/time-precisions":               {routeGroupJournalStore, "The precisions a timeline item's time takes"},
	"GET /journal-store/timelines/{id}/items":          {routeGroupJournalStore, "A timeline's items, oldest first"},
	"POST /journal-store/timelines/{id}/items":         {routeGroupJournalStore, "Add items to a timeline, in one transaction; 201"},
	"GET /journal-store/timeline-items":                {routeGroupJournalStore, "Items of every `timeline` named, merged in time order"},
	"GET /journal-store/timeline-items/{id}":           {routeGroupJournalStore, "One timeline item"},
	"PATCH /journal-store/timeline-items/{id}":         {routeGroupJournalStore, "Change a timeline item"},
	"DELETE /journal-store/timeline-items/{id}":        {routeGroupJournalStore, "Delete a timeline item, reversibly; 204"},
	"POST /journal-store/timeline-items/{id}/restore":  {routeGroupJournalStore, "Undo a delete"},
	"GET /journal-store/timeline-items/{id}/revisions": {routeGroupJournalStore, "A timeline item as it was before each change, newest first"},
}

func init() {
	for pattern, rule := range connectedRecordsRouteAccessRules {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = rule
	}
	for pattern, description := range connectedRecordsRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupPeopleStore, routeGroupCalendarStore, routeGroupJournalStore)
}
