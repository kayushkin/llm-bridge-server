package locationstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// RegisterHandlers mounts the API on mux. Routes are rooted at /; the server
// mounts them under its own prefix. Every request must carry the Caller the
// server verified (ownership.ContextWithCaller); one with none is 401. A
// read reaches the records in the caller's scope, a new record is the
// caller's, and a pass over a whole owner's records (localities, journeys,
// phone points, calendar matching, place suggestions) works through the one
// owner the request acts for.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /vocabularies", h.vocabularies)

	mux.HandleFunc("GET /places", h.listPlaces)
	mux.HandleFunc("POST /places", h.createPlace)
	mux.HandleFunc("GET /places/{id}", h.getPlace)
	mux.HandleFunc("PATCH /places/{id}", h.patchPlace)
	mux.HandleFunc("POST /places/{id}/merge", h.mergePlace)

	mux.HandleFunc("GET /visits", h.listVisits)
	mux.HandleFunc("POST /visits", h.createVisit)
	mux.HandleFunc("GET /visits/{id}", h.getVisit)
	mux.HandleFunc("PATCH /visits/{id}", h.patchVisit)

	mux.HandleFunc("GET /trips", h.listTrips)
	mux.HandleFunc("POST /trips", h.createTrip)
	mux.HandleFunc("GET /trips/{id}", h.getTrip)
	mux.HandleFunc("PATCH /trips/{id}", h.patchTrip)
	mux.HandleFunc("GET /trips/{id}/path", h.tripPath)
	mux.HandleFunc("POST /trips/{id}/children", h.attachChildren)

	for entityType, collection := range map[string]string{EntityPlace: "places", EntityVisit: "visits", EntityTrip: "trips"} {
		mux.HandleFunc("DELETE /"+collection+"/{id}", h.remove(entityType))
		mux.HandleFunc("POST /"+collection+"/{id}/restore", h.restore(entityType))
		mux.HandleFunc("GET /"+collection+"/{id}/revisions", h.revisions(entityType))
	}

	mux.HandleFunc("POST /localities/fill", h.fillLocalities)
	mux.HandleFunc("GET /home-periods", h.listHomePeriods)
	mux.HandleFunc("GET /home-periods/{id}", h.getHomePeriod)
	mux.HandleFunc("POST /journeys/group", h.groupJourneys)
	mux.HandleFunc("GET /journeys", h.listJourneys)
	mux.HandleFunc("GET /journeys/{id}", h.getJourney)

	mux.HandleFunc("POST /calendar/match", h.matchCalendar)
	mux.HandleFunc("GET /calendar-event-links", h.listCalendarEventLinks)
	mux.HandleFunc("POST /calendar-event-links", h.createCalendarEventLink)
	mux.HandleFunc("GET /calendar-event-links/{id}", h.getCalendarEventLink)
	mux.HandleFunc("DELETE /calendar-event-links/{id}", h.deleteCalendarEventLink)
	mux.HandleFunc("GET /calendar-series-places", h.listCalendarSeriesPlaces)
	mux.HandleFunc("GET /calendar-series-places/{id}", h.getCalendarSeriesPlace)

	mux.HandleFunc("GET /unnamed-places", h.listUnnamedPlaces)
	mux.HandleFunc("POST /place-suggestions/proposals", h.proposePlaceSuggestions)
	mux.HandleFunc("GET /place-suggestions", h.listPlaceSuggestions)
	mux.HandleFunc("GET /place-suggestions/{id}", h.getPlaceSuggestion)
	mux.HandleFunc("POST /place-suggestions/{id}/accept", h.decidePlaceSuggestion(true))
	mux.HandleFunc("POST /place-suggestions/{id}/reject", h.decidePlaceSuggestion(false))

	mux.HandleFunc("GET /timeline", h.timeline)
	mux.HandleFunc("GET /points", h.listPoints)
	mux.HandleFunc("POST /points/live", h.recordLivePoints)
	mux.HandleFunc("POST /phone-points/segment", h.segmentPhonePoints)

	mux.HandleFunc("GET /imports", h.listImports)
	mux.HandleFunc("POST /imports", h.startImport)
	mux.HandleFunc("GET /imports/{id}", h.getImport)
	mux.HandleFunc("POST /imports/{id}/batches", h.importBatch)
	mux.HandleFunc("POST /imports/{id}/finish", h.finishImport)
}

type handler struct {
	s *Store
}

// respondRefusal answers an ownership refusal in the store's error shape.
func respondRefusal(w http.ResponseWriter, refusal *ownership.RequestError) bool {
	if refusal == nil {
		return false
	}
	writeErr(w, refusal.Status, refusal.Message)
	return true
}

// readScope is whose records the request reaches, or false after answering
// the refusal.
func readScope(w http.ResponseWriter, r *http.Request) (ownership.Scope, bool) {
	scope, refusal := ownership.ReadScope(r)
	return scope, !respondRefusal(w, refusal)
}

// newRecordOwner is who owns what the request creates, and the scope the
// records it names are looked up in, or false after answering the refusal.
func newRecordOwner(w http.ResponseWriter, r *http.Request) (string, ownership.Scope, bool) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return "", ownership.Scope{}, false
	}
	scope, ok := readScope(w, r)
	return owner, scope, ok
}

// ownerActedFor is the one owner a pass over a whole owner's records works
// through, or false after answering the refusal.
func ownerActedFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, refusal := ownership.OwnerActedFor(r)
	return owner, !respondRefusal(w, refusal)
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	counts, err := h.s.Counts(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, Health{Status: "ok", Counts: counts})
}

func (h *handler) vocabularies(w http.ResponseWriter, r *http.Request) {
	if _, refusal := ownership.CallerOfRequest(r); respondRefusal(w, refusal) {
		return
	}
	writeJSON(w, http.StatusOK, Vocabularies{PlaceKinds: PlaceKinds, TripKinds: TripKinds, TripModes: TripModes, RecordSources: RecordSources,
		PointSources: PointSources, ImportKinds: ImportKinds, CalendarMatchRules: CalendarMatchRules, CalendarLinkTargetTypes: CalendarLinkTargetTypes,
		SeriesOutcomes: SeriesOutcomes, PlaceSuggestionStatuses: PlaceSuggestionStatuses, PlaceSuggestionSources: PlaceSuggestionSources})
}

// ---- query parameters ----

type queryReader struct {
	w      http.ResponseWriter
	r      *http.Request
	failed bool
}

func (q *queryReader) integer(name string) int64 {
	raw := strings.TrimSpace(q.r.URL.Query().Get(name))
	if raw == "" || q.failed {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		writeErr(q.w, http.StatusBadRequest, fmt.Sprintf("%s must be a non-negative integer (epoch seconds for from and to), got %q", name, raw))
		q.failed = true
		return 0
	}
	return value
}

func (q *queryReader) number(name string) float64 {
	raw := strings.TrimSpace(q.r.URL.Query().Get(name))
	if raw == "" || q.failed {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		writeErr(q.w, http.StatusBadRequest, fmt.Sprintf("%s must be a non-negative number, got %q", name, raw))
		q.failed = true
		return 0
	}
	return value
}

func (q *queryReader) boolean(name string) bool {
	raw := strings.TrimSpace(q.r.URL.Query().Get(name))
	if raw == "" || q.failed {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		writeErr(q.w, http.StatusBadRequest, fmt.Sprintf("%s must be true or false, got %q", name, raw))
		q.failed = true
	}
	return value
}

func (q *queryReader) text(name string) string { return strings.TrimSpace(q.r.URL.Query().Get(name)) }

func (q *queryReader) timeFilter() TimeFilter {
	return TimeFilter{From: q.integer("from"), To: q.integer("to"), Source: q.text("source"), Tag: q.text("tag"),
		IncludeDeleted: q.boolean("include_deleted"), Limit: int(q.integer("limit")), Offset: int(q.integer("offset"))}
}

// ---- bodies ----

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

// decodePatch reads a PATCH body into a map first, so a misspelt key is a 400
// and not a 200 that changed nothing. Only the nullable fields may be null,
// which clears them; the rest are unmarshalled into into.
func decodePatch(w http.ResponseWriter, r *http.Request, accepted, nullable []string, into any) (map[string]json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return nil, false
	}
	all := append(append([]string{}, accepted...), nullable...)
	sort.Strings(all)
	plain := map[string]json.RawMessage{}
	for field, value := range raw {
		switch {
		case knownWord(nullable, field):
		case knownWord(accepted, field):
			if string(value) == "null" {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is null; send a value, or leave the field out to keep it", field))
				return nil, false
			}
			plain[field] = value
		default:
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("field %q cannot be patched: PATCH accepts %s", field, strings.Join(all, ", ")))
			return nil, false
		}
	}
	body, _ := json.Marshal(plain)
	if err := json.Unmarshal(body, into); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return nil, false
	}
	return raw, true
}

// optionalID reads a nullable id field of a PATCH body.
func optionalID(w http.ResponseWriter, raw map[string]json.RawMessage, field string) (OptionalID, bool) {
	value, present := raw[field]
	if !present {
		return OptionalID{}, true
	}
	if string(value) == "null" {
		return OptionalID{Set: true}, true
	}
	var id string
	if err := json.Unmarshal(value, &id); err != nil || strings.TrimSpace(id) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q must be an id, or null to clear it", field))
		return OptionalID{}, false
	}
	return OptionalID{Set: true, Value: &id}, true
}

// ---- places ----

func (h *handler) listPlaces(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := PlaceFilter{Query: q.text("q"), Kind: q.text("kind"), Tag: q.text("tag"), GooglePlaceID: q.text("google_place_id"),
		IncludeDeleted: q.boolean("include_deleted"), Limit: int(q.integer("limit")), Offset: int(q.integer("offset"))}
	if q.failed {
		return
	}
	places, err := h.s.ListPlaces(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, places)
}

func (h *handler) createPlace(w http.ResponseWriter, r *http.Request) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return
	}
	var create PlaceCreate
	if !decodeBody(w, r, 1<<20, &create) {
		return
	}
	place, err := h.s.CreatePlace(r.Context(), owner, create)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, place)
}

func (h *handler) getPlace(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	place, err := h.s.GetPlace(r.Context(), scope, r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, place)
}

func (h *handler) patchPlace(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var patch PlacePatch
	if _, ok := decodePatch(w, r, []string{"name", "latitude", "longitude", "radius_meters", "address", "kind", "description", "described_by", "tags"}, nil, &patch); !ok {
		return
	}
	place, err := h.s.PatchPlace(r.Context(), scope, r.PathValue("id"), patch)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, place)
}

func (h *handler) mergePlace(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var request PlaceMergeRequest
	if !decodeBody(w, r, 1<<16, &request) {
		return
	}
	merge, err := h.s.MergePlace(r.Context(), scope, r.PathValue("id"), request)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, merge)
}

// ---- visits ----

func (h *handler) listVisits(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := VisitFilter{TimeFilter: q.timeFilter(), PlaceID: q.text("place_id")}
	if q.failed {
		return
	}
	visits, err := h.s.ListVisits(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, visits)
}

func (h *handler) createVisit(w http.ResponseWriter, r *http.Request) {
	owner, scope, ok := newRecordOwner(w, r)
	if !ok {
		return
	}
	var create VisitCreate
	if !decodeBody(w, r, 1<<20, &create) {
		return
	}
	visit, err := h.s.CreateVisit(r.Context(), scope, owner, create)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, visit)
}

func (h *handler) getVisit(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	visit, err := h.s.GetVisitWithCalendarEventLinks(r.Context(), scope, r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, visit)
}

func (h *handler) patchVisit(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var patch VisitPatch
	raw, ok := decodePatch(w, r, []string{"description", "described_by", "tags"}, []string{"place_id"}, &patch)
	if !ok {
		return
	}
	if patch.PlaceID, ok = optionalID(w, raw, "place_id"); !ok {
		return
	}
	visit, err := h.s.PatchVisit(r.Context(), scope, r.PathValue("id"), patch)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, visit)
}

// ---- trips ----

func (h *handler) listTrips(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := TripFilter{TimeFilter: q.timeFilter(), Mode: q.text("mode"), Kind: q.text("kind"), ParentTripID: q.text("parent_trip_id"),
		MinimumDistanceMeters: q.number("min_distance_meters"), ExcludeParents: q.boolean("exclude_parents")}
	if q.failed {
		return
	}
	trips, err := h.s.ListTrips(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, trips)
}

func (h *handler) createTrip(w http.ResponseWriter, r *http.Request) {
	owner, scope, ok := newRecordOwner(w, r)
	if !ok {
		return
	}
	var create TripCreate
	if !decodeBody(w, r, 1<<20, &create) {
		return
	}
	trip, err := h.s.CreateTrip(r.Context(), scope, owner, create)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, trip)
}

func (h *handler) getTrip(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	trip, err := h.s.GetTripWithCalendarEventLinks(r.Context(), scope, r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, trip)
}

func (h *handler) patchTrip(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var patch TripPatch
	raw, ok := decodePatch(w, r, []string{"mode", "title", "description", "described_by", "tags"},
		[]string{"parent_trip_id", "start_place_id", "end_place_id"}, &patch)
	if !ok {
		return
	}
	if patch.ParentTripID, ok = optionalID(w, raw, "parent_trip_id"); !ok {
		return
	}
	if patch.StartPlaceID, ok = optionalID(w, raw, "start_place_id"); !ok {
		return
	}
	if patch.EndPlaceID, ok = optionalID(w, raw, "end_place_id"); !ok {
		return
	}
	trip, err := h.s.PatchTrip(r.Context(), scope, r.PathValue("id"), patch)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, trip)
}

func (h *handler) tripPath(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	path, err := h.s.TripPath(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, path)
}

func (h *handler) attachChildren(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var attach AttachChildren
	if !decodeBody(w, r, 1<<20, &attach) {
		return
	}
	trip, err := h.s.AttachChildren(r.Context(), scope, r.PathValue("id"), attach.TripIDs)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, trip)
}

// ---- localities, home periods and journeys ----

func (h *handler) fillLocalities(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerActedFor(w, r)
	if !ok {
		return
	}
	var request LocalityFillRequest
	if !decodeBody(w, r, 1<<20, &request) {
		return
	}
	fill, err := h.s.FillLocalities(r.Context(), owner, request.DryRun)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, fill)
}

func (h *handler) listHomePeriods(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	includeDeleted := q.boolean("include_deleted")
	if q.failed {
		return
	}
	periods, err := h.s.ListHomePeriods(r.Context(), scope, includeDeleted)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, periods)
}

func (h *handler) getHomePeriod(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	period, err := h.s.GetHomePeriod(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, period)
}

func (h *handler) recordLivePoints(w http.ResponseWriter, r *http.Request) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return
	}
	var live LivePoints
	if !decodeBody(w, r, 4<<20, &live) {
		return
	}
	recorded, err := h.s.RecordLivePoints(r.Context(), owner, live)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, recorded)
}

func (h *handler) segmentPhonePoints(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerActedFor(w, r)
	if !ok {
		return
	}
	var request PhoneSegmentRequest
	if !decodeBody(w, r, 1<<20, &request) {
		return
	}
	segmentation, err := h.s.SegmentPhonePoints(r.Context(), owner, request)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, segmentation)
}

func (h *handler) groupJourneys(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerActedFor(w, r)
	if !ok {
		return
	}
	var request JourneyGroupRequest
	if !decodeBody(w, r, 1<<20, &request) {
		return
	}
	grouping, err := h.s.GroupJourneys(r.Context(), owner, request)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, grouping)
}

func (h *handler) listJourneys(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := TripFilter{TimeFilter: q.timeFilter(), Kind: TripKindJourney}
	if q.failed {
		return
	}
	trips, err := h.s.ListTrips(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, trips)
}

func (h *handler) getJourney(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	summary, err := h.s.Journey(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// ---- calendar events ----

func (h *handler) matchCalendar(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerActedFor(w, r)
	if !ok {
		return
	}
	var request CalendarMatchRequest
	if !decodeBody(w, r, 64<<20, &request) {
		return
	}
	matching, err := h.s.MatchCalendar(r.Context(), owner, request)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, matching)
}

func (h *handler) listCalendarEventLinks(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := CalendarEventLinkFilter{From: q.integer("from"), To: q.integer("to"), VisitID: q.text("visit_id"), TripID: q.text("trip_id"),
		PlaceID: q.text("place_id"), CalendarID: q.text("calendar_id"), EventID: q.text("event_id"), Rule: q.text("rule"),
		MatchedBy: q.text("matched_by"), IncludeDeleted: q.boolean("include_deleted"), Limit: int(q.integer("limit")), Offset: int(q.integer("offset"))}
	if q.failed {
		return
	}
	links, err := h.s.ListCalendarEventLinks(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, links)
}

func (h *handler) listCalendarSeriesPlaces(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := CalendarSeriesPlaceFilter{PlaceID: q.text("place_id"), CalendarID: q.text("calendar_id"), Outcome: q.text("outcome"),
		Limit: int(q.integer("limit")), Offset: int(q.integer("offset"))}
	if q.failed {
		return
	}
	rows, err := h.s.ListCalendarSeriesPlaces(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *handler) getCalendarSeriesPlace(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	row, err := h.s.GetCalendarSeriesPlace(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (h *handler) listUnnamedPlaces(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	limit, offset := int(q.integer("limit")), int(q.integer("offset"))
	if q.failed {
		return
	}
	places, err := h.s.ListUnnamedPlaces(r.Context(), scope, limit, offset)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, places)
}

func (h *handler) proposePlaceSuggestions(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerActedFor(w, r)
	if !ok {
		return
	}
	var request PlaceSuggestionProposals
	if !decodeBody(w, r, 64<<20, &request) {
		return
	}
	answer, err := h.s.ProposePlaceSuggestions(r.Context(), owner, request)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (h *handler) listPlaceSuggestions(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := PlaceSuggestionFilter{PlaceID: q.text("place_id"), Status: q.text("status"), Source: q.text("source"),
		IncludeDeleted: q.boolean("include_deleted"), Limit: int(q.integer("limit")), Offset: int(q.integer("offset"))}
	if q.failed {
		return
	}
	suggestions, err := h.s.ListPlaceSuggestions(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suggestions)
}

func (h *handler) getPlaceSuggestion(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	suggestion, err := h.s.GetPlaceSuggestion(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suggestion)
}

func (h *handler) decidePlaceSuggestion(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(w, r)
		if !ok {
			return
		}
		var decision PlaceSuggestionDecision
		if !decodeBody(w, r, 1<<20, &decision) {
			return
		}
		decide := h.s.RejectPlaceSuggestion
		if accept {
			decide = h.s.AcceptPlaceSuggestion
		} else if decision.Name != "" || decision.Address != "" || decision.Kind != "" {
			writeErr(w, http.StatusBadRequest, "reject takes decided_by and note only")
			return
		}
		suggestion, err := decide(r.Context(), scope, r.PathValue("id"), decision)
		if respondError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, suggestion)
	}
}

func (h *handler) createCalendarEventLink(w http.ResponseWriter, r *http.Request) {
	owner, scope, ok := newRecordOwner(w, r)
	if !ok {
		return
	}
	var create CalendarEventLinkCreate
	if !decodeBody(w, r, 1<<20, &create) {
		return
	}
	link, err := h.s.CreateCalendarEventLink(r.Context(), scope, owner, create)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

func (h *handler) getCalendarEventLink(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	link, err := h.s.GetCalendarEventLink(r.Context(), scope, r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (h *handler) deleteCalendarEventLink(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.DeleteCalendarEventLink(r.Context(), scope, r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- shared ----

func (h *handler) remove(entityType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(w, r)
		if !ok {
			return
		}
		if respondError(w, h.s.Delete(r.Context(), scope, entityType, r.PathValue("id"))) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) restore(entityType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if respondError(w, h.s.Restore(r.Context(), scope, entityType, id)) {
			return
		}
		var record any
		var err error
		switch entityType {
		case EntityPlace:
			record, err = h.s.GetPlace(r.Context(), scope, id, false)
		case EntityVisit:
			record, err = h.s.GetVisit(r.Context(), scope, id, false)
		default:
			record, err = h.s.GetTrip(r.Context(), scope, id, false)
		}
		if respondError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func (h *handler) revisions(entityType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(w, r)
		if !ok {
			return
		}
		q := &queryReader{w: w, r: r}
		limit, offset := q.integer("limit"), q.integer("offset")
		if q.failed {
			return
		}
		revisions, err := h.s.Revisions(r.Context(), scope, entityType, r.PathValue("id"), int(limit), int(offset))
		if respondError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, revisions)
	}
}

func (h *handler) timeline(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	from, to, limit := q.integer("from"), q.integer("to"), q.integer("limit")
	source, includeParentTrips := q.text("source"), q.boolean("include_parent_trips")
	if q.failed {
		return
	}
	items, err := h.s.Timeline(r.Context(), scope, from, to, source, includeParentTrips, int(limit))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) listPoints(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	filter := PointFilter{From: q.integer("from"), To: q.integer("to"), Source: q.text("source"), SourceRef: q.text("source_ref"),
		Limit: int(q.integer("limit")), Offset: int(q.integer("offset"))}
	if q.failed {
		return
	}
	points, err := h.s.ListPoints(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, points)
}

// ---- imports ----

func (h *handler) listImports(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	q := &queryReader{w: w, r: r}
	limit, offset := q.integer("limit"), q.integer("offset")
	if q.failed {
		return
	}
	imports, err := h.s.ListImports(r.Context(), scope, int(limit), int(offset))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, imports)
}

func (h *handler) startImport(w http.ResponseWriter, r *http.Request) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return
	}
	var start ImportStart
	if !decodeBody(w, r, 1<<20, &start) {
		return
	}
	started, err := h.s.StartImport(r.Context(), owner, start)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, started)
}

func (h *handler) getImport(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	record, err := h.s.GetImport(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (h *handler) importBatch(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var batch ImportBatch
	if !decodeBody(w, r, 256<<20, &batch) {
		return
	}
	counts, err := h.s.ApplyBatch(r.Context(), scope, r.PathValue("id"), batch)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (h *handler) finishImport(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var finish ImportFinish
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&finish); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	record, err := h.s.FinishImport(r.Context(), scope, r.PathValue("id"), finish)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("location-store: encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func respondError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case isUniqueViolation(err):
		// A concurrent write of the same owner took the same key first.
		writeErr(w, http.StatusConflict, err.Error())
	default:
		log.Printf("location-store: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}
