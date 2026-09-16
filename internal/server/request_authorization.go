package server

// Request authorization for a server gated by demo login.
//
// With LLMBRIDGE_DEMO_LOGIN=enabled this server is the whole backend of a
// multi-user product, and every request passes through authorizeRequest
// before the mux dispatches it. With demo login off, ServeHTTP skips all of
// this and behaves exactly as it always has.
//
// Every route is classified in routeAccessRules, keyed by the exact pattern
// it is registered under. The mux is asked which pattern a request matches,
// and the rule for that pattern decides. A pattern with no rule is refused
// (403 naming the pattern) to everyone but the service token, so a route
// added later is closed until someone decides who may call it.
//
// Callers are, in order of precedence:
//
//   - the internal service: X-LLM-Bridge-Service-Token equal to
//     LLMBRIDGE_SERVICE_TOKEN. Unrestricted. A wrong token is 401 — it is never
//     silently downgraded to a principal.
//   - a principal: a valid demo login cookie, or — on the identity-carrying
//     store proxies only — a session agent token (session_agent_token.go).
//     What a principal may do depends on the route's class.
//   - nobody: 401 on any route that is not open or a harness callback.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// serviceTokenHeader carries LLMBRIDGE_SERVICE_TOKEN from an internal service.
const serviceTokenHeader = "X-LLM-Bridge-Service-Token"

// routeAccessClass says who may call a route and how a principal is limited.
type routeAccessClass int

const (
	// routeOpenToEveryone needs no credential at all.
	routeOpenToEveryone routeAccessClass = iota + 1
	// routeHarnessCallback is called back by a harness child process, a
	// sidecar or a runner on this host, which today carry no login. It keeps
	// the behaviour it has without demo login: whatever check the handler
	// itself makes, and nothing more.
	routeHarnessCallback
	// routeOperatorOnly needs the service token.
	routeOperatorOnly
	// routePrincipalCreatesSession is POST /sessions: the session is created
	// as the calling principal, whatever the body says.
	routePrincipalCreatesSession
	// routePrincipalOwnsSession names a session in a path value; a principal
	// reaches it only if the session's principal_id is theirs, and gets 404
	// — the same answer as a missing session — otherwise.
	routePrincipalOwnsSession
	// routePrincipalOwnsSignal names a signal in a path value; a principal
	// reaches it only if the signal's session is theirs, 404 otherwise.
	routePrincipalOwnsSignal
	// routePrincipalSeesOwnSessionsOnly lists or streams across sessions; the
	// handler narrows the answer to the principal's own sessions, reading the
	// principal from the request context.
	routePrincipalSeesOwnSessionsOnly
	// routePrincipalReadsCatalog is a read-only catalog a chat UI needs; a
	// principal reads it as it is (or as the handler narrows it).
	routePrincipalReadsCatalog
	// routePrincipalRefusedCannotBeFiltered answers across sessions from a
	// source that cannot be narrowed to one principal cheaply and correctly,
	// so a principal is refused rather than shown other principals' data.
	routePrincipalRefusedCannotBeFiltered
	// routePrincipalOrSessionAgentThroughStoreProxy is an identity-carrying
	// store proxy: a login cookie or a session agent token, and the request
	// reaches the store as that principal.
	routePrincipalOrSessionAgentThroughStoreProxy
)

// routeAccessRule is one route's classification.
type routeAccessRule struct {
	class routeAccessClass
	// ownedResourcePathValueName names the path value holding the session id
	// (routePrincipalOwnsSession) or the signal id (routePrincipalOwnsSignal).
	ownedResourcePathValueName string
	// reason says why the route is in its class, for the next reader and for
	// the refusal message.
	reason string
}

func openRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeOpenToEveryone, reason: reason}
}
func harnessCallbackRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeHarnessCallback, reason: reason}
}
func operatorRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeOperatorOnly, reason: reason}
}
func sessionOwnedRoute(pathValueName string) routeAccessRule {
	return routeAccessRule{class: routePrincipalOwnsSession, ownedResourcePathValueName: pathValueName, reason: "acts on one session"}
}
func signalOwnedRoute(pathValueName string) routeAccessRule {
	return routeAccessRule{class: routePrincipalOwnsSignal, ownedResourcePathValueName: pathValueName, reason: "acts on one signal of one session"}
}
func ownSessionsOnlyRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routePrincipalSeesOwnSessionsOnly, reason: reason}
}
func catalogRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routePrincipalReadsCatalog, reason: reason}
}
func cannotBeFilteredRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routePrincipalRefusedCannotBeFiltered, reason: reason}
}

// routeAccessRules classifies every route this server registers, by its exact
// registration pattern. A route missing here is refused to all but the
// service token; TestEveryRegisteredRouteIsClassified fails when a
// HandleFunc/Handle in this package has no rule.
var routeAccessRules = map[string]routeAccessRule{
	// Open.
	"GET /health":           openRoute("liveness for load balancers and healthcheck"),
	"POST /auth/demo-login": openRoute("how a principal obtains a login"),
	"GET /auth/principal":   openRoute("answers 401 itself without a valid cookie"),
	"POST /auth/logout":     openRoute("clears a cookie; needs none"),

	// Harness callbacks: called by processes this server spawned or by
	// runners, none of which carry a login.
	"POST /permission/cc-prehook/{bridge_id}":    harnessCallbackRoute("Claude Code PreToolUse hook, a curl from the harness child"),
	"POST /permission/codex-prehook/{bridge_id}": harnessCallbackRoute("codex PreToolUse hook, a curl from the harness child"),
	"POST /sidecar/event/{bridge_id}":            harnessCallbackRoute("OTel sidecar of a pty session posts translated events"),
	"POST /hooks/exec/{id}":                      harnessCallbackRoute("registered hook fired by the harness child via curl"),
	"POST /sessions/{id}/auto-rename":            harnessCallbackRoute("renamer session posts its title via curl; the handler requires the reserved renamer_session_id"),
	"GET /api/runner/ws":                         harnessCallbackRoute("runner WebSocket; the handler authenticates the runner's bearer token"),
	"POST /api/runner/enroll":                    harnessCallbackRoute("runner enrollment; the handler checks the single-use passphrase"),
	"GET /api/runner/install.sh":                 harnessCallbackRoute("bootstrap script fetched by a machine before it has a runner token"),
	"GET /api/runner/binary":                     harnessCallbackRoute("runner and harness binaries fetched by a machine during bootstrap"),
	"/api/agent-store/":                          harnessCallbackRoute("seed proxy for runners; the handler authenticates the runner's bearer token"),
	"/api/skill-store/":                          harnessCallbackRoute("seed proxy for runners; the handler authenticates the runner's bearer token"),
	"/api/harness-proxy/{harness}/{rest...}":     harnessCallbackRoute("harness backend proxy for wrappers on runners; unauthenticated today (noteboard card f4e5e1ef)"),

	// Sessions.
	"POST /sessions":              {class: routePrincipalCreatesSession, reason: "a principal creates sessions as itself"},
	"GET /sessions":               ownSessionsOnlyRoute("session list, narrowed in the store"),
	"GET /sessions/summary":       ownSessionsOnlyRoute("sidebar list, narrowed in the store"),
	"POST /sessions/summary":      ownSessionsOnlyRoute("sidebar list, narrowed in the store"),
	"GET /sessions/recent-bundle": ownSessionsOnlyRoute("recent sessions warmed for the chat page, narrowed in the store"),
	"GET /sessions/validators":    ownSessionsOnlyRoute("staleness check; ids not owned by the caller are dropped before log-store is asked"),
	"POST /sessions/validators":   ownSessionsOnlyRoute("staleness check; ids not owned by the caller are dropped before log-store is asked"),
	"GET /session-events":         ownSessionsOnlyRoute("session list stream; frames about other principals' sessions are not written"),
	"GET /signals":                ownSessionsOnlyRoute("cross-session signal inbox, narrowed in the store"),
	"GET /sessions/search":        cannotBeFilteredRoute("full-text search answered by log-store across every session"),
	"GET /sessions/aggregates":    cannotBeFilteredRoute("aggregates computed by log-store across every session"),
	"GET /sessions/discover":      operatorRoute("imports sessions found on this host's disk"),

	"GET /sessions/{id}":                               sessionOwnedRoute("id"),
	"POST /sessions/{id}/send":                         sessionOwnedRoute("id"),
	"GET /sessions/{id}/events":                        sessionOwnedRoute("id"),
	"GET /sessions/{id}/attach":                        sessionOwnedRoute("id"),
	"GET /sessions/{id}/attach-token":                  sessionOwnedRoute("id"),
	"GET /sessions/{id}/messages":                      sessionOwnedRoute("id"),
	"GET /sessions/{id}/messages/raw":                  sessionOwnedRoute("id"),
	"GET /sessions/{id}/entries/{eventId}":             sessionOwnedRoute("id"),
	"POST /sessions/{id}/interrupt":                    sessionOwnedRoute("id"),
	"POST /sessions/{id}/resume":                       sessionOwnedRoute("id"),
	"POST /sessions/{id}/stop":                         sessionOwnedRoute("id"),
	"POST /sessions/{id}/mode":                         sessionOwnedRoute("id"),
	"POST /sessions/{id}/compact":                      sessionOwnedRoute("id"),
	"POST /sessions/{id}/fork":                         sessionOwnedRoute("id"),
	"POST /sessions/{id}/rename":                       sessionOwnedRoute("id"),
	"POST /sessions/{id}/config":                       sessionOwnedRoute("id"),
	"POST /sessions/{id}/mark-done":                    sessionOwnedRoute("id"),
	"GET /sessions/{id}/git/repos":                     sessionOwnedRoute("id"),
	"GET /sessions/{id}/git":                           sessionOwnedRoute("id"),
	"GET /sessions/{id}/hooks/pending":                 sessionOwnedRoute("id"),
	"POST /sessions/{id}/hooks/{request_id}/resolve":   sessionOwnedRoute("id"),
	"GET /sessions/{id}/signals":                       sessionOwnedRoute("id"),
	"POST /sessions/{id}/signals":                      sessionOwnedRoute("id"),
	"GET /sessions/{id}/tools/{tool_use_id}/snapshots": sessionOwnedRoute("id"),
	"PUT /sessions/{id}/folder":                        operatorRoute("files a session into the shared folder registry"),
	"PUT /sessions/{id}/permission-mode":               operatorRoute("can switch a session to bypass, skipping permission-store"),
	"PUT /sessions/{id}/bypass-permissions":            operatorRoute("can switch a session to bypass, skipping permission-store"),
	"POST /signals/{id}/resolve":                       signalOwnedRoute("id"),
	"POST /signals/{id}/answer":                        signalOwnedRoute("id"),
	"GET /snapshots/blob/{sha}":                        cannotBeFilteredRoute("content-addressed blob shared across sessions; nothing ties a sha to one session"),

	// Catalogs a chat UI reads.
	"GET /harnesses":                     catalogRoute("harness types"),
	"GET /harnesses/{name}/capabilities": catalogRoute("what a harness type supports"),
	"GET /harnesses/{name}/agents":       catalogRoute("agents available on a harness type"),
	"/images/":                           catalogRoute("harness images"),
	"GET /session-taxonomy":              catalogRoute("session type and purpose vocabulary"),
	"GET /instances":                     catalogRoute("instances to start a session on; narrowed to can_dispatch_on grants and without machine details"),
	"GET /agents":                        catalogRoute("agent-store agents, unfiltered by can_run_as"),
	"GET /agents/{slug}":                 catalogRoute("one agent-store agent, unfiltered by can_run_as"),

	// Identity-carrying store proxies.
	kanbanProxyMountPrefix + "/": {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "kanban-store as the caller's principal"},

	// Operator.
	"GET /services":                                operatorRoute("host service inventory"),
	"GET /services/databases/schema":               operatorRoute("host database schemas"),
	"GET /services/databases/rows":                 operatorRoute("host database rows"),
	"POST /bridge/permission-mode":                 operatorRoute("global permission mode"),
	"POST /bridge/bypass-permissions":              operatorRoute("global permission mode"),
	"GET /folders":                                 operatorRoute("folder registry"),
	"POST /folders":                                operatorRoute("folder registry"),
	"DELETE /folders/{name}":                       operatorRoute("folder registry"),
	"PUT /folders/{name}":                          operatorRoute("folder registry"),
	"GET /source-folders":                          operatorRoute("purpose to folder mapping"),
	"PUT /source-folders/{source}":                 operatorRoute("purpose to folder mapping"),
	"DELETE /source-folders/{source}":              operatorRoute("purpose to folder mapping"),
	"GET /session-taxonomy/report":                 operatorRoute("lists sessions across every principal"),
	"POST /instances":                              operatorRoute("instance registry"),
	"GET /instances/{id}":                          operatorRoute("instance with machine details"),
	"PUT /instances/{id}":                          operatorRoute("instance registry"),
	"DELETE /instances/{id}":                       operatorRoute("instance registry"),
	"GET /instances/{id}/status":                   operatorRoute("machine reachability"),
	"GET /instances/{id}/sessions":                 operatorRoute("sessions of every principal on an instance"),
	"GET /instances/{id}/credentials":              operatorRoute("credential bindings"),
	"POST /instances/{id}/credentials":             operatorRoute("credential bindings"),
	"DELETE /instances/{id}/credentials/{cred_id}": operatorRoute("credential bindings"),
	"POST /instances/{id}/oneshot":                 operatorRoute("runs a model call outside any session"),
	"POST /hooks":                                  operatorRoute("hook registry"),
	"GET /hooks":                                   operatorRoute("hook registry"),
	"GET /hooks/{id}":                              operatorRoute("hook registry"),
	"PATCH /hooks/{id}":                            operatorRoute("hook registry"),
	"DELETE /hooks/{id}":                           operatorRoute("hook registry"),
	"GET /credentials":                             operatorRoute("auth-store credentials"),
	"POST /credentials":                            operatorRoute("auth-store credentials"),
	"DELETE /credentials/{id}":                     operatorRoute("auth-store credentials"),
	"GET /models":                                  operatorRoute("models joined to auth-store credential labels and masked keys"),
	"GET /conformance":                             operatorRoute("harness conformance matrix"),
	"POST /conformance/run":                        operatorRoute("runs harness conformance"),
	"GET /bridge-prefs":                            operatorRoute("server preferences"),
	"PUT /bridge-prefs":                            operatorRoute("server preferences"),
	"POST /admin/file-inactive":                    operatorRoute("housekeeping across every session"),
	"POST /admin/archive-old":                      operatorRoute("housekeeping across every session"),
	"GET /machines":                                operatorRoute("machine registry"),
	"POST /machines":                               operatorRoute("machine registry"),
	"GET /machines/{id}":                           operatorRoute("machine registry"),
	"PUT /machines/{id}":                           operatorRoute("machine registry"),
	"DELETE /machines/{id}":                        operatorRoute("machine registry"),
	"POST /api/runner/seed/broadcast":              operatorRoute("tells every runner to reconcile"),

	// agent-store's library routes other than the two catalog reads above.
	"POST /agents":                           operatorRoute("agent-store write"),
	"PUT /agents/{slug}":                     operatorRoute("agent-store write"),
	"DELETE /agents/{slug}":                  operatorRoute("agent-store write"),
	"GET /agents/{slug}/harnesses":           operatorRoute("agent-store harness bindings"),
	"POST /agents/{slug}/harnesses":          operatorRoute("agent-store write"),
	"GET /agents/{slug}/config":              operatorRoute("agent-store runtime config"),
	"GET /configs":                           operatorRoute("agent-store runtime configs"),
	"GET /reconcile":                         operatorRoute("agent-store reconcile"),
	"GET /files":                             operatorRoute("agent-store tracked context files"),
	"GET /files/{id}":                        operatorRoute("agent-store tracked context files"),
	"GET /files/{id}/content":                operatorRoute("agent-store tracked context files"),
	"PUT /files/{id}/content":                operatorRoute("agent-store write"),
	"POST /files/{id}/enable":                operatorRoute("agent-store write"),
	"POST /files/{id}/disable":               operatorRoute("agent-store write"),
	"POST /files/scan":                       operatorRoute("agent-store scan"),
	"GET /prompt-collections":                operatorRoute("agent-store prompts"),
	"POST /prompt-collections":               operatorRoute("agent-store write"),
	"POST /prompt-collections/{id}/sections": operatorRoute("agent-store write"),
	"POST /prompt-collections/{id}/compile":  operatorRoute("agent-store write"),
	"PUT /prompt-sections/{id}":              operatorRoute("agent-store write"),
	"DELETE /prompt-sections/{id}":           operatorRoute("agent-store write"),
	"GET /files/{id}/versions":               operatorRoute("agent-store tracked context files"),
	"GET /versions/{vid}/content":            operatorRoute("agent-store tracked context files"),
	"GET /seed/profiles":                     operatorRoute("agent-store seed"),
	"GET /seed/profile":                      operatorRoute("agent-store seed"),
	"PUT /seed/profile":                      operatorRoute("agent-store seed"),
	"GET /seed/manifest":                     operatorRoute("agent-store seed"),
	"POST /seed/observe":                     operatorRoute("agent-store seed"),
	"POST /seed/drift":                       operatorRoute("agent-store seed"),
	"GET /seed/state":                        operatorRoute("agent-store seed"),
	"GET /context/resolve":                   operatorRoute("agent-store context"),

	// memory-store's library routes.
	"POST /memories":         operatorRoute("memory-store is not partitioned by principal"),
	"GET /memories/{id}":     operatorRoute("memory-store is not partitioned by principal"),
	"DELETE /memories/{id}":  operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/search":  operatorRoute("memory-store is not partitioned by principal"),
	"GET /memories/recent":   operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/decay":   operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/compact": operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/context": operatorRoute("memory-store is not partitioned by principal"),
}

// requestPrincipalContextKey carries the principal a gated request acts as.
type requestPrincipalContextKey struct{}

// principalRestrictingRequest returns the principal a request is restricted
// to, and false when it is not restricted: demo login is off, or the caller
// is the internal service. Handlers that narrow their answer to one
// principal's sessions read it here and nowhere else.
func principalRestrictingRequest(r *http.Request) (string, bool) {
	principalID, ok := r.Context().Value(requestPrincipalContextKey{}).(string)
	return principalID, ok && principalID != ""
}

func withRestrictingPrincipal(r *http.Request, principalID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestPrincipalContextKey{}, principalID))
}

// authorizeAndServe is ServeHTTP when demo login is enabled.
func (s *Server) authorizeAndServe(w http.ResponseWriter, r *http.Request) {
	_, pattern := s.mux.Handler(r)
	if pattern == "" {
		// No route matches: the mux answers 404, 405 or a redirect, none of
		// which reaches a handler.
		s.mux.ServeHTTP(w, r)
		return
	}

	if presentedServiceToken, present := r.Header[http.CanonicalHeaderKey(serviceTokenHeader)]; present {
		if len(presentedServiceToken) != 1 || subtle.ConstantTimeCompare([]byte(presentedServiceToken[0]), []byte(s.cfg.ServiceToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid_service_token", serviceTokenHeader+" does not match this server's service token")
			return
		}
		s.mux.ServeHTTP(w, r)
		return
	}

	rule, classified := routeAccessRules[pattern]
	if !classified {
		writeJSONError(w, http.StatusForbidden, "route_not_classified", fmt.Sprintf(
			"route %q has no access rule, so with demo login enabled only the service token may call it", pattern))
		return
	}
	switch rule.class {
	case routeOpenToEveryone, routeHarnessCallback:
		s.mux.ServeHTTP(w, r)
		return
	}

	principalID, credentialError := s.principalOfRequest(r, rule)
	if credentialError != nil {
		credentialError.write(w)
		return
	}

	switch rule.class {
	case routeOperatorOnly:
		writeJSONError(w, http.StatusForbidden, "operator_route", fmt.Sprintf(
			"operator route: use the service token (%s: %s)", pattern, rule.reason))
		return
	case routePrincipalRefusedCannotBeFiltered:
		writeJSONError(w, http.StatusForbidden, "route_not_available_to_principals", fmt.Sprintf(
			"%s cannot be narrowed to one principal's sessions (%s), so it is refused rather than answered across principals; use the service token", pattern, rule.reason))
		return
	case routePrincipalOwnsSession:
		sessionID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		if !s.sessionIsOwnedByPrincipal(sessionID, principalID) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	case routePrincipalOwnsSignal:
		signalID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		signal, err := s.store.GetSignal(signalID)
		if err != nil || !s.sessionIsOwnedByPrincipal(signal.SessionID, principalID) {
			http.Error(w, "signal not found", http.StatusNotFound)
			return
		}
	case routePrincipalCreatesSession, routePrincipalSeesOwnSessionsOnly, routePrincipalReadsCatalog,
		routePrincipalOrSessionAgentThroughStoreProxy:
	default:
		writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", fmt.Sprintf("route %q has an access class this server does not handle", pattern))
		return
	}
	s.mux.ServeHTTP(w, withRestrictingPrincipal(r, principalID))
}

// sessionIsOwnedByPrincipal reports whether the session exists and was
// started as principalID. A session with no principal is owned by nobody.
func (s *Server) sessionIsOwnedByPrincipal(sessionID, principalID string) bool {
	if principalID == "" {
		return false
	}
	session, err := s.store.GetSession(sessionID)
	return err == nil && session.PrincipalID == principalID
}

// requestCredentialError is a refusal decided while establishing who a
// request comes from.
type requestCredentialError struct {
	status  int
	code    string
	message string
}

func (e *requestCredentialError) write(w http.ResponseWriter) {
	writeJSONError(w, e.status, e.code, e.message)
}

// principalOfRequest establishes which principal a request acts as, from its
// login cookie — or, on a route that accepts one, its session agent token.
func (s *Server) principalOfRequest(r *http.Request, rule routeAccessRule) (string, *requestCredentialError) {
	if _, hasAuthorization := r.Header["Authorization"]; hasAuthorization {
		if rule.class != routePrincipalOrSessionAgentThroughStoreProxy {
			return "", &requestCredentialError{http.StatusUnauthorized, "authorization_header_not_accepted", fmt.Sprintf(
				"an Authorization header is accepted only on %s/, as a session agent token; this route takes the login cookie or %s",
				kanbanProxyMountPrefix, serviceTokenHeader)}
		}
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return "", &requestCredentialError{http.StatusUnauthorized, "invalid_session_agent_token",
				"the Authorization header must be \"Bearer <session agent token>\""}
		}
		principalID, err := s.principalOfSessionAgentToken(strings.TrimSpace(token))
		if err != nil {
			return "", &requestCredentialError{http.StatusUnauthorized, "invalid_session_agent_token", err.Error()}
		}
		return principalID, nil
	}
	session, err := s.verifiedPrincipalSession(r)
	if err != nil {
		return "", &requestCredentialError{http.StatusUnauthorized, "not_logged_in", err.Error()}
	}
	return session.PrincipalID, nil
}

// pathValueForPattern extracts the named wildcard from an escaped request path
// that matched pattern, and unescapes it the way the mux does for PathValue.
// The mux's own PathValue is not available here: it is set only when the mux
// dispatches the request, and authorization runs before that.
func pathValueForPattern(pattern, escapedPath, pathValueName string) (string, error) {
	patternPath := pattern
	if _, afterMethod, hasMethod := strings.Cut(pattern, " "); hasMethod {
		patternPath = afterMethod
	}
	patternSegments := strings.Split(strings.TrimPrefix(patternPath, "/"), "/")
	pathSegments := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	for index, segment := range patternSegments {
		if segment != "{"+pathValueName+"}" {
			continue
		}
		if index >= len(pathSegments) {
			return "", fmt.Errorf("path %q is shorter than pattern %q", escapedPath, pattern)
		}
		value, err := url.PathUnescape(pathSegments[index])
		if err != nil {
			return "", fmt.Errorf("path segment %q of %q does not unescape: %w", pathSegments[index], escapedPath, err)
		}
		return value, nil
	}
	return "", errors.New("pattern " + pattern + " has no path value named " + pathValueName)
}
