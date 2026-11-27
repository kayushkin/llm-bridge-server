package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"
)

// SessionPrincipal is one principal sessions were started as, among the
// sessions the caller may see: the sidebar's "who started it" marker and its
// filter read these. PrincipalID ” stands for sessions started as no
// principal, with DisplayName ”. DisplayName is principal-store's, read now,
// for display only.
type SessionPrincipal struct {
	PrincipalID  string `json:"principal_id"`
	DisplayName  string `json:"display_name"`
	SessionCount int    `json:"session_count"`
}

// handleSessionPrincipals answers GET /sessions/principals: who the caller's
// visible sessions were started as, most sessions first. A person held to their
// own sessions sees only themselves; an administrator sees everyone.
func (s *Server) handleSessionPrincipals(w http.ResponseWriter, r *http.Request) {
	ownedBy, throughPeer := sessionScopeFilterOfRequest(r)
	counts, err := s.store.SessionCountsByPrincipal(ownedBy, throughPeer)
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "count_sessions", err.Error())
		return
	}
	principals := make([]SessionPrincipal, 0, len(counts))
	for _, count := range counts {
		row := SessionPrincipal{PrincipalID: count.PrincipalID, SessionCount: count.SessionCount}
		if count.PrincipalID != "" {
			if s.principalLookupCache == nil {
				httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "principal_store_not_configured", "names come from principal-store, and this server has no principal lookup")
				return
			}
			principal, err := s.principalLookupCache.lookUp(r.Context(), count.PrincipalID)
			if err != nil {
				httpjson.WriteJSONError(w, http.StatusBadGateway, "principal_store_unavailable", "read "+count.PrincipalID+" from principal-store: "+err.Error())
				return
			}
			row.DisplayName = principal.DisplayName
		}
		principals = append(principals, row)
	}
	httpjson.WriteJSON(w, map[string]any{"principals": principals})
}
