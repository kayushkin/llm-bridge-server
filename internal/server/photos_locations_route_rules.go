package server

import "fmt"

// The rules and descriptions of the photo-store and location-store routes
// (photos_locations_routes.go). They are kept here and added to
// routeAccessRules, routeDescriptions and the README's group order at start,
// so the shared tables gain them in one place.

// photoStoreRouteRule is the rule of every photo-store route.
var photoStoreRouteRule = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "photo-store as the caller's principal: every photo, face and face group has an owner, and the store narrows every read and write to the caller's own, or every owner's for an administrator and for the internal service acting as itself"}

// locationStoreRouteRule is the rule of every location-store route.
var locationStoreRouteRule = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "location-store as the caller's principal: every place, visit, trip, point and import has an owner, and the store narrows every read and write to the caller's own, or every owner's for an administrator and for the internal service acting as itself; a pass over records acts for one owner"}

const (
	routeGroupPhotoStore    routeGroup = "photo-store, mounted in this process"
	routeGroupLocationStore routeGroup = "location-store, mounted in this process"
)

var photoStoreRouteDescriptions = map[string]routeDescription{
	"GET /photos/health":                     {routeGroupPhotoStore, "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own photos"},
	"GET /photos/date-sources":               {routeGroupPhotoStore, "The ways a photo's date is known, strongest first"},
	"GET /photos/tags":                       {routeGroupPhotoStore, "Every tag on the caller's live photos, with how many carry it"},
	"GET /photos/photos":                     {routeGroupPhotoStore, "The caller's live photos by `from`, `to`, `folder`, `q`, `tag`, `person`, `described`, `has_gps`, `sha256`, `exclude_tag`, `limit` and `offset`, oldest taken first"},
	"GET /photos/sightings":                  {routeGroupPhotoStore, "Every place an import found each of the caller's photos, by `source_machine`, `limit` and `offset`"},
	"GET /photos/taken-activity":             {routeGroupPhotoStore, "The caller's photos counted per day, week, month or year in `time_zone`, with sample ids"},
	"POST /photos/imports":                   {routeGroupPhotoStore, "Upsert photos owned by the caller by the original's sha256, thumbnails kept in file-store; description and tags are never touched"},
	"GET /photos/photos/{id}":                {routeGroupPhotoStore, "One photo; a deleted one only with `include_deleted=true`"},
	"PATCH /photos/photos/{id}":              {routeGroupPhotoStore, "Change a photo's description, tags or date"},
	"DELETE /photos/photos/{id}":             {routeGroupPhotoStore, "Delete a photo, reversibly; 204"},
	"POST /photos/photos/{id}/restore":       {routeGroupPhotoStore, "Undo a delete; a purged photo is 409"},
	"POST /photos/photos/{id}/purge":         {routeGroupPhotoStore, "Remove a picture that must not be kept: destroy its thumbnail and blank its description everywhere"},
	"GET /photos/photos/{id}/revisions":      {routeGroupPhotoStore, "A photo as it was before each change, newest first"},
	"GET /photos/photos/{id}/thumbnail":      {routeGroupPhotoStore, "The photo's JPEG thumbnail, from file-store; a purged photo is 410"},
	"GET /photos/face-group-statuses":        {routeGroupPhotoStore, "The statuses a face group may hold"},
	"GET /photos/face-groups":                {routeGroupPhotoStore, "The caller's face groups by `status`, `person_id`, `limit` and `offset`, the most photos first"},
	"POST /photos/face-groups":               {routeGroupPhotoStore, "Make an open group of one owner's faces; 201"},
	"GET /photos/face-groups/{id}":           {routeGroupPhotoStore, "One face group with every face in it"},
	"POST /photos/face-groups/{id}/members":  {routeGroupPhotoStore, "Add faces of the group's owner to an open group"},
	"POST /photos/face-groups/{id}/guesses":  {routeGroupPhotoStore, "Record who a model or the context thinks a group is; 201"},
	"POST /photos/face-groups/{id}/decision": {routeGroupPhotoStore, "A person saying who a group is, confirming or rejecting its faces as that person"},
	"POST /photos/face-scans":                {routeGroupPhotoStore, "Record what one face model found on one of the caller's photos; 201"},
	"GET /photos/face-scans/pending":         {routeGroupPhotoStore, "Ids of the caller's photos a `model` has not scanned, oldest first"},
	"GET /photos/faces":                      {routeGroupPhotoStore, "Faces on the caller's live photos in id order, by `photo_id`, `model`, `after`, `include_embeddings` and `limit`"},
	"GET /photos/photos/{id}/faces":          {routeGroupPhotoStore, "The faces found on one photo"},
	"GET /photos/faces/{id}/image":           {routeGroupPhotoStore, "One face cut from its photo's thumbnail, as a JPEG"},
	"GET /photos/photo-person-statuses":      {routeGroupPhotoStore, "The statuses a person on a photo may hold"},
	"GET /photos/photo-people":               {routeGroupPhotoStore, "Who is on which of the caller's photos, by `photo_id`, `person_id`, `status`, `limit` and `offset`"},
	"POST /photos/photo-people":              {routeGroupPhotoStore, "Propose or confirm a person of the photo owner's people on a photo; only ever moves a link upward"},
	"PATCH /photos/photo-people/{id}":        {routeGroupPhotoStore, "A person's decision on who is on a photo"},
	"GET /photos/people-counts":              {routeGroupPhotoStore, "How many of the caller's photos each person is confirmed and suggested on"},
}

var locationStoreRouteDescriptions = map[string]routeDescription{
	"GET /locations/health":                         {routeGroupLocationStore, "`{\"status\":\"ok\",\"counts\":…}`, counted over the caller's own records"},
	"GET /locations/vocabularies":                   {routeGroupLocationStore, "The kinds, sources, modes and reasons location-store knows"},
	"GET /locations/places":                         {routeGroupLocationStore, "The caller's places by `q`, `kind`, `tag` and `google_place_id`, oldest first"},
	"POST /locations/places":                        {routeGroupLocationStore, "Make a place owned by the caller; 201"},
	"GET /locations/places/{id}":                    {routeGroupLocationStore, "One place; a deleted one only with `include_deleted=true`"},
	"PATCH /locations/places/{id}":                  {routeGroupLocationStore, "Change a place's name, address, description or tags"},
	"POST /locations/places/{id}/merge":             {routeGroupLocationStore, "Merge this place into another of the same owner's (`into`), moving its visits, trips and links"},
	"DELETE /locations/places/{id}":                 {routeGroupLocationStore, "Delete a place, reversibly"},
	"POST /locations/places/{id}/restore":           {routeGroupLocationStore, "Undo a delete"},
	"GET /locations/places/{id}/revisions":          {routeGroupLocationStore, "A place as it was before each change"},
	"GET /locations/visits":                         {routeGroupLocationStore, "The caller's visits by `from`, `to`, `place_id`, `source` and `tag`, earliest first"},
	"POST /locations/visits":                        {routeGroupLocationStore, "Make a visit owned by the caller; 201"},
	"GET /locations/visits/{id}":                    {routeGroupLocationStore, "One visit"},
	"PATCH /locations/visits/{id}":                  {routeGroupLocationStore, "Change a visit's place, description or tags"},
	"DELETE /locations/visits/{id}":                 {routeGroupLocationStore, "Delete a visit, reversibly"},
	"POST /locations/visits/{id}/restore":           {routeGroupLocationStore, "Undo a delete"},
	"GET /locations/visits/{id}/revisions":          {routeGroupLocationStore, "A visit as it was before each change"},
	"GET /locations/trips":                          {routeGroupLocationStore, "The caller's trips by `from`, `to`, `mode`, `parent_trip_id`, `source`, `tag` and the rest of CONTRACT.md's filters, earliest first"},
	"POST /locations/trips":                         {routeGroupLocationStore, "Make a trip owned by the caller; 201"},
	"GET /locations/trips/{id}":                     {routeGroupLocationStore, "One trip"},
	"PATCH /locations/trips/{id}":                   {routeGroupLocationStore, "Change a trip's parent, places, mode, title, description or tags"},
	"GET /locations/trips/{id}/path":                {routeGroupLocationStore, "The points a trip went through, in time order"},
	"POST /locations/trips/{id}/children":           {routeGroupLocationStore, "Make trips of the same owner part of this one, all or none"},
	"DELETE /locations/trips/{id}":                  {routeGroupLocationStore, "Delete a trip, reversibly"},
	"POST /locations/trips/{id}/restore":            {routeGroupLocationStore, "Undo a delete"},
	"GET /locations/trips/{id}/revisions":           {routeGroupLocationStore, "A trip as it was before each change"},
	"POST /locations/localities/fill":               {routeGroupLocationStore, "Name the town of every empty locality in one owner's records, offline, from GeoNames"},
	"GET /locations/home-periods":                   {routeGroupLocationStore, "The caller's home periods, oldest first"},
	"GET /locations/home-periods/{id}":              {routeGroupLocationStore, "One home period"},
	"POST /locations/journeys/group":                {routeGroupLocationStore, "Group one owner's movements into home periods and journeys"},
	"GET /locations/journeys":                       {routeGroupLocationStore, "The caller's journeys"},
	"GET /locations/journeys/{id}":                  {routeGroupLocationStore, "One journey with its movements"},
	"POST /locations/calendar/match":                {routeGroupLocationStore, "Link the calendar events sent to one owner's visits, flights and journeys"},
	"GET /locations/calendar-event-links":           {routeGroupLocationStore, "The caller's links from calendar events to visits, flights and journeys"},
	"POST /locations/calendar-event-links":          {routeGroupLocationStore, "Link a calendar event to one of the caller's records by hand; 201"},
	"GET /locations/calendar-event-links/{id}":      {routeGroupLocationStore, "One calendar event link"},
	"DELETE /locations/calendar-event-links/{id}":   {routeGroupLocationStore, "Delete a calendar event link"},
	"GET /locations/calendar-series-places":         {routeGroupLocationStore, "What each recurring calendar series came to in the caller's records"},
	"GET /locations/calendar-series-places/{id}":    {routeGroupLocationStore, "One series place"},
	"GET /locations/unnamed-places":                 {routeGroupLocationStore, "The caller's places with no name, the most visited first"},
	"POST /locations/place-suggestions/proposals":   {routeGroupLocationStore, "Propose names for one owner's unnamed places"},
	"GET /locations/place-suggestions":              {routeGroupLocationStore, "The caller's place name suggestions by `status` and `place_id`"},
	"GET /locations/place-suggestions/{id}":         {routeGroupLocationStore, "One place name suggestion"},
	"POST /locations/place-suggestions/{id}/accept": {routeGroupLocationStore, "Accept a suggested name, naming the place"},
	"POST /locations/place-suggestions/{id}/reject": {routeGroupLocationStore, "Reject a suggested name"},
	"GET /locations/timeline":                       {routeGroupLocationStore, "The caller's visits and trips overlapping `from` to `to`, in start order"},
	"GET /locations/points":                         {routeGroupLocationStore, "The caller's raw location points by `from`, `to`, `source` and `source_ref`, earliest first"},
	"POST /locations/points/live":                   {routeGroupLocationStore, "A phone's fixes as it makes them, owned by the caller"},
	"POST /locations/phone-points/segment":          {routeGroupLocationStore, "Make one owner's live phone points into visits and movements"},
	"GET /locations/imports":                        {routeGroupLocationStore, "The caller's imports, newest first"},
	"POST /locations/imports":                       {routeGroupLocationStore, "Start an import owned by the caller, or answer the caller's one with that hash"},
	"GET /locations/imports/{id}":                   {routeGroupLocationStore, "One import"},
	"POST /locations/imports/{id}/batches":          {routeGroupLocationStore, "Add a batch of records to an import, in one transaction"},
	"POST /locations/imports/{id}/finish":           {routeGroupLocationStore, "Finish an import, applying a snapshot's span"},
}

func init() {
	for _, routes := range []struct {
		descriptions map[string]routeDescription
		rule         routeAccessRule
	}{{photoStoreRouteDescriptions, photoStoreRouteRule}, {locationStoreRouteDescriptions, locationStoreRouteRule}} {
		for pattern, description := range routes.descriptions {
			if _, taken := routeAccessRules[pattern]; taken {
				panic(fmt.Sprintf("route %s has two access rules", pattern))
			}
			routeAccessRules[pattern] = routes.rule
			if _, taken := routeDescriptions[pattern]; taken {
				panic(fmt.Sprintf("route %s has two descriptions", pattern))
			}
			routeDescriptions[pattern] = description
		}
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupPhotoStore, routeGroupLocationStore)
}
