package principalstore

import "net/http"

// IdentityBindingLink is the body of POST /identity-bindings.
type IdentityBindingLink struct {
	PrincipalID string `json:"principal_id"`
	Provider    string `json:"provider"`
	TenantID    string `json:"tenant_id"`
	Subject     string `json:"subject"`
	LinkedBy    string `json:"linked_by"`
	Note        string `json:"note"`
}

// IdentityBindingUnlink is the body of POST /identity-bindings/{id}/unlink.
type IdentityBindingUnlink struct {
	UnlinkedBy string `json:"unlinked_by"`
	Reason     string `json:"reason"`
}

func (h *handler) identityProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, IdentityProviders)
}

func (h *handler) listIdentityBindings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bindings, err := h.s.ListIdentityBindings(IdentityBindingFilter{
		PrincipalID:     q.Get("principal_id"),
		Provider:        q.Get("provider"),
		TenantID:        q.Get("tenant_id"),
		IncludeUnlinked: isTrue(q.Get("include_unlinked")),
	})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, bindings)
}

func (h *handler) linkIdentity(w http.ResponseWriter, r *http.Request) {
	var body IdentityBindingLink
	if !decode(w, r, &body) {
		return
	}
	b, err := h.s.LinkIdentity(body.PrincipalID, body.Provider, body.TenantID, body.Subject, body.LinkedBy, body.Note)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (h *handler) resolveIdentity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	resolution, err := h.s.ResolveIdentity(q.Get("provider"), q.Get("tenant_id"), q.Get("subject"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, resolution)
}

func (h *handler) getIdentityBinding(w http.ResponseWriter, r *http.Request) {
	b, err := h.s.GetIdentityBinding(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *handler) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	var body IdentityBindingUnlink
	if !decode(w, r, &body) {
		return
	}
	b, err := h.s.UnlinkIdentity(r.PathValue("id"), body.UnlinkedBy, body.Reason)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, b)
}
