package grantstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Handlers answers grant-store's routes. The server it is mounted in
// registers each one under its own prefix, with the same paths and wildcard
// names as the standalone store's: GET /grants/{id}, POST /grants/{id}/revoke,
// GET /principals/{id}/effective and the rest. Every route but Health,
// Relations and ResourceTypes reads the caller from the request's context; see
// principal_enforcement.go.
type Handlers struct {
	store     *Store
	directory PrincipalDirectory
	checker   ResourceChecker
}

// NewHandlers answers the routes from store, checking principals with
// directory and resources with checker. A nil one panics here, at start, rather
// than at the first POST.
func NewHandlers(store *Store, directory PrincipalDirectory, checker ResourceChecker) *Handlers {
	if store == nil {
		panic("grant-store: NewHandlers needs a Store")
	}
	if directory == nil {
		panic("grant-store: NewHandlers needs a PrincipalDirectory; without one a grant's principal cannot be checked against principal-store")
	}
	if checker == nil {
		panic("grant-store: NewHandlers needs a ResourceChecker; without one a grant's resource cannot be checked against its owner")
	}
	return &Handlers{store: store, directory: directory, checker: checker}
}

// Health answers its route.
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	counts, err := h.store.Counts(r.Context())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

// Relations answers its route.
func (h *Handlers) Relations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Relations)
}

// ResourceTypes answers its route.
func (h *Handlers) ResourceTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ResourceTypes)
}

// ListGrants answers its route.
func (h *Handlers) ListGrants(w http.ResponseWriter, r *http.Request) {
	caller, identified := h.identifyCaller(w, r)
	if !identified {
		return
	}
	q := r.URL.Query()
	if !caller.unrestricted && q.Get("principal_id") != caller.principalID {
		if q.Get("resource_type") != ResourceTypeBoard || q.Get("resource_id") == "" {
			writeErr(w, http.StatusForbidden, "a principal lists grants with principal_id set to itself, or with resource_type=board and the resource_id of a board it administers")
			return
		}
		administers, err := h.callerAdministersBoard(r, caller, q.Get("resource_id"))
		if respondStoreError(w, err) {
			return
		}
		if !administers {
			writeErr(w, http.StatusForbidden, "listing the grants on board "+q.Get("resource_id")+" needs can_administer on it")
			return
		}
	}
	grants, err := h.store.List(r.Context(), Filter{
		PrincipalID:    q.Get("principal_id"),
		Relation:       q.Get("relation"),
		ResourceType:   q.Get("resource_type"),
		ResourceID:     q.Get("resource_id"),
		IncludeRevoked: isTrue(q.Get("include_revoked")),
		Limit:          int(atoi64(q.Get("limit"))),
		Offset:         int(atoi64(q.Get("offset"))),
	})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

// CreateGrant answers its route.
func (h *Handlers) CreateGrant(w http.ResponseWriter, r *http.Request) {
	var request GrantRequest
	caller, identified := h.identifyCaller(w, r)
	if !identified {
		return
	}
	if !decode(w, r, &request) {
		return
	}
	if !caller.unrestricted {
		if request.ResourceType != ResourceTypeBoard {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("a principal may grant only on boards it administers; a %q grant needs the internal service or an administrator", request.ResourceType))
			return
		}
		administers, err := h.callerAdministersBoard(r, caller, request.ResourceID)
		if respondStoreError(w, err) {
			return
		}
		if !administers {
			writeErr(w, http.StatusForbidden, "granting on board "+request.ResourceID+" needs can_administer on it")
			return
		}
	}
	grant, created, err := h.store.Create(r.Context(), h.directory, h.checker, request)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, grant)
}

// GetGrant answers its route.
func (h *Handlers) GetGrant(w http.ResponseWriter, r *http.Request) {
	caller, identified := h.identifyCaller(w, r)
	if !identified {
		return
	}
	grant, err := h.store.Get(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	mayRead, err := h.callerMayReadGrant(r, caller, grant)
	if respondStoreError(w, err) {
		return
	}
	if !mayRead {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("%s: grant %s", ErrNotFound, grant.ID))
		return
	}
	writeJSON(w, http.StatusOK, grant)
}

// RevokeGrant answers its route.
func (h *Handlers) RevokeGrant(w http.ResponseWriter, r *http.Request) {
	caller, identified := h.identifyCaller(w, r)
	if !identified {
		return
	}
	if !caller.unrestricted {
		existing, err := h.store.Get(r.Context(), r.PathValue("id"))
		if respondStoreError(w, err) {
			return
		}
		mayRead, err := h.callerMayReadGrant(r, caller, existing)
		if respondStoreError(w, err) {
			return
		}
		if !mayRead {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("%s: grant %s", ErrNotFound, existing.ID))
			return
		}
		administers := false
		if existing.ResourceType == ResourceTypeBoard {
			administers, err = h.callerAdministersBoard(r, caller, existing.ResourceID)
			if respondStoreError(w, err) {
				return
			}
		}
		if !administers {
			writeErr(w, http.StatusForbidden, "revoking a grant needs can_administer on its board; other grants need the service token")
			return
		}
	}
	grant, err := h.store.Revoke(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, grant)
}

// Effective answers its route.
func (h *Handlers) Effective(w http.ResponseWriter, r *http.Request) {
	caller, identified := h.identifyCaller(w, r)
	if !identified {
		return
	}
	if !caller.unrestricted && r.PathValue("id") != caller.principalID {
		writeErr(w, http.StatusForbidden, "a principal reads only its own effective grants")
		return
	}
	q := r.URL.Query()
	grants, err := h.store.Effective(r.Context(), h.directory, r.PathValue("id"), q.Get("relation"), q.Get("resource_type"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, grants)
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
		fmt.Printf("grant-store: encode response: %v\n", err)
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
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalidGrant):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrOwnerUnavailable):
		// principal-store or a resource owner, not this store and not the
		// caller, is what failed.
		writeErr(w, http.StatusBadGateway, err.Error())
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
