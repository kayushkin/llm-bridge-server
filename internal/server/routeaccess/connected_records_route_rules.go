package routeaccess

import "fmt"

// The rules and descriptions of the connected_records routes
// (connected_records_routes.go). They are kept here and added to
// RouteAccessRules, routeDescriptions and the README's group order at start,
// so the shared tables gain them in one place.

// peopleStoreRoute is the rule of every people-store route.
var peopleStoreRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "people-store as the caller's principal: every person has an owner, and the store narrows every read and write to the caller's own people, or every owner's for an administrator and for the internal service acting as itself"}

// calendarStoreRoute is the rule of every calendar-store route.
var calendarStoreRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "calendar-store as the caller's principal: every calendar has an owner, and the store narrows every read and write to the caller's own calendars, or every owner's for an administrator and for the internal service acting as itself"}

// journalStoreRoute is the rule of every journal-store route.
var journalStoreRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "journal-store as the caller's principal: every entry has an owner, and the store answers only the owner and an administrator; the internal service acting as nobody is refused, so the service token reaches it only beside the person it acts for"}

var connectedRecordsRouteAccessRules = map[string]RouteAccessRule{
	"GET /people/health":                      peopleStoreRoute,
	"GET /people/identity-kinds":              peopleStoreRoute,
	"GET /people/relationship-kinds":          peopleStoreRoute,
	"GET /people/link-sources":                peopleStoreRoute,
	"GET /people/persons":                     peopleStoreRoute,
	"POST /people/persons":                    peopleStoreRoute,
	"GET /people/persons/{id}":                peopleStoreRoute,
	"PATCH /people/persons/{id}":              peopleStoreRoute,
	"DELETE /people/persons/{id}":             peopleStoreRoute,
	"POST /people/persons/{id}/restore":       peopleStoreRoute,
	"GET /people/persons/{id}/revisions":      peopleStoreRoute,
	"POST /people/persons/{id}/identities":    peopleStoreRoute,
	"POST /people/persons/{id}/relationships": peopleStoreRoute,
	"GET /people/identities":                  peopleStoreRoute,
	"GET /people/identities/resolve":          peopleStoreRoute,
	"GET /people/refusals":                    peopleStoreRoute,
	"DELETE /people/identities/{id}":          peopleStoreRoute,
	"DELETE /people/relationships/{id}":       peopleStoreRoute,
	"GET /people/suggestions":                 peopleStoreRoute,
	"POST /people/suggestions":                peopleStoreRoute,
	"GET /people/suggestions/{id}":            peopleStoreRoute,
	"POST /people/suggestions/{id}/accept":    peopleStoreRoute,
	"POST /people/suggestions/{id}/reject":    peopleStoreRoute,
	"PUT /people/handle-activity":             peopleStoreRoute,
	"GET /people/reviews/pending":             peopleStoreRoute,
	"POST /people/reviews/claim":              peopleStoreRoute,
	"POST /people/reviews":                    peopleStoreRoute,

	"GET /calendar/health":                               calendarStoreRoute,
	"GET /calendar/vocabularies":                         calendarStoreRoute,
	"GET /calendar/calendars":                            calendarStoreRoute,
	"POST /calendar/calendars":                           calendarStoreRoute,
	"POST /calendar/calendars/{id}/imports":              calendarStoreRoute,
	"POST /calendar/calendars/{id}/google-event-imports": calendarStoreRoute,
	"GET /calendar/imports":                              calendarStoreRoute,
	"GET /calendar/events":                               calendarStoreRoute,
	"GET /calendar/events/{id}":                          calendarStoreRoute,
	"GET /calendar/events/{id}/revisions":                calendarStoreRoute,
	"POST /calendar/events/{id}/people":                  calendarStoreRoute,
	"DELETE /calendar/events/{id}/people/{person_id}":    calendarStoreRoute,

	"GET /journal/health":                        journalStoreRoute,
	"GET /journal/entry-kinds":                   journalStoreRoute,
	"GET /journal/visibilities":                  journalStoreRoute,
	"GET /journal/entries":                       journalStoreRoute,
	"POST /journal/entries":                      journalStoreRoute,
	"GET /journal/entries/{id}":                  journalStoreRoute,
	"PATCH /journal/entries/{id}":                journalStoreRoute,
	"DELETE /journal/entries/{id}":               journalStoreRoute,
	"POST /journal/entries/{id}/restore":         journalStoreRoute,
	"GET /journal/entries/{id}/revisions":        journalStoreRoute,
	"POST /journal/entries/{id}/promote-todo":    journalStoreRoute,
	"GET /journal/mentions":                      journalStoreRoute,
	"GET /journal/published":                     journalStoreRoute,
	"GET /journal/published/{slug}":              journalStoreRoute,
	"POST /journal/imports":                      journalStoreRoute,
	"GET /journal/time-precisions":               journalStoreRoute,
	"GET /journal/timelines/{id}/items":          journalStoreRoute,
	"POST /journal/timelines/{id}/items":         journalStoreRoute,
	"GET /journal/timeline-items":                journalStoreRoute,
	"GET /journal/timeline-items/{id}":           journalStoreRoute,
	"PATCH /journal/timeline-items/{id}":         journalStoreRoute,
	"DELETE /journal/timeline-items/{id}":        journalStoreRoute,
	"POST /journal/timeline-items/{id}/restore":  journalStoreRoute,
	"GET /journal/timeline-items/{id}/revisions": journalStoreRoute,
}

const (
	routeGroupPeopleStore   routeGroup = "people-store, mounted in this process"
	routeGroupCalendarStore routeGroup = "calendar-store, mounted in this process"
	routeGroupJournalStore  routeGroup = "journal-store, mounted in this process"
)

var connectedRecordsRouteDescriptions = map[string]routeDescription{
	"GET /people/health":                      {routeGroupPeopleStore, "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own people"},
	"GET /people/identity-kinds":              {routeGroupPeopleStore, "The handle kinds and how each is normalized"},
	"GET /people/relationship-kinds":          {routeGroupPeopleStore, "The relationship kinds and their inverses"},
	"GET /people/link-sources":                {routeGroupPeopleStore, "Who may link a handle, and whether each overrides refusals"},
	"GET /people/persons":                     {routeGroupPeopleStore, "The caller's people by `q`, `tag`, `principal_id`, `include_deleted`, `limit` and `offset`, by name"},
	"POST /people/persons":                    {routeGroupPeopleStore, "Make a person owned by the caller, with handles; 201"},
	"GET /people/persons/{id}":                {routeGroupPeopleStore, "One person with handles and relationships; a deleted one answers with `deleted_at`"},
	"PATCH /people/persons/{id}":              {routeGroupPeopleStore, "Change a person's name, birthday, how met, tags or principal"},
	"DELETE /people/persons/{id}":             {routeGroupPeopleStore, "Delete a person, reversibly"},
	"POST /people/persons/{id}/restore":       {routeGroupPeopleStore, "Undo a delete"},
	"GET /people/persons/{id}/revisions":      {routeGroupPeopleStore, "A person as they were before each change, oldest first"},
	"POST /people/persons/{id}/identities":    {routeGroupPeopleStore, "Link a handle to a person, saying who linked it and why; 201"},
	"POST /people/persons/{id}/relationships": {routeGroupPeopleStore, "Record how two of one owner's people are related; 201"},
	"GET /people/identities":                  {routeGroupPeopleStore, "Every handle of a `kind`, in the order linked"},
	"GET /people/identities/resolve":          {routeGroupPeopleStore, "The person holding a `kind` and `value`; 404 when nobody does"},
	"GET /people/refusals":                    {routeGroupPeopleStore, "Handles refused for a person, by `person_id`, `kind` and `value`"},
	"DELETE /people/identities/{id}":          {routeGroupPeopleStore, "Remove a handle, refusing it for that person"},
	"DELETE /people/relationships/{id}":       {routeGroupPeopleStore, "Remove a relationship"},
	"GET /people/suggestions":                 {routeGroupPeopleStore, "Guesses about who is who, by `status`, `q`, `sort`, `limit` and `offset`"},
	"POST /people/suggestions":                {routeGroupPeopleStore, "Propose that some handles are one person; 201, or 200 when none is new"},
	"GET /people/suggestions/{id}":            {routeGroupPeopleStore, "One suggestion"},
	"POST /people/suggestions/{id}/accept":    {routeGroupPeopleStore, "Accept a suggestion into a new person or `person_id`"},
	"POST /people/suggestions/{id}/reject":    {routeGroupPeopleStore, "Reject a suggestion, refusing its handles for the people it named"},
	"PUT /people/handle-activity":             {routeGroupPeopleStore, "Report how much the caller talks with each handle"},
	"GET /people/reviews/pending":             {routeGroupPeopleStore, "Handles the caller talks with that nobody has reviewed"},
	"POST /people/reviews/claim":              {routeGroupPeopleStore, "Take handles off the review queue for 30 minutes"},
	"POST /people/reviews":                    {routeGroupPeopleStore, "Mark handles reviewed"},

	"GET /calendar/health":                               {routeGroupCalendarStore, "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own calendars"},
	"GET /calendar/vocabularies":                         {routeGroupCalendarStore, "The link sources and revision reasons"},
	"GET /calendar/calendars":                            {routeGroupCalendarStore, "The caller's calendars, in the order made"},
	"POST /calendar/calendars":                           {routeGroupCalendarStore, "Make a calendar owned by the caller; 201, or 200 for the caller's one with that `source` and `source_ref`"},
	"POST /calendar/calendars/{id}/imports":              {routeGroupCalendarStore, "Import an .ics file into a calendar, in one transaction; 201"},
	"POST /calendar/calendars/{id}/google-event-imports": {routeGroupCalendarStore, "Import Google Calendar event resources into a calendar; 201"},
	"GET /calendar/imports":                              {routeGroupCalendarStore, "The caller's imports, newest first"},
	"GET /calendar/events":                               {routeGroupCalendarStore, "Events by `calendar_id`, `from`, `to`, `q`, `person_id`, `limit` and `offset`, oldest first"},
	"GET /calendar/events/{id}":                          {routeGroupCalendarStore, "One event, with its raw VEVENT"},
	"GET /calendar/events/{id}/revisions":                {routeGroupCalendarStore, "An event as it was before each import changed it, newest first"},
	"POST /calendar/events/{id}/people":                  {routeGroupCalendarStore, "Link one of the event owner's people to an event; 201"},
	"DELETE /calendar/events/{id}/people/{person_id}":    {routeGroupCalendarStore, "Unlink a person from an event"},

	"GET /journal/health":                        {routeGroupJournalStore, "`{\"status\":…,\"counts\":…}`, counted over the caller's own entries"},
	"GET /journal/entry-kinds":                   {routeGroupJournalStore, "The entry kinds"},
	"GET /journal/visibilities":                  {routeGroupJournalStore, "The visibilities"},
	"GET /journal/entries":                       {routeGroupJournalStore, "The caller's entries by `q`, `kind`, `tag`, `visibility`, `about_ref`, `source`, `from`, `to`, `limit` and `offset`, newest first"},
	"POST /journal/entries":                      {routeGroupJournalStore, "Write an entry owned by the caller; 201"},
	"GET /journal/entries/{id}":                  {routeGroupJournalStore, "One entry with its mentions"},
	"PATCH /journal/entries/{id}":                {routeGroupJournalStore, "Change an entry"},
	"DELETE /journal/entries/{id}":               {routeGroupJournalStore, "Delete an entry, reversibly; 204"},
	"POST /journal/entries/{id}/restore":         {routeGroupJournalStore, "Undo a delete"},
	"GET /journal/entries/{id}/revisions":        {routeGroupJournalStore, "An entry as it was before each change, newest first"},
	"POST /journal/entries/{id}/promote-todo":    {routeGroupJournalStore, "Make a checklist `line` a noteboard todo; an administrator's own entries only"},
	"GET /journal/mentions":                      {routeGroupJournalStore, "The caller's entries that mention `ref`"},
	"GET /journal/published":                     {routeGroupJournalStore, "The caller's published entries, newest first"},
	"GET /journal/published/{slug}":              {routeGroupJournalStore, "One of the caller's published entries by slug"},
	"POST /journal/imports":                      {routeGroupJournalStore, "Upsert entries by `source` and `source_ref`, in one transaction; `dry_run=true` writes nothing"},
	"GET /journal/time-precisions":               {routeGroupJournalStore, "The precisions a timeline item's time takes"},
	"GET /journal/timelines/{id}/items":          {routeGroupJournalStore, "A timeline's items, oldest first"},
	"POST /journal/timelines/{id}/items":         {routeGroupJournalStore, "Add items to a timeline, in one transaction; 201"},
	"GET /journal/timeline-items":                {routeGroupJournalStore, "Items of every `timeline` named, merged in time order"},
	"GET /journal/timeline-items/{id}":           {routeGroupJournalStore, "One timeline item"},
	"PATCH /journal/timeline-items/{id}":         {routeGroupJournalStore, "Change a timeline item"},
	"DELETE /journal/timeline-items/{id}":        {routeGroupJournalStore, "Delete a timeline item, reversibly; 204"},
	"POST /journal/timeline-items/{id}/restore":  {routeGroupJournalStore, "Undo a delete"},
	"GET /journal/timeline-items/{id}/revisions": {routeGroupJournalStore, "A timeline item as it was before each change, newest first"},
}

func init() {
	for pattern, rule := range connectedRecordsRouteAccessRules {
		if _, taken := RouteAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		RouteAccessRules[pattern] = rule
	}
	for pattern, description := range connectedRecordsRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupPeopleStore, routeGroupCalendarStore, routeGroupJournalStore)
}
