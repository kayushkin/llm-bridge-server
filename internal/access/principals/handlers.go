package principals

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Handlers answers the store's routes. The bridge mounts each method under
// /principal-store/ with the paths the standalone store served at its root, so
// every route reads its path values by the same names ({id}, {group},
// {member}, {timeOffID}). Who may call which route is the bridge's decision;
// nothing here checks a caller.
type Handlers struct {
	s *Store
}

// NewHandlers returns the handlers of store's routes.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{s: store}
}

// patchableFields is every key PATCH /principals/{id} accepts, matching the
// json tags on Patch. kind and disabled_at are deliberately absent and are
// refused by name, pointing at what does move them.
var patchableFields = map[string]bool{
	"display_name":     true,
	"email":            true,
	"availability":     true,
	"is_administrator": true,
}

// unpatchableFields maps a key PATCH refuses to the explanation it answers with.
var unpatchableFields = map[string]string{
	"kind":        "kind is fixed at creation — a group that became a human would strand its memberships. Create a new principal instead.",
	"disabled_at": "use POST /principals/{id}/disable or POST /principals/{id}/enable, so that removal is always an explicit act.",
	"id":          "id is assigned by this store and never changes; other stores join on it.",
	"seq":         "seq is assigned by this store and never changes; it generates the id.",
}

func sortedPatchableFields() []string {
	out := make([]string, 0, len(patchableFields))
	for field := range patchableFields {
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	counts, err := h.s.Counts(r.Context())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func (h *Handlers) Kinds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Kinds)
}

func (h *Handlers) KindDefinitions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, KindDefinitions)
}

func (h *Handlers) AvailabilityReasons(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, AvailabilityReasons)
}

func (h *Handlers) WeekdayCodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, WeekdayCodes)
}

// instantFrom reads an epoch-seconds query parameter, defaulting to now when
// absent. A value that is present and not an integer is a 400 — a caller
// that sent "2026-09-11" must not be answered about this very second.
func instantFrom(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, true
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be epoch seconds, got %q", name, raw))
		return nil, false
	}
	at := time.Unix(seconds, 0).UTC()
	return &at, true
}

func filterFrom(w http.ResponseWriter, r *http.Request) (Filter, bool) {
	q := r.URL.Query()
	f := Filter{
		Kind:            q.Get("kind"),
		Query:           q.Get("q"),
		IncludeDisabled: isTrue(q.Get("include_disabled")),
		Limit:           int(atoi64(q.Get("limit"))),
		Offset:          int(atoi64(q.Get("offset"))),
	}
	if _, present := q["available_at"]; present {
		at, ok := instantFrom(w, r, "available_at")
		if !ok {
			return f, false
		}
		if at == nil {
			t := time.Now().UTC()
			at = &t
		}
		f.AvailableAt = at
	}
	return f, true
}

func (h *Handlers) ListPrincipals(w http.ResponseWriter, r *http.Request) {
	f, ok := filterFrom(w, r)
	if !ok {
		return
	}
	principals, err := h.s.List(r.Context(), f)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, principals)
}

func (h *Handlers) Availability(w http.ResponseWriter, r *http.Request) {
	at, ok := instantFrom(w, r, "at")
	if !ok {
		return
	}
	if at == nil {
		t := time.Now().UTC()
		at = &t
	}
	answer, err := h.s.Availability(r.Context(), r.PathValue("id"), *at)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (h *Handlers) ListTimeOff(w http.ResponseWriter, r *http.Request) {
	rows, err := h.s.ListTimeOff(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handlers) AddTimeOff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StartsAt int64  `json:"starts_at"`
		EndsAt   int64  `json:"ends_at"`
		Note     string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	created, err := h.s.AddTimeOff(r.Context(), r.PathValue("id"), body.StartsAt, body.EndsAt, body.Note)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handlers) DeleteTimeOff(w http.ResponseWriter, r *http.Request) {
	if respondStoreError(w, h.s.RemoveTimeOff(r.Context(), r.PathValue("id"), r.PathValue("timeOffID"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveContact turns an email address into one contact id, creating the
// contact the first time that address is seen. It is how mail intake gives a
// ticket a requester it can join on; see contact.go for why this is the one
// lookup by name in the store.
func (h *Handlers) ResolveContact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	if !decode(w, r, &body) {
		return
	}
	resolution, err := h.s.ResolveContact(r.Context(), body.Email, body.DisplayName)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if resolution.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resolution)
}

func (h *Handlers) CreatePrincipal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind        string `json:"kind"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	created, err := h.s.Create(r.Context(), &Principal{Kind: body.Kind, DisplayName: body.DisplayName, Email: body.Email})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handlers) GetPrincipal(w http.ResponseWriter, r *http.Request) {
	p, err := h.s.Get(r.Context(), r.PathValue("id"), isTrue(r.URL.Query().Get("include_disabled")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handlers) PatchPrincipal(w http.ResponseWriter, r *http.Request) {
	// Decoded strictly so that PATCHing "kind" or "disabled_at" is a loud 400
	// naming what does move them, not a silent no-op that leaves the caller
	// believing the row changed.
	var raw map[string]json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	for field, explanation := range unpatchableFields {
		if _, present := raw[field]; present {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q cannot be patched: %s", field, explanation))
			return
		}
	}
	// Decoding into a map defeats DisallowUnknownFields, so the allowed keys
	// are checked by hand. Without this a PATCH of {"display_nmae":"…"} answers
	// 200 having changed nothing, and the caller believes the edit landed.
	for field := range raw {
		if !patchableFields[field] {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf(
				"unknown field %q: PATCH accepts %s", field, strings.Join(sortedPatchableFields(), ", ")))
			return
		}
	}
	body, err := json.Marshal(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	var patch Patch
	if err := json.Unmarshal(body, &patch); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	updated, err := h.s.Patch(r.Context(), r.PathValue("id"), patch)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handlers) DisablePrincipal(w http.ResponseWriter, r *http.Request) {
	p, err := h.s.Disable(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handlers) EnablePrincipal(w http.ResponseWriter, r *http.Request) {
	p, err := h.s.Enable(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handlers) ListMembers(w http.ResponseWriter, r *http.Request) {
	if _, present := r.URL.Query()["available_at"]; present {
		at, ok := instantFrom(w, r, "available_at")
		if !ok {
			return
		}
		if at == nil {
			t := time.Now().UTC()
			at = &t
		}
		members, err := h.s.ListAvailableMembers(r.Context(), r.PathValue("id"), *at)
		if respondStoreError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, members)
		return
	}
	members, err := h.s.ListMembers(r.Context(), r.PathValue("id"), isTrue(r.URL.Query().Get("include_disabled")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, members)
}

func (h *Handlers) ListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.s.ListGroups(r.Context(), r.PathValue("id"), isTrue(r.URL.Query().Get("include_disabled")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (h *Handlers) PutMember(w http.ResponseWriter, r *http.Request) {
	groupID, memberID := r.PathValue("group"), r.PathValue("member")
	created, err := h.s.AddMember(r.Context(), groupID, memberID)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, GroupMembership{GroupID: groupID, MemberID: memberID, Created: created})
}

func (h *Handlers) DeleteMember(w http.ResponseWriter, r *http.Request) {
	if respondStoreError(w, h.s.RemoveMember(r.Context(), r.PathValue("group"), r.PathValue("member"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decode reads a JSON body, rejecting unknown fields so a misspelled key is a
// 400 rather than a write that silently drops it.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		fmt.Printf("principal-store: encode response: %v\n", err)
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func respondStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotAMember):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrIdentityAlreadyBound):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidPrincipal), errors.Is(err, ErrInvalidMembership),
		errors.Is(err, ErrInvalidAvailability), errors.Is(err, ErrInvalidContact),
		errors.Is(err, ErrInvalidIdentityBinding):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func atoi64(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func isTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
