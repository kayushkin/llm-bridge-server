package server

// Request authorization.
//
// This server is the whole backend of a multi-user product, and every request
// passes through authorizeAndServe before the mux dispatches it. There is no
// way to switch that off: the credentials it needs are mandatory at startup
// (config.ValidateRequestAuthorizationSettings).
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
//   - the internal service acting as a person: the same token plus
//     X-Principal-Id. dash is the browser's front door on this host and holds
//     the token, so it may say which of its logged-in users a request is for.
//     The request is then that principal's in every respect. X-Principal-Id
//     without the token is 401; it must never be believed on its own.
//   - a principal: a valid demo login cookie, or — on the identity-carrying
//     store proxies only — a session agent token (session_agent_token.go).
//     What a principal may do depends on the route's class.
//   - nobody: 401 on any route that is not open or a harness callback.
//
// Whatever principal a request resolves to is read from principal-store
// before the route's rule is applied (request_principal_lookup.go): a
// principal it does not know is 400, a disabled one is 403, and one it marks
// is_administrator is past every per-resource check below.

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
	// whatever check the handler itself makes, and nothing more.
	routeHarnessCallback
	// routeOperatorOnly needs the service token, or a principal
	// principal-store calls an administrator.
	routeOperatorOnly
	// routePrincipalCreatesSession is POST /sessions: the session is created
	// as the calling principal, whatever the body says.
	routePrincipalCreatesSession
	// routePrincipalOwnsSession names a session in a path value; a principal
	// reaches it only if the session's principal_id is theirs, and gets 404
	// — the same answer as a missing session — otherwise. An administrator
	// reaches every session, as does the service token.
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
	// routePrincipalCreatesOperation is POST /operations: the operation is
	// the calling principal's, whatever the body says, and the handler checks
	// the principal belongs to the organization.
	routePrincipalCreatesOperation
	// routePrincipalOwnsOperation names an operation in a path value; a
	// principal reaches it only if the operation is theirs, 404 otherwise.
	routePrincipalOwnsOperation
	// routePrincipalSeesOwnOperationsOnly lists operations; the handler
	// narrows the answer to the principal's own.
	routePrincipalSeesOwnOperationsOnly
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
func operationOwnedRoute(pathValueName string) routeAccessRule {
	return routeAccessRule{class: routePrincipalOwnsOperation, ownedResourcePathValueName: pathValueName, reason: "acts on one operation"}
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
	"GET /sessions/{id}/effective-config":              sessionOwnedRoute("id"),
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

	// Operations.
	"POST /operations":              {class: routePrincipalCreatesOperation, reason: "a principal starts operations as itself, in an organization it belongs to"},
	"GET /operations":               {class: routePrincipalSeesOwnOperationsOnly, reason: "operation list, narrowed in the store"},
	"GET /operations/{id}":          operationOwnedRoute("id"),
	"GET /operations/{id}/events":   operationOwnedRoute("id"),
	"POST /operations/{id}/cancel":  operationOwnedRoute("id"),
	"GET /operations/{id}/children": operationOwnedRoute("id"),
	"GET /operation-types":          catalogRoute("the operation types this server runs"),

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
	kanbanProxyMountPrefix + "/":     {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "kanban-store as the caller's principal"},
	grantStoreProxyMountPrefix + "/": {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "grant-store as the caller's principal"},

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
	"GET /hook-options":                            operatorRoute("hook registry vocabulary"),
	"GET /settings":                                operatorRoute("this server's own configuration, with where each value came from"),
	"PUT /settings/{key}":                          operatorRoute("changes how this server behaves"),
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
	"GET /effective-config":                        operatorRoute("what a session would be given, before it exists"),
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
	"POST /agents":                  operatorRoute("agent-store write"),
	"PUT /agents/{slug}":            operatorRoute("agent-store write"),
	"DELETE /agents/{slug}":         operatorRoute("agent-store write"),
	"GET /agents/{slug}/harnesses":  operatorRoute("agent-store harness bindings"),
	"POST /agents/{slug}/harnesses": operatorRoute("agent-store write"),
	"GET /agents/{slug}/config":     operatorRoute("agent-store runtime config"),
	"GET /configs":                  operatorRoute("agent-store runtime configs"),
	"GET /reconcile":                operatorRoute("agent-store reconcile"),
	"GET /files":                    operatorRoute("agent-store tracked context files"),
	"GET /files/{id}":               operatorRoute("agent-store tracked context files"),
	"GET /files/{id}/content":       operatorRoute("agent-store tracked context files"),
	"PUT /files/{id}/content":       operatorRoute("agent-store write"),
	"POST /files/{id}/enable":       operatorRoute("agent-store write"),
	"POST /files/{id}/disable":      operatorRoute("agent-store write"),
	"POST /files/scan":              operatorRoute("agent-store scan"),
	// agent-store's prompt source: the host prompt every harness receives.
	// Reading it shows the operator's whole prompt; writing it changes what
	// every session on this host is told.
	"GET /prompt-collections":                      operatorRoute("agent-store prompt source"),
	"POST /prompt-collections":                     operatorRoute("agent-store write"),
	"GET /prompt-collections/{id}":                 operatorRoute("agent-store prompt source"),
	"POST /prompt-collections/{id}/sections":       operatorRoute("agent-store write"),
	"POST /prompt-collections/{id}/render":         operatorRoute("agent-store write: rewrites prompt files on disk"),
	"POST /prompt-collections/{id}/outputs":        operatorRoute("agent-store write"),
	"GET /prompt-collections/{id}/revisions":       operatorRoute("agent-store prompt source"),
	"POST /prompt-collections/import-untracked":    operatorRoute("agent-store write"),
	"POST /prompt-outputs/{id}/enable":             operatorRoute("agent-store write"),
	"POST /prompt-outputs/{id}/disable":            operatorRoute("agent-store write"),
	"PUT /prompt-sections/{id}":                    operatorRoute("agent-store write"),
	"DELETE /prompt-sections/{id}":                 operatorRoute("agent-store write"),
	"GET /prompt-sections/{id}/revisions":          operatorRoute("agent-store prompt source"),
	"GET /prompt-drifts":                           operatorRoute("agent-store prompt source"),
	"POST /prompt-drifts/reconcile":                operatorRoute("agent-store write: may apply a file edit to the prompt"),
	"GET /prompt-drifts/{id}":                      operatorRoute("agent-store prompt source"),
	"GET /prompt-drifts/{id}/disk-content":         operatorRoute("agent-store prompt source"),
	"PUT /prompt-drifts/{id}/annotation":           operatorRoute("agent-store write"),
	"POST /prompt-drifts/{id}/apply":               operatorRoute("agent-store write: changes the prompt"),
	"POST /prompt-drifts/{id}/dismiss":             operatorRoute("agent-store write"),
	"GET /prompt-harness-deliveries":               operatorRoute("agent-store prompt source"),
	"PUT /prompt-harness-deliveries/{harness}":     operatorRoute("agent-store write: decides whether a harness gets the prompt"),
	"GET /prompt-delivery-options":                 operatorRoute("agent-store prompt source"),
	"GET /tracked-file-ignore-rules":               operatorRoute("agent-store tracked context files"),
	"POST /tracked-file-ignore-rules":              operatorRoute("agent-store write"),
	"POST /tracked-file-ignore-rules/{id}/enable":  operatorRoute("agent-store write"),
	"POST /tracked-file-ignore-rules/{id}/disable": operatorRoute("agent-store write"),
	"GET /files/{id}/versions":                     operatorRoute("agent-store tracked context files"),
	"GET /versions/{vid}/content":                  operatorRoute("agent-store tracked context files"),
	"GET /seed/profiles":                           operatorRoute("agent-store seed"),
	"GET /seed/profile":                            operatorRoute("agent-store seed"),
	"PUT /seed/profile":                            operatorRoute("agent-store seed"),
	"GET /seed/manifest":                           operatorRoute("agent-store seed"),
	"POST /seed/observe":                           operatorRoute("agent-store seed"),
	"POST /seed/drift":                             operatorRoute("agent-store seed"),
	"GET /seed/state":                              operatorRoute("agent-store seed"),
	"GET /context/resolve":                         operatorRoute("agent-store context"),

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

// requestCallerContextKey carries who an authorized request acts as.
type requestCallerContextKey struct{}

// requestCaller is who an authorized request acts as, as request
// authorization established it.
type requestCaller struct {
	// principalID is the principal the request acts as, and empty for the
	// internal service acting as itself.
	principalID string
	// isAdministrator is principal-store's own answer for principalID. An
	// administrator is past every per-resource check: every session whoever
	// owns it, every operator route, every list unfiltered, both store
	// proxies.
	isAdministrator bool
}

// narrowsToOnePrincipal reports whether this caller's answers must be limited
// to one principal's own records. The internal service and an administrator
// are identified but not narrowed.
func (caller requestCaller) narrowsToOnePrincipal() bool {
	return caller.principalID != "" && !caller.isAdministrator
}

func callerOfRequest(r *http.Request) (requestCaller, bool) {
	caller, ok := r.Context().Value(requestCallerContextKey{}).(requestCaller)
	return caller, ok
}

// principalRestrictingRequest returns the principal a request is restricted
// to, and "" with false when it is not restricted: the caller is the internal
// service acting as itself, or an administrator. Handlers that narrow their
// answer to one principal's sessions read it here and nowhere else.
//
// The id is empty whenever the bool is false, so a caller that reads only the
// id — a store filter where "" means every owner — cannot narrow an
// administrator to the sessions that happen to carry their own id.
// principalIdentityOfRequest is the one that names an administrator.
func principalRestrictingRequest(r *http.Request) (string, bool) {
	caller, ok := callerOfRequest(r)
	if !ok || !caller.narrowsToOnePrincipal() {
		return "", false
	}
	return caller.principalID, true
}

// principalIdentityOfRequest returns who the request acts as, whether or not
// that narrows what it may reach — an administrator is identified here and
// restricted nowhere. The store proxies set X-Principal-Id from this, because
// kanban-store and grant-store make their own administrator check.
func principalIdentityOfRequest(r *http.Request) (string, bool) {
	caller, ok := callerOfRequest(r)
	return caller.principalID, ok && caller.principalID != ""
}

func withRequestCaller(r *http.Request, caller requestCaller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestCallerContextKey{}, caller))
}

// authorizeAndServe is ServeHTTP: every request reaches the mux through here.
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
		assertedPrincipalHeaderName := principalIdentityHeaderNameIn(r.Header)
		if assertedPrincipalHeaderName == "" {
			// The internal service acting as itself: unrestricted, and not
			// held to the route table, so a route added later still answers it.
			s.mux.ServeHTTP(w, r)
			return
		}
		assertedPrincipalIDs := r.Header[assertedPrincipalHeaderName]
		if len(assertedPrincipalIDs) != 1 {
			writeJSONError(w, http.StatusBadRequest, "ambiguous_principal_header", fmt.Sprintf(
				"%s was sent %d times (%v); a request acts as one principal", assertedPrincipalHeaderName, len(assertedPrincipalIDs), assertedPrincipalIDs))
			return
		}
		caller, credentialError := s.callerAssertedByInternalService(r.Context(), strings.TrimSpace(assertedPrincipalIDs[0]))
		if credentialError != nil {
			credentialError.write(w)
			return
		}
		rule, classified := routeAccessRules[pattern]
		if !classified {
			// The fence in front of an unclassified route is not about who is
			// calling: nobody has decided who may call it yet, and neither an
			// asserted principal nor an administrator decides it by arriving.
			// The service token acting as itself is the only way past.
			refuseUnclassifiedRoute(w, pattern)
			return
		}
		s.serveAsCaller(w, r, pattern, rule, caller)
		return
	}

	// X-Principal-Id is a claim about who the caller is, and only the service
	// token makes it believable. Refused here, before the route's class is
	// looked at, so there is no route on which it could be believed.
	if headerName := principalIdentityHeaderNameIn(r.Header); headerName != "" {
		writeJSONError(w, http.StatusUnauthorized, "principal_header_without_service_token", fmt.Sprintf(
			"%s says which principal a request acts as and is believed only from a caller presenting %s; log in with POST /auth/demo-login instead",
			headerName, serviceTokenHeader))
		return
	}

	rule, classified := routeAccessRules[pattern]
	if !classified {
		refuseUnclassifiedRoute(w, pattern)
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
	caller, credentialError := s.callerActingAsPrincipal(r.Context(), principalID)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	s.serveAsCaller(w, r, pattern, rule, caller)
}

// refuseUnclassifiedRoute answers a route nobody has written a rule for.
func refuseUnclassifiedRoute(w http.ResponseWriter, pattern string) {
	writeJSONError(w, http.StatusForbidden, "route_not_classified", fmt.Sprintf(
		"route %q has no access rule, so only the service token may call it", pattern))
}

// serveAsCaller applies the route's rule to a caller acting as a principal and
// dispatches, or writes the refusal the rule decides on.
func (s *Server) serveAsCaller(w http.ResponseWriter, r *http.Request, pattern string, rule routeAccessRule, caller requestCaller) {
	switch rule.class {
	case routeOpenToEveryone, routeHarnessCallback:
		s.mux.ServeHTTP(w, withRequestCaller(r, caller))
		return
	}
	if caller.isAdministrator {
		// principal-store says this person may do anything here, so no
		// per-resource check applies. The identity still travels with the
		// request: the store proxies set X-Principal-Id from it, and
		// kanban-store and grant-store make their own administrator check.
		s.mux.ServeHTTP(w, withRequestCaller(r, caller))
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
		if !s.sessionIsOwnedByPrincipal(sessionID, caller.principalID) {
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
		if err != nil || !s.sessionIsOwnedByPrincipal(signal.SessionID, caller.principalID) {
			http.Error(w, "signal not found", http.StatusNotFound)
			return
		}
	case routePrincipalOwnsOperation:
		operationID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		if s.operations != nil && !s.operationIsOwnedByPrincipal(operationID, caller.principalID) {
			http.Error(w, "operation not found", http.StatusNotFound)
			return
		}
	case routePrincipalCreatesSession, routePrincipalSeesOwnSessionsOnly, routePrincipalReadsCatalog,
		routePrincipalOrSessionAgentThroughStoreProxy, routePrincipalCreatesOperation, routePrincipalSeesOwnOperationsOnly:
	default:
		writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", fmt.Sprintf("route %q has an access class this server does not handle", pattern))
		return
	}
	s.mux.ServeHTTP(w, withRequestCaller(r, caller))
}

// principalIdentityHeaderNameIn returns the key a request carries
// X-Principal-Id under, in whatever casing it was set, and "" when it carries
// none. Header.Get would miss a non-canonical key set directly on the map, and
// a header this server refuses to believe must be found however it is spelled.
func principalIdentityHeaderNameIn(header http.Header) string {
	for key, values := range header {
		if !strings.EqualFold(key, principalIdentityHeader) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return key
			}
		}
	}
	return ""
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
				"an Authorization header is accepted only on %s/ and %s/, as a session agent token; this route takes the login cookie or %s",
				kanbanProxyMountPrefix, grantStoreProxyMountPrefix, serviceTokenHeader)}
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
