package server

// Administrator routes over principal-store's identity bindings: which Entra
// identity signs in as which principal.
//
// principal-store has no authentication of its own and cannot tell an
// administrator from anyone; it records linked_by and unlinked_by as it is
// told. It runs in this process; these routes call its handlers directly. So these
// routes are operator routes, and the bridge — not the body — says who made
// the change: linked_by and unlinked_by are the caller's principal, and a body
// that tries to set either is refused rather than quietly overwritten.
// principal-store's status and body come back unchanged.
//
// Unlinking a binding also revokes every live login session signed in through
// it, so a person whose link is removed is signed out at once rather than at
// the end of their 12 hours.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"
)

// identityBindingLinkRequest is the body POST /admin/identity-bindings takes.
// linked_by is not in it: the bridge sets it.
type identityBindingLinkRequest struct {
	PrincipalID string `json:"principal_id"`
	Provider    string `json:"provider"`
	TenantID    string `json:"tenant_id"`
	Subject     string `json:"subject"`
	Note        string `json:"note"`
}

// identityBindingUnlinkRequest is the body POST
// /admin/identity-bindings/{id}/unlink takes. unlinked_by is not in it.
type identityBindingUnlinkRequest struct {
	Reason string `json:"reason"`
}

// principalStoreAnswer is principal-store's reply, held whole so it can be
// passed back unchanged after the bridge has acted on its status.
type principalStoreAnswer struct {
	status int
	header http.Header
	body   []byte
}

func (answer principalStoreAnswer) writeUnchanged(w http.ResponseWriter) {
	for key, values := range answer.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(answer.status)
	_, _ = w.Write(answer.body)
}

// askPrincipalStore sends one request to principal-store's own route, in this
// process, and keeps the whole answer.
func (s *Server) askPrincipalStore(r *http.Request, method, escapedPath, rawQuery string, body []byte) (principalStoreAnswer, error) {
	handlers := s.access.Load()
	if handlers == nil {
		return principalStoreAnswer{}, errAccessOff
	}
	target := escapedPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	inner, err := http.NewRequestWithContext(r.Context(), method, target, reader)
	if err != nil {
		return principalStoreAnswer{}, fmt.Errorf("build %s %s: %w", method, target, err)
	}
	if body != nil {
		inner.Header.Set("Content-Type", "application/json")
	}
	inner.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handlers.principalStoreRoot.ServeHTTP(recorder, inner)
	header := recorder.Header().Clone()
	header.Del("Content-Length")
	return principalStoreAnswer{status: recorder.Code, header: header, body: recorder.Body.Bytes()}, nil
}

// administratorMakingIdentityBindingChange is the principal a binding change
// is stamped with. The service token acting as itself is nobody, so it must
// say whom it acts for.
func administratorMakingIdentityBindingChange(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID, identified := requestcaller.PrincipalIdentityOfRequest(r)
	if !identified {
		httpjson.WriteJSONError(w, http.StatusForbidden, "identity_binding_change_requires_a_principal", fmt.Sprintf(
			"a binding records which administrator changed it; send %s with the service token", requestcaller.PrincipalIdentityHeader))
		return "", false
	}
	return principalID, true
}

// handleListIdentityBindings passes GET /identity-bindings through with the
// caller's query unchanged.
func (s *Server) handleListIdentityBindings(w http.ResponseWriter, r *http.Request) {
	answer, err := s.askPrincipalStore(r, http.MethodGet, "/identity-bindings", r.URL.RawQuery, nil)
	if err != nil {
		writePrincipalStoreCallError(w, err)
		return
	}
	answer.writeUnchanged(w)
}

// handleLinkIdentityBinding links an outside identity to a principal, as the
// calling administrator.
func (s *Server) handleLinkIdentityBinding(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingIdentityBindingChange(w, r)
	if !ok {
		return
	}
	var request identityBindingLinkRequest
	if !httpjson.DecodeAdministrationBody(w, r, &request, "linked_by") {
		return
	}
	body, err := json.Marshal(map[string]string{
		"principal_id": request.PrincipalID,
		"provider":     request.Provider,
		"tenant_id":    request.TenantID,
		"subject":      request.Subject,
		"note":         request.Note,
		"linked_by":    administratorID,
	})
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "invalid_request", err.Error())
		return
	}
	answer, err := s.askPrincipalStore(r, http.MethodPost, "/identity-bindings", "", body)
	if err != nil {
		writePrincipalStoreCallError(w, err)
		return
	}
	answer.writeUnchanged(w)
}

// handleUnlinkIdentityBinding unlinks a binding, as the calling administrator,
// and then revokes every live login session signed in through it.
func (s *Server) handleUnlinkIdentityBinding(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingIdentityBindingChange(w, r)
	if !ok {
		return
	}
	var request identityBindingUnlinkRequest
	if !httpjson.DecodeAdministrationBody(w, r, &request, "unlinked_by") {
		return
	}
	bindingID := r.PathValue("id")
	body, err := json.Marshal(map[string]string{"unlinked_by": administratorID, "reason": request.Reason})
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "invalid_request", err.Error())
		return
	}
	answer, err := s.askPrincipalStore(r, http.MethodPost, "/identity-bindings/"+url.PathEscape(bindingID)+"/unlink", "", body)
	if err != nil {
		writePrincipalStoreCallError(w, err)
		return
	}
	if answer.status == http.StatusOK {
		revokedIDs, err := s.store.RevokeLiveLoginSessionsOfIdentityBinding(bindingID, administratorID, "identity unlinked: "+request.Reason, s.loginClock())
		if err != nil {
			// principal-store has already unlinked it, so no new sign-in can
			// use it; the sessions it already opened are what is left live.
			log.Printf("[login] ERROR: identity binding %s was unlinked by %s, and its login sessions could not be revoked: %v", bindingID, administratorID, err)
			httpjson.WriteJSONError(w, http.StatusInternalServerError, "login_sessions_not_revoked", fmt.Sprintf(
				"identity binding %s was unlinked in principal-store, but the login sessions signed in through it are still live: %v; revoke them with POST /auth/sessions/{id}/revoke", bindingID, err))
			return
		}
		if len(revokedIDs) > 0 {
			log.Printf("[login] identity binding %s unlinked by %s; revoked login sessions %s", bindingID, administratorID, strings.Join(revokedIDs, ", "))
		}
	}
	answer.writeUnchanged(w)
}
