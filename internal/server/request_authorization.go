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
//   - a principal: a login cookie naming a live login session
//     (login_sessions.go) — which must also carry the session's CSRF token on
//     anything but GET, HEAD and OPTIONS — or, on the identity-carrying store
//     proxies only, a session agent token (session_agent_token.go). What a
//     principal may do depends on the route's class.
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

// peerTokenHeader carries a peer token: another llm-bridge-server acting here
// as the peer principal this server issued the token for (peers.go).
const peerTokenHeader = "X-LLM-Bridge-Peer-Token"

// peerUserHeader names, beside a peer token, which of the calling server's
// own principals a request is for: its principal-store id there (phase 2 of
// design note d107154a). This server acts for that user as the local
// principal its administrator mapped the pair to (peer_users.go), allowed only
// what both principals' grants allow. It means nothing without a peer token,
// and the calling server sets it itself, never from a client's header.
const peerUserHeader = "X-LLM-Bridge-Peer-User"

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
	// routePeerConnectionCall is called by a connected llm-bridge-server about
	// the connection itself, never with a peer token: the handler checks the
	// connection's credential, or the socket the call arrived over
	// (peers.go).
	routePeerConnectionCall
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
	// routeSessionOwnerOrItsAgentPostsIntoSession puts something into the
	// session in a path value: a shared file, or an action for a person to
	// confirm. A principal reaches it as on routePrincipalOwnsSession. So does
	// the session's own agent, presenting its session posting token
	// (session_posting_token.go) as a Bearer token — for that session and no
	// other. The token is accepted on no other class.
	routeSessionOwnerOrItsAgentPostsIntoSession
	// routePrincipalManagesOwnLoginSessions lists or revokes login sessions;
	// the handler narrows a principal to its own, and lets an administrator
	// reach anyone's.
	routePrincipalManagesOwnLoginSessions
	// routePrincipalActsThroughPeerConnection is the connection proxy: any
	// principal of this server may call a connected one through it, named
	// there as itself (peerUserHeader) unless it is an administrator, who
	// acts as this whole server everywhere but in starting a session. A peer
	// may not go on through it to a third server.
	routePrincipalActsThroughPeerConnection
	// routePrincipalSeesPeerConnectionsItMayCallThrough reads this server's
	// connections to others: an administrator every one, anyone else only
	// those its can_call_through grants name, which the handler narrows to.
	// Holding none shows none, whatever the kind.
	routePrincipalSeesPeerConnectionsItMayCallThrough
)

// sessionAgentDelegateAccess says whether, and how, a session's own agent may
// call a route with its session posting token to start and drive delegates:
// sessions it creates under itself (manager_session_id), as bridge-agent does.
// The agent acts with exactly the principal its own session runs with, and
// reaches only its own delegates. Every route without one refuses the token as
// before, except routeSessionOwnerOrItsAgentPostsIntoSession.
type sessionAgentDelegateAccess int

const (
	// sessionAgentNotAccepted is every route but the few below.
	sessionAgentNotAccepted sessionAgentDelegateAccess = iota
	// sessionAgentCreatesItsDelegate is POST /sessions: the new session sits
	// under the agent's session and runs as that session's principal.
	sessionAgentCreatesItsDelegate
	// sessionAgentReachesItsDelegate names a session in a path value that the
	// agent's session must manage.
	sessionAgentReachesItsDelegate
	// sessionAgentReachesItsDelegateOrItself is the same, and also reaches the
	// agent's own session.
	sessionAgentReachesItsDelegateOrItself
	// sessionAgentReadsCatalog is a catalog a delegate's start depends on.
	sessionAgentReadsCatalog
	// sessionAgentReadsSessions is a read-only session route — the list,
	// full-text search, one session and its history — that a session's agent
	// reads as its own session's principal: every session when that session
	// runs with no principal, as the internal service does, and otherwise
	// exactly what the principal (and, for a session a connected server
	// started, that server too) may read. It never sends, stops or changes
	// anything, and never answers through the public reverse proxy.
	sessionAgentReadsSessions
	// sessionAgentUsesPeerMessaging lists connected servers and their messages
	// and sends one, as an administrator would, for an agent whose session runs
	// with no principal or as an administrator. It never approves or declines:
	// that is a person's decision.
	sessionAgentUsesPeerMessaging
)

// routeAccessRule is one route's classification.
type routeAccessRule struct {
	class routeAccessClass
	// sessionAgentAccess says whether a session's agent may call the route
	// with its posting token to start and drive its delegates.
	sessionAgentAccess sessionAgentDelegateAccess
	// ownedResourcePathValueName names the path value holding the session id
	// (routePrincipalOwnsSession) or the signal id (routePrincipalOwnsSignal).
	ownedResourcePathValueName string
	// reason says why the route is in its class, for the next reader and for
	// the refusal message.
	reason string
	// answersThroughPublicProxy lets a routeHarnessCallback route answer a
	// request that came through the public reverse proxy. Only the runner's
	// routes set it, because their handlers check the runner's token or its
	// one-time passphrase themselves; every other harness callback takes no
	// credential and is for this host's own processes alone.
	answersThroughPublicProxy bool
}

func openRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeOpenToEveryone, reason: reason}
}
func harnessCallbackRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeHarnessCallback, reason: reason}
}

// runnerRoute is a harness callback a remote runner calls, which may arrive
// through the public reverse proxy because its handler checks the runner's
// own credential.
func runnerRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeHarnessCallback, reason: reason, answersThroughPublicProxy: true}
}
func peerConnectionCallRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routePeerConnectionCall, reason: reason}
}
func peerConnectionProxyRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routePrincipalActsThroughPeerConnection, reason: reason}
}
func peerConnectionReadRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routePrincipalSeesPeerConnectionsItMayCallThrough, reason: reason}
}
func operatorRoute(reason string) routeAccessRule {
	return routeAccessRule{class: routeOperatorOnly, reason: reason}
}
func sessionAgentPostingRoute(pathValueName, reason string) routeAccessRule {
	return routeAccessRule{class: routeSessionOwnerOrItsAgentPostsIntoSession, ownedResourcePathValueName: pathValueName, reason: reason}
}
func sessionOwnedRoute(pathValueName string) routeAccessRule {
	return routeAccessRule{class: routePrincipalOwnsSession, ownedResourcePathValueName: pathValueName, reason: "acts on one session"}
}

// delegateDrivenSessionRoute is sessionOwnedRoute that a session's agent may
// also call, with its posting token, on a delegate it started.
func delegateDrivenSessionRoute(pathValueName string, access sessionAgentDelegateAccess) routeAccessRule {
	return routeAccessRule{class: routePrincipalOwnsSession, ownedResourcePathValueName: pathValueName, sessionAgentAccess: access,
		reason: "acts on one session, as its owner or as the agent of the session that started it"}
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

// sessionReadRoute is a read-only session route a session's agent may also
// call with its posting token, reading as its own session's principal.
func sessionReadRoute(rule routeAccessRule) routeAccessRule {
	rule.sessionAgentAccess = sessionAgentReadsSessions
	rule.reason += "; a session's agent reads it as its own session's principal"
	return rule
}

// peerMessagingRoute is an operator route a session's agent may also call
// with its posting token to read connections and messages and send one.
func peerMessagingRoute(rule routeAccessRule) routeAccessRule {
	rule.sessionAgentAccess = sessionAgentUsesPeerMessaging
	rule.reason += "; a session's agent with no principal, or an administrator's, may call it with its posting token"
	return rule
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
	"GET /health":                                                 openRoute("liveness for load balancers and healthcheck"),
	"POST /auth/demo-login":                                       openRoute("how a principal obtains a login; answers 404 itself when switched off"),
	"GET /auth/entra/start":                                       openRoute("sends the browser to Entra ID to sign in"),
	"GET /auth/entra/callback":                                    openRoute("Entra ID sends the browser back here; the handler verifies everything it is given"),
	"GET /auth/principal":                                         openRoute("answers 401 itself without a live login session"),
	"POST /auth/logout":                                           openRoute("revokes the cookie's live session, checking its CSRF token itself, and clears a dead cookie with none"),
	"POST /peer-connection-invites":                               operatorRoute("lets another server connect to this one"),
	"GET /peer-connection-invites":                                operatorRoute("who was invited to connect, and by whom"),
	"POST /peer-connections":                                      operatorRoute("connects to another server and stores a credential for it"),
	"GET /peer-connections":                                       peerMessagingRoute(peerConnectionReadRoute("which servers this one is connected to, and the access each way; anyone but an administrator sees only those its can_call_through grants name")),
	"POST /peer-connections/{id}/messages":                        peerMessagingRoute(operatorRoute("sends the other server a request, notice or status")),
	"GET /peer-messages":                                          peerMessagingRoute(operatorRoute("messages to and from connected servers")),
	"GET /peer-messages/{id}":                                     peerMessagingRoute(operatorRoute("one message to or from a connected server")),
	"POST /peer-messages/{id}/approve":                            operatorRoute("approves a connected server's request (starting its session) or notice (running the update); never an agent"),
	"POST /peer-messages/{id}/decline":                            operatorRoute("declines a connected server's request or notice and tells it why"),
	"POST /peer/messages":                                         peerConnectionCallRoute("the other server delivers a message; the handler checks the connection and what it may send"),
	"POST /peer-message-attachments":                              peerMessagingRoute(operatorRoute("stores a file to send to a connected server with the next message")),
	"GET /peer-messages/{id}/attachments/{attachment_id}":         peerMessagingRoute(operatorRoute("downloads a file of a message; an agent only of a message it sent or whose approval started its session")),
	"POST /peer-messages/{id}/attachments/{attachment_id}/retry":  operatorRoute("fetches a file of an incoming message again now"),
	"GET /peer/messages/{message_id}/attachments/{attachment_id}": peerConnectionCallRoute("the other server fetches a file of a message this server sent it; the handler checks the connection"),
	"GET /peer-connections/{id}":                                  peerConnectionReadRoute("one connection and the access each way; anyone but an administrator reaches only one its can_call_through grants name"),
	"POST /peer-connections/{id}/remove":                          operatorRoute("ends a connection and the access each way"),
	"POST /peer-connections/{id}/grant":                           operatorRoute("gives another server access to this one"),
	"POST /peer-connections/{id}/revoke-grant":                    operatorRoute("takes another server's access away"),
	"POST /peer-connections/{id}/check":                           operatorRoute("calls another server with a stored token"),
	"/peer-connections/{id}/proxy/{rest...}":                      peerConnectionProxyRoute("acts on another server: as this server's peer principal for an administrator, and for anyone else as that person, whom the other server maps to one of its own principals"),
	"GET /peer-access-levels":                                     operatorRoute("the levels of access this server can give"),
	"POST /peer-access-levels/{key}/provision":                    operatorRoute("makes a level's group and its grants"),
	"POST /peer/connect":                                          openRoute("the other server has no credential yet; the one-time passphrase is the credential, checked by the handler"),
	"GET /peer/socket":                                            peerConnectionCallRoute("the dial-out socket; the handler checks the connection's credential"),
	"POST /peer/access":                                           peerConnectionCallRoute("the other server delivers a token; the handler checks the connection"),
	"POST /peer/access/revoked":                                   peerConnectionCallRoute("the other server takes a token back; the handler checks the connection"),
	"POST /peer/connection/removed":                               peerConnectionCallRoute("the other server says it removed the connection; the handler checks the connection"),
	"GET /peer-user-mappings":                                     operatorRoute("which connected server's users act as which principal here"),
	"POST /peer-user-mappings":                                    operatorRoute("lets a connected server's user act here as one of this server's principals"),
	"POST /peer-user-mappings/{id}/remove":                        operatorRoute("takes that away"),
	"GET /peer/self":                                              catalogRoute("tells a caller who it is; the handler answers only a peer token"),
	"GET /peer/server-name":                                       peerConnectionCallRoute("a connected server asks this one's name and gives its own; the handler checks the connection"),
	"GET /server":                                                 catalogRoute("what this server calls itself, which the chat draws as the heading of its sessions"),
	"GET /auth/sessions":                                          {class: routePrincipalManagesOwnLoginSessions, reason: "a principal's own login sessions; an administrator names anyone's"},
	"POST /auth/sessions/{id}/revoke":                             {class: routePrincipalManagesOwnLoginSessions, reason: "ends a principal's own login session; an administrator ends anyone's"},
	"GET /auth/login-attempts":                                    operatorRoute("every sign-in attempt, with the identities of people refused"),

	// Harness callbacks: called by processes this server spawned or by
	// runners, none of which carry a login.
	"POST /permission/cc-prehook/{bridge_id}":    harnessCallbackRoute("Claude Code PreToolUse hook, a curl from the harness child"),
	"POST /permission/codex-prehook/{bridge_id}": harnessCallbackRoute("codex PreToolUse hook, a curl from the harness child"),
	"POST /sidecar/event/{bridge_id}":            harnessCallbackRoute("OTel sidecar of a pty session posts translated events"),
	"POST /hooks/exec/{id}":                      harnessCallbackRoute("registered hook fired by the harness child via curl"),
	"GET /api/runner/ws":                         runnerRoute("runner WebSocket; the handler authenticates the runner's bearer token"),
	"POST /api/runner/enroll":                    runnerRoute("runner enrollment; the handler checks the single-use passphrase"),
	"GET /api/runner/install.sh":                 runnerRoute("bootstrap script fetched by a machine before it has a runner token"),
	"GET /api/runner/binary":                     runnerRoute("runner and harness binaries fetched by a machine during bootstrap"),
	"/api/agent-store/":                          runnerRoute("seed proxy for runners; the handler authenticates the runner's bearer token"),
	"/api/skill-store/":                          runnerRoute("seed proxy for runners; the handler authenticates the runner's bearer token"),
	"/api/harness-proxy/{harness}/{rest...}":     harnessCallbackRoute("harness backend proxy for wrappers on runners; unauthenticated today (noteboard card f4e5e1ef)"),

	// Sessions.
	"POST /sessions":              {class: routePrincipalCreatesSession, sessionAgentAccess: sessionAgentCreatesItsDelegate, reason: "a principal creates sessions as itself, and a session's agent its delegates as its own session's principal"},
	"GET /sessions":               sessionReadRoute(ownSessionsOnlyRoute("session list, narrowed in the store")),
	"GET /sessions/summary":       ownSessionsOnlyRoute("sidebar list, narrowed in the store"),
	"POST /sessions/summary":      ownSessionsOnlyRoute("sidebar list, narrowed in the store"),
	"GET /sessions/recent-bundle": ownSessionsOnlyRoute("recent sessions warmed for the chat page, narrowed in the store"),
	"GET /sessions/validators":    ownSessionsOnlyRoute("staleness check; ids not owned by the caller are dropped before log-store is asked"),
	"POST /sessions/validators":   ownSessionsOnlyRoute("staleness check; ids not owned by the caller are dropped before log-store is asked"),
	"GET /session-events":         ownSessionsOnlyRoute("session list stream; frames about other principals' sessions are not written"),
	"GET /signals":                ownSessionsOnlyRoute("cross-session signal inbox, narrowed in the store"),
	"GET /sessions/search":        sessionReadRoute(cannotBeFilteredRoute("full-text search answered by log-store across every session")),
	"GET /sessions/aggregates":    cannotBeFilteredRoute("aggregates computed by log-store across every session"),
	"GET /sessions/discover":      operatorRoute("imports sessions found on this host's disk"),

	"GET /sessions/{id}":                                  sessionReadRoute(sessionOwnedRoute("id")),
	"POST /sessions/{id}/send":                            delegateDrivenSessionRoute("id", sessionAgentReachesItsDelegate),
	"POST /sessions/{id}/files":                           sessionAgentPostingRoute("id", "shares a file into one session, as its owner or as its own agent"),
	"GET /sessions/{id}/files":                            sessionOwnedRoute("id"),
	"GET /sessions/{id}/files/{file_id}/content":          sessionOwnedRoute("id"),
	"GET /sessions/{id}/files-named-by-tools":             sessionOwnedRoute("id"),
	"GET /sessions/{id}/files-named-by-tools/content":     sessionOwnedRoute("id"),
	"POST /sessions/{id}/actions":                         sessionAgentPostingRoute("id", "offers an action in one session, as its owner or as its own agent"),
	"POST /sessions/{id}/images":                          sessionAgentPostingRoute("id", "makes images and shares them into one session, as its owner or as its own agent"),
	"GET /sessions/{id}/actions":                          sessionOwnedRoute("id"),
	"POST /sessions/{id}/actions/{action_id}/run":         sessionOwnedRoute("id"),
	"GET /sessions/{id}/events":                           delegateDrivenSessionRoute("id", sessionAgentReachesItsDelegate),
	"GET /sessions/{id}/attach":                           sessionOwnedRoute("id"),
	"GET /sessions/{id}/attach-token":                     sessionOwnedRoute("id"),
	"GET /sessions/{id}/messages":                         sessionReadRoute(sessionOwnedRoute("id")),
	"GET /sessions/{id}/messages/raw":                     sessionOwnedRoute("id"),
	"GET /sessions/{id}/entries/{eventId}":                sessionOwnedRoute("id"),
	"GET /sessions/{id}/entries/{eventId}/images/{index}": sessionOwnedRoute("id"),
	"POST /sessions/{id}/interrupt":                       sessionOwnedRoute("id"),
	"POST /sessions/{id}/resume":                          sessionOwnedRoute("id"),
	"POST /sessions/{id}/stop":                            delegateDrivenSessionRoute("id", sessionAgentReachesItsDelegate),
	"POST /sessions/{id}/mode":                            sessionOwnedRoute("id"),
	"POST /sessions/{id}/compact":                         sessionOwnedRoute("id"),
	"POST /sessions/{id}/fork":                            sessionOwnedRoute("id"),
	"POST /sessions/{id}/rename":                          sessionOwnedRoute("id"),
	"POST /sessions/{id}/config":                          sessionOwnedRoute("id"),
	"POST /sessions/{id}/mark-done":                       sessionOwnedRoute("id"),
	"GET /sessions/{id}/git/repos":                        sessionOwnedRoute("id"),
	"GET /sessions/{id}/git":                              sessionOwnedRoute("id"),
	"GET /sessions/{id}/effective-config":                 sessionOwnedRoute("id"),
	"GET /sessions/{id}/hooks/pending":                    sessionOwnedRoute("id"),
	"POST /sessions/{id}/hooks/{request_id}/resolve":      sessionOwnedRoute("id"),
	"GET /sessions/{id}/signals":                          sessionOwnedRoute("id"),
	"POST /sessions/{id}/signals":                         sessionOwnedRoute("id"),
	"GET /sessions/{id}/tools/{tool_use_id}/snapshots":    sessionOwnedRoute("id"),
	"PUT /sessions/{id}/folder":                           operatorRoute("files a session into the shared folder registry"),
	"PUT /sessions/{id}/starter":                          operatorRoute("records which service started a session as another principal, letting it drive the session"),
	"POST /service-principal-tokens":                      operatorRoute("issues a token a service principal calls this server with"),
	"GET /service-principal-tokens":                       operatorRoute("every service principal token's record; never a token"),
	"GET /self":                                           catalogRoute("tells any caller who it acts as"),
	"POST /service-principal-tokens/{id}/revoke":          operatorRoute("stops a service principal token working"),
	"PUT /sessions/{id}/permission-mode":                  operatorRoute("can switch a session to bypass, skipping permission-store"),
	"PUT /sessions/{id}/bypass-permissions":               operatorRoute("can switch a session to bypass, skipping permission-store"),
	"POST /signals/{id}/resolve":                          signalOwnedRoute("id"),
	"POST /signals/{id}/answer":                           signalOwnedRoute("id"),
	"GET /snapshots/blob/{sha}":                           cannotBeFilteredRoute("content-addressed blob shared across sessions; nothing ties a sha to one session"),

	// Operations.
	"POST /operations":                            {class: routePrincipalCreatesOperation, reason: "a principal starts operations as itself, in an organization it belongs to"},
	"GET /operations":                             {class: routePrincipalSeesOwnOperationsOnly, reason: "operation list, narrowed in the store"},
	"GET /operations/{id}":                        operationOwnedRoute("id"),
	"GET /operations/{id}/events":                 operationOwnedRoute("id"),
	"POST /operations/{id}/cancel":                operationOwnedRoute("id"),
	"GET /operations/{id}/children":               operationOwnedRoute("id"),
	"GET /operation-types":                        openRoute("names the operation types this server runs and their limits, nothing about anyone; grant-store reads it without a credential to check an operation_type grant"),
	"GET /operation-budgets":                      operatorRoute("every organization's spending"),
	"GET /operation-budgets/{organization_id}":    operatorRoute("an organization's spending"),
	"PUT /operation-budgets/{organization_id}":    operatorRoute("sets an organization's monthly limit"),
	"DELETE /operation-budgets/{organization_id}": operatorRoute("removes an organization's monthly limit"),

	// Catalogs a chat UI reads.
	"GET /harnesses":                     catalogRoute("harness types"),
	"GET /harnesses/{name}/capabilities": catalogRoute("what a harness type supports"),
	"GET /harnesses/{name}/agents":       catalogRoute("agents available on a harness type"),
	"/images/":                           catalogRoute("harness images"),
	"GET /session-taxonomy":              catalogRoute("session type and purpose vocabulary"),
	"GET /instances":                     {class: routePrincipalReadsCatalog, sessionAgentAccess: sessionAgentReadsCatalog, reason: "instances to start a session on; narrowed to can_dispatch_on grants and without machine details"},
	"GET /agents":                        catalogRoute("agent-store agents, unfiltered by can_run_as"),
	"GET /agents/{slug}":                 catalogRoute("one agent-store agent, unfiltered by can_run_as"),

	// Identity-carrying store proxies.
	kanbanProxyMountPrefix + "/":      {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "kanban-store as the caller's principal"},
	kanbanStoreProxyMountPrefix + "/": {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "kanban-store, rooted, as the caller's principal"},
	grantStoreProxyMountPrefix + "/":  {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "grant-store as the caller's principal"},
	mailstackProxyMountPrefix + "/":   {class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "mailstack as the caller's principal"},

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
	"POST /oneshot":                                operatorRoute("runs a model call outside any session"),
	"POST /images":                                 operatorRoute("makes images outside any session, on the operator's subscription"),
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
	"PUT /machines/{id}/computer-use":              operatorRoute("machine registry"),
	"DELETE /machines/{id}":                        operatorRoute("machine registry"),
	"POST /api/runner/seed/broadcast":              operatorRoute("tells every runner to reconcile"),

	// Identity bindings in principal-store: who an Entra identity signs in as.
	"GET /admin/identity-bindings":              operatorRoute("which outside identity signs in as which principal"),
	"POST /admin/identity-bindings":             operatorRoute("decides who an outside identity signs in as"),
	"POST /admin/identity-bindings/{id}/unlink": operatorRoute("stops an outside identity signing in, and ends its login sessions"),

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
// peerCaller is what a peer token says about the server presenting it: the
// grant it was issued by, the connection that grant is on, its level, and the
// peer principal it acts as. When the request also names one of the calling
// server's users (peerUserHeader), remoteUserPrincipalID is that user's id
// there and mappingID the mapping that made it one of this server's
// principals; the request's principalID is then the mapped local principal,
// and peerPrincipalID still limits what it may do.
type peerCaller struct {
	grantID         string
	connectionID    string
	level           string
	peerPrincipalID string
	// peerIsAdministrator is principal-store's answer for the peer principal.
	peerIsAdministrator   bool
	remoteUserPrincipalID string
	mappingID             string
}

// actsForRemoteUser reports whether a peer request names one of the calling
// server's users and so acts as a mapped local principal.
func (peer *peerCaller) actsForRemoteUser() bool {
	return peer != nil && peer.remoteUserPrincipalID != ""
}

type requestCaller struct {
	// principalID is the principal the request acts as, and empty for the
	// internal service acting as itself.
	principalID string
	// isAdministrator is principal-store's own answer for principalID. An
	// administrator is past every per-resource check: every session whoever
	// owns it, every operator route, every list unfiltered, both store
	// proxies.
	isAdministrator bool
	// loginSessionID is the login session whose cookie authenticated the
	// request, and empty for every other credential.
	loginSessionID string
	// peer is set when another llm-bridge-server presented a peer token;
	// principalID is then the peer principal the token acts as.
	peer *peerCaller
	// postingAgentOfSessionID is set, and nothing else is, when the caller
	// is a session's own agent presenting its session posting token. It reaches
	// only routeSessionOwnerOrItsAgentPostsIntoSession, for this session.
	postingAgentOfSessionID string
	// delegatingAgentOfSessionID is set when the caller is a session's agent
	// presenting its posting token on a route that lets it start or drive its
	// delegates. principalID is then the principal that session runs with,
	// empty when it runs with none.
	delegatingAgentOfSessionID string
	// observesEverySession is set on a read-only route that narrows to one
	// principal's sessions when the caller's can_observe_sessions_on grants
	// name every instance: sessionScopeRestrictingRequest then narrows
	// nothing.
	observesEverySession bool
}

// sessionScopeOfCaller is the sessions a restricted caller reaches by
// ownership (peer_users.go).
func sessionScopeOfCaller(caller requestCaller) sessionScope {
	scope := sessionScope{principalID: caller.principalID}
	if caller.peer.actsForRemoteUser() {
		scope.throughPeerPrincipalID = caller.peer.peerPrincipalID
	}
	return scope
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

	presentedPeerTokens, peerTokenPresent := r.Header[http.CanonicalHeaderKey(peerTokenHeader)]
	presentedPeerUsers, peerUserPresent := r.Header[http.CanonicalHeaderKey(peerUserHeader)]
	if peerUserPresent && !peerTokenPresent {
		writeJSONError(w, http.StatusBadRequest, "peer_user_without_peer_token", fmt.Sprintf(
			"%s names a connected server's user and means nothing without %s", peerUserHeader, peerTokenHeader))
		return
	}
	if _, serviceTokenPresent := r.Header[http.CanonicalHeaderKey(serviceTokenHeader)]; peerTokenPresent && serviceTokenPresent {
		writeJSONError(w, http.StatusBadRequest, "ambiguous_credentials", fmt.Sprintf(
			"%s and %s were both sent; a request comes from one caller", serviceTokenHeader, peerTokenHeader))
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
			"%s says which principal a request acts as and is believed only from a caller presenting %s; sign in with GET /auth/entra/start or POST /auth/demo-login instead",
			headerName, serviceTokenHeader))
		return
	}

	rule, classified := routeAccessRules[pattern]
	if !classified {
		refuseUnclassifiedRoute(w, pattern)
		return
	}

	// The harness callbacks take no credential: they are for harness children,
	// sidecars and runner tunnels on this host. A request that came through the
	// public reverse proxy carries the headers it sets, and is refused here
	// whatever else it presents, so a mistake in the proxy's rules cannot open
	// them. The runner's own routes are exempt (answersThroughPublicProxy).
	if rule.class == routeHarnessCallback && !rule.answersThroughPublicProxy && requestCameThroughPublicProxy(r) {
		writeJSONError(w, http.StatusForbidden, "harness_callback_not_public", fmt.Sprintf(
			"%s is called by this host's own processes and is not answered through the public proxy", pattern))
		return
	}

	// A request another server sent over a peer socket acts only as the peer
	// its token names, or is a connection call: the routes open to everyone
	// and the harness callbacks are for this host's own callers, never for a
	// peer, however it arrived.
	socketConnectionID, arrivedOverPeerSocket := peerSocketConnectionOfRequest(r)
	if arrivedOverPeerSocket && !peerTokenPresent && rule.class != routePeerConnectionCall {
		writeJSONError(w, http.StatusForbidden, "peer_socket_requires_peer_token", fmt.Sprintf(
			"a request over a peer socket must present %s, or be a connection call; %s is neither", peerTokenHeader, pattern))
		return
	}

	// Another llm-bridge-server acting as the peer principal its token was
	// issued for. From here it is held to the route rules exactly as a person
	// signed in as that principal would be.
	if peerTokenPresent {
		if len(presentedPeerTokens) != 1 || strings.TrimSpace(presentedPeerTokens[0]) == "" {
			writeJSONError(w, http.StatusBadRequest, "ambiguous_peer_token", fmt.Sprintf(
				"%s was sent %d times or empty; a request presents one peer token", peerTokenHeader, len(presentedPeerTokens)))
			return
		}
		if _, hasAuthorization := r.Header["Authorization"]; hasAuthorization {
			writeJSONError(w, http.StatusBadRequest, "ambiguous_credentials", fmt.Sprintf(
				"%s and an Authorization header were both sent; a request comes from one caller", peerTokenHeader))
			return
		}
		if !arrivedOverPeerSocket && s.cfg.DemoLoginEnabled {
			writeJSONError(w, http.StatusConflict, "demo_login_enabled",
				"a peer token by https is refused while demo login is on: it is a call from another machine, and demo login signs anyone in as anyone; a peer reaches this server over the socket this server dialed")
			return
		}
		if rule.class == routeHarnessCallback || rule.class == routePeerConnectionCall {
			writeJSONError(w, http.StatusForbidden, "route_not_for_peers", fmt.Sprintf(
				"%s is called by this host's own processes or as a connection call, never with a peer token", pattern))
			return
		}
		caller, credentialError := s.callerOfPeerToken(r.Context(), presentedPeerTokens[0], socketConnectionID, arrivedOverPeerSocket)
		if credentialError != nil {
			credentialError.write(w)
			return
		}
		if !arrivedOverPeerSocket {
			// Over a socket, serveRequest has already read and stamped the id and name.
			s.noteOtherServerIdentity(caller.peer.connectionID, r.Header)
			s.stampThisServersIdentity(w.Header())
		}
		if peerUserPresent {
			if len(presentedPeerUsers) != 1 || strings.TrimSpace(presentedPeerUsers[0]) == "" {
				writeJSONError(w, http.StatusBadRequest, "ambiguous_peer_user", fmt.Sprintf(
					"%s was sent %d times or empty; a request names one user", peerUserHeader, len(presentedPeerUsers)))
				return
			}
			caller, credentialError = s.callerForPeerUser(r.Context(), caller, strings.TrimSpace(presentedPeerUsers[0]))
			if credentialError != nil {
				credentialError.write(w)
				return
			}
		}
		s.serveAsCaller(w, r, pattern, rule, caller)
		return
	}

	switch rule.class {
	case routeOpenToEveryone, routeHarnessCallback, routePeerConnectionCall:
		s.mux.ServeHTTP(w, r)
		return
	}

	// A session agent token and a service principal token go to the principal
	// path below on every route; any other bearer on these routes is a session
	// posting token.
	_, hasAuthorization := r.Header["Authorization"]
	presentedBearer := bearerTokenOf(r)
	presentsPostingToken := hasAuthorization && !isSessionAgentToken(presentedBearer) && !isServicePrincipalToken(presentedBearer)
	if presentsPostingToken && rule.class == routeSessionOwnerOrItsAgentPostsIntoSession {
		caller, credentialError := s.postingAgentOfRequest(r, pattern, rule)
		if credentialError != nil {
			credentialError.write(w)
			return
		}
		s.mux.ServeHTTP(w, withRequestCaller(r, caller))
		return
	}

	if presentsPostingToken && rule.sessionAgentAccess != sessionAgentNotAccepted {
		s.serveDelegatingAgent(w, r, pattern, rule)
		return
	}

	principalID, loginSessionID, sessionAgentTokenSessionID, credentialError := s.principalOfRequest(r, rule)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	caller, credentialError := s.callerActingAsPrincipal(r.Context(), principalID)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	if sessionAgentTokenSessionID != "" && rule.class != routePrincipalOrSessionAgentThroughStoreProxy {
		if caller, credentialError = s.administratorOfSessionAgentToken(r, pattern, caller, sessionAgentTokenSessionID); credentialError != nil {
			credentialError.write(w)
			return
		}
	}
	caller.loginSessionID = loginSessionID
	s.serveAsCaller(w, r, pattern, rule, caller)
}

// administratorOfSessionAgentToken decides whether a session agent token may
// be used off the store proxies. Only an administrator's may: principal-store
// says that person may do anything here, so the agent of a session they
// started may reach the operator routes and every session as they could from
// dash. Everyone else's token stays on the store proxies. It is held to the
// peer the session was started through, as a posting token is, and never
// answers through the public reverse proxy, so a token copied out of a
// transcript reaches nothing from outside this host.
func (s *Server) administratorOfSessionAgentToken(r *http.Request, pattern string, caller requestCaller, sessionID string) (requestCaller, *requestCredentialError) {
	caller, credentialError := s.administratorOnlyIfSessionPeerIsOne(r.Context(), caller, sessionID)
	if credentialError != nil {
		return requestCaller{}, credentialError
	}
	if !caller.isAdministrator {
		return requestCaller{}, &requestCredentialError{http.StatusUnauthorized, "authorization_header_not_accepted", fmt.Sprintf(
			"a session agent token is accepted on %s only for a principal principal-store marks is_administrator, and %s is not one; anyone's is accepted on %s/, %s/, %s/ and %s/",
			pattern, caller.principalID, kanbanProxyMountPrefix, kanbanStoreProxyMountPrefix, grantStoreProxyMountPrefix, mailstackProxyMountPrefix)}
	}
	if requestCameThroughPublicProxy(r) {
		return requestCaller{}, &requestCredentialError{http.StatusForbidden, "session_agent_token_not_public", fmt.Sprintf(
			"an administrator's session agent token reaches %s only from this host, never through the public proxy", pattern)}
	}
	return caller, nil
}

// bearerTokenOf returns the token of an "Authorization: Bearer" header, and
// "" for any other header or none.
func bearerTokenOf(r *http.Request) string {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// publicProxyHeaders are set by the reverse proxy in front of this server on
// every request it forwards. A harness child, a sidecar or a runner reaching
// 127.0.0.1 directly or through an SSH tunnel sends none of them.
var publicProxyHeaders = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded"}

// requestCameThroughPublicProxy reports whether any header the public reverse
// proxy sets is present.
func requestCameThroughPublicProxy(r *http.Request) bool {
	for _, name := range publicProxyHeaders {
		if _, present := r.Header[http.CanonicalHeaderKey(name)]; present {
			return true
		}
	}
	return false
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
	if caller.peer != nil && rule.class == routePrincipalActsThroughPeerConnection {
		writeJSONError(w, http.StatusForbidden, "route_not_for_peers", fmt.Sprintf(
			"%s reaches a server connected to this one, and a connected server may not go on through it to another", pattern))
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
	if caller.peer.actsForRemoteUser() && routeClassIsRefusedToPeerUsers(rule.class) {
		writeJSONError(w, http.StatusForbidden, "route_not_for_peer_users", fmt.Sprintf(
			"%s answers as one principal from a store or record this server cannot narrow to what both the connected server and its mapped user may do, so a connected server's user is refused it (%s)", pattern, rule.reason))
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
	case routePrincipalOwnsSession, routeSessionOwnerOrItsAgentPostsIntoSession:
		sessionID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		if !s.sessionIDIsInScope(sessionID, sessionScopeOfCaller(caller)) {
			// A service that started this session as another principal
			// drives it while it holds can_start_sessions_as on that one.
			drives, err := s.callerStartedSessionIDAndMayDriveIt(r.Context(), caller, sessionID)
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
				return
			}
			if drives {
				break
			}
			if !requestOnlyReads(r, pattern) {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			observes, err := s.mayObserveSession(r, caller, sessionID)
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
				return
			}
			if !observes {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
		}
	case routePrincipalOwnsSignal:
		signalID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		signal, err := s.store.GetSignal(signalID)
		if err != nil {
			http.Error(w, "signal not found", http.StatusNotFound)
			return
		}
		if !s.sessionIDIsInScope(signal.SessionID, sessionScopeOfCaller(caller)) {
			observes := false
			if requestOnlyReads(r, pattern) {
				if observes, err = s.mayObserveSession(r, caller, signal.SessionID); err != nil {
					writeJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
					return
				}
			}
			if !observes {
				http.Error(w, "signal not found", http.StatusNotFound)
				return
			}
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
	case routePrincipalSeesOwnSessionsOnly:
		if requestOnlyReads(r, pattern) {
			observes, err := s.mayObserveEverySession(r, caller)
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
				return
			}
			caller.observesEverySession = observes
		}
	case routePrincipalCreatesSession, routePrincipalReadsCatalog, routePrincipalActsThroughPeerConnection,
		routePrincipalOrSessionAgentThroughStoreProxy, routePrincipalCreatesOperation, routePrincipalSeesOwnOperationsOnly,
		routePrincipalManagesOwnLoginSessions, routePrincipalSeesPeerConnectionsItMayCallThrough:
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
// login cookie or its session agent token, and returns the login session's id
// when the cookie was the credential, or the session agent token's session
// when the token was. A cookie-authenticated request with an unsafe method is
// refused here unless it carries the session's CSRF token. Whether the token
// may be used on this route is the caller's decision: anyone's on the store
// proxies, an administrator's elsewhere.
func (s *Server) principalOfRequest(r *http.Request, rule routeAccessRule) (string, string, string, *requestCredentialError) {
	if _, hasAuthorization := r.Header["Authorization"]; hasAuthorization {
		token := bearerTokenOf(r)
		// A service's own token is accepted on every route, as the principal
		// it was issued for (service_principal_tokens.go).
		if isServicePrincipalToken(token) {
			principalID, credentialError := s.principalOfServicePrincipalToken(r.Context(), r, token)
			return principalID, "", "", credentialError
		}
		if rule.class != routePrincipalOrSessionAgentThroughStoreProxy && !isSessionAgentToken(token) {
			return "", "", "", &requestCredentialError{http.StatusUnauthorized, "authorization_header_not_accepted", fmt.Sprintf(
				"an Authorization header is accepted on %s/, %s/, %s/ and %s/ as a session agent token, and elsewhere only as an administrator's session agent token; this route takes the login cookie or %s",
				kanbanProxyMountPrefix, kanbanStoreProxyMountPrefix, grantStoreProxyMountPrefix, mailstackProxyMountPrefix, serviceTokenHeader)}
		}
		if token == "" {
			return "", "", "", &requestCredentialError{http.StatusUnauthorized, "invalid_session_agent_token",
				"the Authorization header must be \"Bearer <session agent token>\""}
		}
		claims, err := s.sessionAgentTokenClaimsOfLiveSession(token)
		if err != nil {
			return "", "", "", &requestCredentialError{http.StatusUnauthorized, "invalid_session_agent_token", err.Error()}
		}
		return claims.PrincipalID, "", claims.SessionID, nil
	}
	session, refusal := s.liveLoginSessionOfRequest(r)
	if refusal != nil {
		return "", "", "", refusal
	}
	if refusal := csrfRefusalOf(r, session); refusal != nil {
		return "", "", "", refusal
	}
	return session.PrincipalID, session.ID, "", nil
}

// postingAgentOfRequest verifies a session posting token and holds it to the
// session in the path. A token for another session is 404, the same answer a
// principal gets for a session that is not theirs.
func (s *Server) postingAgentOfRequest(r *http.Request, pattern string, rule routeAccessRule) (requestCaller, *requestCredentialError) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return requestCaller{}, &requestCredentialError{http.StatusUnauthorized, "invalid_session_posting_token",
			"the Authorization header must be \"Bearer <session posting token>\""}
	}
	tokenSession, credentialError := s.verifiedPostingSession(token)
	if credentialError != nil {
		return requestCaller{}, credentialError
	}
	tokenSessionID := tokenSession.SessionID
	pathSessionID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
	if err != nil {
		return requestCaller{}, &requestCredentialError{http.StatusInternalServerError, "route_rule_invalid", err.Error()}
	}
	session, err := s.store.GetSession(pathSessionID)
	if err != nil || session.SessionID != tokenSessionID {
		return requestCaller{}, &requestCredentialError{http.StatusNotFound, "session_not_found", "session not found"}
	}
	return requestCaller{postingAgentOfSessionID: tokenSessionID}, nil
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
