// Package routeaccess is who may call each route this server registers: the
// access class and its reason for every pattern, and the description the
// README route table shows. It is the one place a route's class is decided;
// request authorization reads it.
package routeaccess

// RouteAccessClass says who may call a route and how a principal is limited.
type RouteAccessClass int

const (
	// RouteOpenToEveryone needs no credential at all.
	RouteOpenToEveryone RouteAccessClass = iota + 1
	// RouteHarnessCallback is called back by a harness child process, a
	// sidecar or a runner on this host, which today carry no login. It keeps
	// whatever check the handler itself makes, and nothing more.
	RouteHarnessCallback
	// RouteOperatorOnly needs the service token, or a principal
	// principal-store calls an administrator.
	RouteOperatorOnly
	// RoutePeerConnectionCall is called by a connected llm-bridge-server about
	// the connection itself, never with a peer token: the handler checks the
	// connection's credential, or the socket the call arrived over
	// (peers.go).
	RoutePeerConnectionCall
	// RoutePrincipalCreatesSession is POST /sessions: the session is created
	// as the calling principal, whatever the body says.
	RoutePrincipalCreatesSession
	// RoutePrincipalOwnsSession names a session in a path value; a principal
	// reaches it only if the session's principal_id is theirs, and gets 404
	// — the same answer as a missing session — otherwise. An administrator
	// reaches every session, as does the service token.
	RoutePrincipalOwnsSession
	// RoutePrincipalOwnsSignal names a signal in a path value; a principal
	// reaches it only if the signal's session is theirs, 404 otherwise.
	RoutePrincipalOwnsSignal
	// RoutePrincipalSeesOwnSessionsOnly lists or streams across sessions; the
	// handler narrows the answer to the principal's own sessions, reading the
	// principal from the request context.
	RoutePrincipalSeesOwnSessionsOnly
	// RoutePrincipalReadsCatalog is a read-only catalog a chat UI needs; a
	// principal reads it as it is (or as the handler narrows it).
	RoutePrincipalReadsCatalog
	// RoutePrincipalRefusedCannotBeFiltered answers across sessions from a
	// source that cannot be narrowed to one principal cheaply and correctly,
	// so a principal is refused rather than shown other principals' data.
	RoutePrincipalRefusedCannotBeFiltered
	// RoutePrincipalCreatesOperation is POST /operations: the operation is
	// the calling principal's, whatever the body says, and the handler checks
	// the principal belongs to the organization.
	RoutePrincipalCreatesOperation
	// RoutePrincipalOwnsOperation names an operation in a path value; a
	// principal reaches it only if the operation is theirs, 404 otherwise.
	RoutePrincipalOwnsOperation
	// RoutePrincipalSeesOwnOperationsOnly lists operations; the handler
	// narrows the answer to the principal's own.
	RoutePrincipalSeesOwnOperationsOnly
	// RoutePrincipalOrSessionAgentThroughStoreProxy is an identity-carrying
	// store proxy: a login cookie or a session agent token, and the request
	// reaches the store as that principal.
	RoutePrincipalOrSessionAgentThroughStoreProxy
	// RouteSessionOwnerOrItsAgentPostsIntoSession puts something into the
	// session in a path value: a shared file, or an action for a person to
	// confirm. A principal reaches it as on RoutePrincipalOwnsSession. So does
	// the session's own agent, presenting its session posting token
	// (session_posting_token.go) as a Bearer token — for that session and no
	// other. The token is accepted on no other class.
	RouteSessionOwnerOrItsAgentPostsIntoSession
	// RoutePrincipalManagesOwnLoginSessions lists or revokes login sessions;
	// the handler narrows a principal to its own, and lets an administrator
	// reach anyone's.
	RoutePrincipalManagesOwnLoginSessions
	// RoutePrincipalActsThroughPeerConnection is the connection proxy: any
	// principal of this server may call a connected one through it, named
	// there as itself (PeerUserHeader) unless it is an administrator, who
	// acts as this whole server everywhere but in starting a session. A peer
	// may not go on through it to a third server.
	RoutePrincipalActsThroughPeerConnection
	// RoutePrincipalSeesPeerConnectionsItMayCallThrough reads this server's
	// connections to others: an administrator every one, anyone else only
	// those its can_call_through grants name, which the handler narrows to.
	// Holding none shows none, whatever the kind.
	RoutePrincipalSeesPeerConnectionsItMayCallThrough
)

// sessionAgentDelegateAccess says whether, and how, a session's own agent may
// call a route with its session posting token to start and drive delegates:
// sessions it creates under itself (manager_session_id), as bridge-agent does.
// The agent acts with exactly the principal its own session runs with, and
// reaches only its own delegates. Every route without one refuses the token as
// before, except RouteSessionOwnerOrItsAgentPostsIntoSession.
type sessionAgentDelegateAccess int

const (
	// SessionAgentNotAccepted is every route but the few below.
	SessionAgentNotAccepted sessionAgentDelegateAccess = iota
	// SessionAgentCreatesItsDelegate is POST /sessions: the new session sits
	// under the agent's session and runs as that session's principal.
	SessionAgentCreatesItsDelegate
	// SessionAgentReachesItsDelegate names a session in a path value that the
	// agent's session must manage.
	SessionAgentReachesItsDelegate
	// SessionAgentReachesItsDelegateOrItself is the same, and also reaches the
	// agent's own session.
	SessionAgentReachesItsDelegateOrItself
	// SessionAgentReadsCatalog is a catalog a delegate's start depends on.
	SessionAgentReadsCatalog
	// SessionAgentReadsSessions is a read-only session route — the list,
	// full-text search, one session and its history — that a session's agent
	// reads as its own session's principal: every session when that session
	// runs with no principal, as the internal service does, and otherwise
	// exactly what the principal (and, for a session a connected server
	// started, that server too) may read. It never sends, stops or changes
	// anything, and never answers through the public reverse proxy.
	SessionAgentReadsSessions
	// SessionAgentUsesPeerMessaging lists connected servers and their messages
	// and sends one, as an administrator would, for an agent whose session runs
	// with no principal or as an administrator. It never approves or declines:
	// that is a person's decision.
	SessionAgentUsesPeerMessaging
)

// RouteAccessRule is one route's classification.
type RouteAccessRule struct {
	Class RouteAccessClass
	// SessionAgentAccess says whether a session's agent may call the route
	// with its posting token to start and drive its delegates.
	SessionAgentAccess sessionAgentDelegateAccess
	// OwnedResourcePathValueName names the path value holding the session id
	// (RoutePrincipalOwnsSession) or the signal id (RoutePrincipalOwnsSignal).
	OwnedResourcePathValueName string
	// Reason says why the route is in its class, for the next reader and for
	// the refusal message.
	Reason string
	// AnswersThroughPublicProxy lets a RouteHarnessCallback route answer a
	// request that came through the public reverse proxy. Only the runner's
	// routes set it, because their handlers check the runner's token or its
	// one-time passphrase themselves; every other harness callback takes no
	// credential and is for this host's own processes alone.
	AnswersThroughPublicProxy bool
	// SignsIn marks an open route that makes, reads or ends a person's login
	// session. Another server never signs anyone in here, so a request that
	// carries a peer token is refused one, by https or over a peer socket.
	SignsIn bool
}

func OpenRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RouteOpenToEveryone, Reason: reason}
}

// signInRoute is an open route a person's browser uses to sign in, see who it
// is signed in as, or sign out. No connected server may call one: until
// 2026-11-07 a peer token of any level reached POST /auth/demo-login over the
// socket and could make a login session for anyone on a server that dialed
// out with demo login on.
func signInRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RouteOpenToEveryone, Reason: reason, SignsIn: true}
}
func harnessCallbackRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RouteHarnessCallback, Reason: reason}
}

// runnerRoute is a harness callback a remote runner calls, which may arrive
// through the public reverse proxy because its handler checks the runner's
// own credential.
func runnerRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RouteHarnessCallback, Reason: reason, AnswersThroughPublicProxy: true}
}
func PeerConnectionCallRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePeerConnectionCall, Reason: reason}
}
func peerConnectionProxyRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalActsThroughPeerConnection, Reason: reason}
}
func peerConnectionReadRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalSeesPeerConnectionsItMayCallThrough, Reason: reason}
}

// The rules of the agents stores mounted in this process. None of the three
// keeps an owner on a row, and tool-store's provision answers with resolved
// credentials and its invoke runs in-process tools, so all three are the
// operator's, as they were behind dash's login. Runners keep their four
// skill-store seed routes.
var (
	principalStoreOperatorRoute   = operatorRoute("principal-store has no owner on a row: whoever reaches it may create principals, make an administrator, disable anyone and read every email")
	principalStoreVocabularyRoute = catalogRoute("a vocabulary principal-store serves, nothing about anyone")
	identityBindingOperatorRoute  = operatorRoute("which outside identity signs in as which principal")
	grantStoreRoute               = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "grant-store as the caller's principal: it narrows every read and write to what that principal may see and administer, and the internal service is unrestricted"}
	signInStoreBrowserRoute       = signInRoute("a browser signing in or out through sign-in-store; the route checks the browser's own cookie or link secret")
	signInStoreAdministratorRoute = operatorRoute("mints sign-in links, lists or ends anyone's sessions, or messages a member: whoever reaches it could sign in as anyone")
	permissionStoreOperatorRoute  = operatorRoute("permission-store's rules decide which tool calls every session may make, and its audit holds every gated tool input")
	toolStoreRoute                = operatorRoute("tool-store is one registry with no owner on a row; provision answers resolved credentials and invoke runs tools in this process")
	skillStoreRoute               = operatorRoute("skill-store is one registry with no owner on a row, and install writes into this host's skills directory")
	bundleStoreRoute              = operatorRoute("bundle-store is one registry with no owner on a row, and every session given a bundle is composed from it")
	skillStoreRunnerSeedRoute     = runnerRoute("skill-store seed route for runners; the handler authenticates the runner's bearer token")
)

// notesRoute is the rule of every notes route.
var notesRoute = operatorRoute("notes holds the operator's private notes, ranked lists and workspaces with no owner on a row, so any caller could read or rewrite all of them")

// issueItemsRoute is the rule of every /issues/items route: the content of
// every issue, on a board or not. The boards and cards under the rest of
// /issues/ answer a principal only the cards on boards it may view; these
// routes cannot, because an issue on no board names nobody who may see it.
var issueItemsRoute = operatorRoute("/issues/items holds the content of every issue with no owner on a row, on a board or not, so any caller could read or rewrite all of them; a principal reads its cards through /issues/cards")

// projectStoreRoute is the rule of every project-store route.
var projectStoreRoute = operatorRoute("project-store keeps one set of projects with no owner on a row, and its links name sessions, people and notes, so any caller could read or rewrite all of them")

// The access stores' rules (access_routes.go). principal-store and
// permission-store have no owner on a row and no rule of their own about who
// may call them, so every route that changes or reveals more than a
// vocabulary is the operator's. grant-store narrows every answer to the
// caller itself, so it takes the store proxies' class and the verified
// principal in the request context. sign-in-store's browser routes check the
// browser's own cookie or secret, and its administrator routes record the
// administrator who acted.
// predictionStoreRoute is the rule of every prediction-store route.
var predictionStoreRoute = operatorRoute("prediction-store keeps one ledger with no owner on a row, so any caller could rewrite, resolve or purge anyone's prediction")

func operatorRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RouteOperatorOnly, Reason: reason}
}
func sessionAgentPostingRoute(pathValueName, reason string) RouteAccessRule {
	return RouteAccessRule{Class: RouteSessionOwnerOrItsAgentPostsIntoSession, OwnedResourcePathValueName: pathValueName, Reason: reason}
}
func sessionOwnedRoute(pathValueName string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalOwnsSession, OwnedResourcePathValueName: pathValueName, Reason: "acts on one session"}
}

// delegateDrivenSessionRoute is sessionOwnedRoute that a session's agent may
// also call, with its posting token, on a delegate it started.
func delegateDrivenSessionRoute(pathValueName string, access sessionAgentDelegateAccess) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalOwnsSession, OwnedResourcePathValueName: pathValueName, SessionAgentAccess: access,
		Reason: "acts on one session, as its owner or as the agent of the session that started it"}
}
func signalOwnedRoute(pathValueName string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalOwnsSignal, OwnedResourcePathValueName: pathValueName, Reason: "acts on one signal of one session"}
}
func operationOwnedRoute(pathValueName string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalOwnsOperation, OwnedResourcePathValueName: pathValueName, Reason: "acts on one operation"}
}
func ownSessionsOnlyRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalSeesOwnSessionsOnly, Reason: reason}
}

// sessionReadRoute is a read-only session route a session's agent may also
// call with its posting token, reading as its own session's principal.
func sessionReadRoute(rule RouteAccessRule) RouteAccessRule {
	rule.SessionAgentAccess = SessionAgentReadsSessions
	rule.Reason += "; a session's agent reads it as its own session's principal"
	return rule
}

// peerMessagingRoute is an operator route a session's agent may also call
// with its posting token to read connections and messages and send one.
func peerMessagingRoute(rule RouteAccessRule) RouteAccessRule {
	rule.SessionAgentAccess = SessionAgentUsesPeerMessaging
	rule.Reason += "; a session's agent with no principal, or an administrator's, may call it with its posting token"
	return rule
}
func catalogRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalReadsCatalog, Reason: reason}
}
func cannotBeFilteredRoute(reason string) RouteAccessRule {
	return RouteAccessRule{Class: RoutePrincipalRefusedCannotBeFiltered, Reason: reason}
}

// RouteAccessRules classifies every route this server registers, by its exact
// registration pattern. A route missing here is refused to all but the
// service token; TestEveryRegisteredRouteIsClassified fails when a
// HandleFunc/Handle in this package has no rule.
var RouteAccessRules = map[string]RouteAccessRule{
	// Open.
	"GET /health":                                         OpenRoute("liveness for load balancers and healthcheck"),
	"GET /ui/":                                            OpenRoute("the front end's own files, the same code as the repo; the app signs in through the login routes after it loads"),
	"GET /{$}":                                            OpenRoute("sends a browser at the bare address to the front end"),
	"POST /auth/demo-login":                               signInRoute("how a principal obtains a login; answers 404 itself when switched off"),
	"GET /auth/entra/start":                               signInRoute("sends the browser to Entra ID to sign in"),
	"GET /auth/entra/callback":                            signInRoute("Entra ID sends the browser back here; the handler verifies everything it is given"),
	"GET /auth/principal":                                 signInRoute("answers 401 itself without a live login session"),
	"POST /auth/logout":                                   signInRoute("revokes the cookie's live session, checking its CSRF token itself, and clears a dead cookie with none"),
	"POST /peer-connection-invites":                       operatorRoute("lets another server connect to this one"),
	"GET /peer-connection-invites":                        operatorRoute("who was invited to connect, and by whom"),
	"POST /peer-connections":                              operatorRoute("connects to another server and stores a credential for it"),
	"GET /peer-connections":                               peerMessagingRoute(peerConnectionReadRoute("which servers this one is connected to, and the access each way; anyone but an administrator sees only those its can_call_through grants name")),
	"POST /peer-connections/{id}/messages":                peerMessagingRoute(operatorRoute("sends the other server a request, notice or status")),
	"GET /peer-messages":                                  peerMessagingRoute(operatorRoute("messages to and from connected servers")),
	"GET /peer-messages/{id}":                             peerMessagingRoute(operatorRoute("one message to or from a connected server")),
	"POST /peer-messages/{id}/approve":                    operatorRoute("approves a connected server's request or patch (handing it to the orchestrator, or starting its session on a server with none) or notice (running the update); never an agent"),
	"POST /peer-messages/{id}/decline":                    operatorRoute("declines a connected server's request or notice and tells it why"),
	"POST /peer-messages/{id}/place":                      peerMessagingRoute(operatorRoute("starts an approved request's or patch's session on the instance the orchestrator chose; the handler lets an agent place only a message handed to its own session")),
	"POST /peer/messages":                                 PeerConnectionCallRoute("the other server delivers a message; the handler checks the connection and what it may send"),
	"POST /peer-message-attachments":                      peerMessagingRoute(operatorRoute("stores a file to send to a connected server with the next message")),
	"GET /peer-messages/{id}/attachments/{attachment_id}": peerMessagingRoute(operatorRoute("downloads a file of a message; an agent only of a message it sent or whose approval started its session")),
	"POST /peer-messages/{id}/attachments/{attachment_id}/retry":  operatorRoute("fetches a file of an incoming message again now"),
	"GET /peer/messages/{message_id}/attachments/{attachment_id}": PeerConnectionCallRoute("the other server fetches a file of a message this server sent it; the handler checks the connection"),
	"GET /peer-connections/{id}":                                  peerConnectionReadRoute("one connection and the access each way; anyone but an administrator reaches only one its can_call_through grants name"),
	"POST /peer-connections/{id}/remove":                          operatorRoute("ends a connection and the access each way"),
	"POST /peer-connections/{id}/grant":                           operatorRoute("gives another server access to this one"),
	"POST /peer-connections/{id}/revoke-grant":                    operatorRoute("takes another server's access away"),
	"POST /peer-connections/{id}/check":                           operatorRoute("calls another server with a stored token"),
	"/peer-connections/{id}/proxy/{rest...}":                      peerConnectionProxyRoute("acts on another server: as this server's peer principal for an administrator, and for anyone else as that person, whom the other server maps to one of its own principals"),
	"GET /peer-access-levels":                                     operatorRoute("the levels of access this server can give"),
	"POST /peer-access-levels/{key}/provision":                    operatorRoute("makes a level's group and its grants"),
	"POST /peer/connect":                                          OpenRoute("the other server has no credential yet; the one-time passphrase is the credential, checked by the handler"),
	"GET /peer/socket":                                            PeerConnectionCallRoute("the dial-out socket; the handler checks the connection's credential"),
	"POST /peer/access":                                           PeerConnectionCallRoute("the other server delivers a token; the handler checks the connection"),
	"POST /peer/access/revoked":                                   PeerConnectionCallRoute("the other server takes a token back; the handler checks the connection"),
	"POST /peer/connection/removed":                               PeerConnectionCallRoute("the other server says it removed the connection; the handler checks the connection"),
	"GET /peer-user-mappings":                                     operatorRoute("which connected server's users act as which principal here"),
	"POST /peer-user-mappings":                                    operatorRoute("lets a connected server's user act here as one of this server's principals"),
	"POST /peer-user-mappings/{id}/remove":                        operatorRoute("takes that away"),
	"GET /peer/self":                                              catalogRoute("tells a caller who it is; the handler answers only a peer token"),
	"GET /peer/server-name":                                       PeerConnectionCallRoute("a connected server asks this one's name and gives its own; the handler checks the connection"),
	"GET /server":                                                 catalogRoute("what this server calls itself, which the chat draws as the heading of its sessions"),
	"GET /auth/sessions":                                          {Class: RoutePrincipalManagesOwnLoginSessions, Reason: "a principal's own login sessions; an administrator names anyone's"},
	"POST /auth/sessions/{id}/revoke":                             {Class: RoutePrincipalManagesOwnLoginSessions, Reason: "ends a principal's own login session; an administrator ends anyone's"},
	"GET /auth/login-attempts":                                    operatorRoute("every sign-in attempt, with the identities of people refused"),

	// Harness callbacks: called by processes this server spawned or by
	// runners, none of which carry a login.
	"POST /permission/cc-prehook/{bridge_id}":    harnessCallbackRoute("Claude Code PreToolUse hook, a curl from the harness child"),
	"POST /permission/codex-prehook/{bridge_id}": harnessCallbackRoute("codex PreToolUse hook, a curl from the harness child"),
	"POST /permission/devin-prehook/{bridge_id}": harnessCallbackRoute("devin PreToolUse hook, a curl from the harness child, and llm-bridge-devin itself for session/request_permission"),
	"POST /sidecar/event/{bridge_id}":            harnessCallbackRoute("OTel sidecar of a pty session posts translated events"),
	"POST /hooks/exec/{id}":                      harnessCallbackRoute("registered hook fired by the harness child via curl"),
	"GET /api/runner/ws":                         runnerRoute("runner WebSocket; the handler authenticates the runner's bearer token"),
	"POST /api/runner/enroll":                    runnerRoute("runner enrollment; the handler checks the single-use passphrase"),
	"GET /api/runner/install.sh":                 runnerRoute("bootstrap script fetched by a machine before it has a runner token"),
	"GET /api/runner/binary":                     runnerRoute("runner and harness binaries fetched by a machine during bootstrap"),
	// log-store, mounted in this process: every event of every session, with no
	// owner on a row it could narrow by, and an ingest route that writes into
	// any session. The bridge's own session routes answer a principal from it.
	"/session-log/":                          operatorRoute("log-store holds every session's events and takes events into any session, and has no owner on a row to narrow an answer by"),
	"/api/harness-proxy/{harness}/{rest...}": harnessCallbackRoute("harness backend proxy for wrappers on runners; unauthenticated today (noteboard card f4e5e1ef)"),

	// Sessions.
	"POST /sessions":              {Class: RoutePrincipalCreatesSession, SessionAgentAccess: SessionAgentCreatesItsDelegate, Reason: "a principal creates sessions as itself, and a session's agent its delegates as its own session's principal"},
	"GET /sessions":               sessionReadRoute(ownSessionsOnlyRoute("session list, narrowed in the store")),
	"GET /sessions/summary":       ownSessionsOnlyRoute("sidebar list, narrowed in the store"),
	"GET /sessions/principals":    ownSessionsOnlyRoute("who the sessions were started as, narrowed in the store"),
	"POST /sessions/summary":      ownSessionsOnlyRoute("sidebar list, narrowed in the store"),
	"GET /sessions/recent-bundle": ownSessionsOnlyRoute("recent sessions warmed for the chat page, narrowed in the store"),
	"GET /sessions/validators":    ownSessionsOnlyRoute("staleness check; ids not owned by the caller are dropped before log-store is asked"),
	"POST /sessions/validators":   ownSessionsOnlyRoute("staleness check; ids not owned by the caller are dropped before log-store is asked"),
	"GET /session-events":         ownSessionsOnlyRoute("session list stream; frames about other principals' sessions are not written"),
	"GET /signals":                ownSessionsOnlyRoute("cross-session signal inbox, narrowed in the store"),
	"GET /session-briefs":         ownSessionsOnlyRoute("cross-session brief list, narrowed in the store"),
	"GET /sessions/search":        sessionReadRoute(cannotBeFilteredRoute("full-text search answered by log-store across every session")),
	"GET /sessions/aggregates":    cannotBeFilteredRoute("aggregates computed by log-store across every session"),
	"GET /sessions/discover":      operatorRoute("imports sessions found on this host's disk"),

	"GET /sessions/{id}":                                    sessionReadRoute(sessionOwnedRoute("id")),
	"POST /sessions/{id}/send":                              delegateDrivenSessionRoute("id", SessionAgentReachesItsDelegate),
	"POST /sessions/{id}/files":                             sessionAgentPostingRoute("id", "shares a file into one session, as its owner or as its own agent"),
	"GET /sessions/{id}/files":                              sessionOwnedRoute("id"),
	"GET /sessions/{id}/files/{file_id}/content":            sessionOwnedRoute("id"),
	"GET /sessions/{id}/files-named-by-tools":               sessionOwnedRoute("id"),
	"GET /sessions/{id}/files-named-by-tools/content":       sessionOwnedRoute("id"),
	"GET /sessions/{id}/files-named-by-tools/image":         sessionOwnedRoute("id"),
	"POST /sessions/{id}/chat-links":                        sessionAgentPostingRoute("id", "links one session to an outside conversation, as its owner or as its own agent"),
	"GET /sessions/{id}/chat-links":                         sessionAgentPostingRoute("id", "reads one session's chat links, as its owner or as its own agent"),
	"GET /sessions/{id}/chat-links/{link_id}":               sessionAgentPostingRoute("id", "reads one of a session's chat links and its log, as its owner or as its own agent"),
	"POST /sessions/{id}/chat-links/{link_id}/end":          sessionAgentPostingRoute("id", "ends one of a session's chat links, as its owner or as its own agent"),
	"POST /sessions/{id}/actions":                           sessionAgentPostingRoute("id", "offers an action in one session, as its owner or as its own agent"),
	"POST /sessions/{id}/images":                            sessionAgentPostingRoute("id", "makes images and shares them into one session, as its owner or as its own agent"),
	"GET /sessions/{id}/actions":                            sessionOwnedRoute("id"),
	"POST /sessions/{id}/actions/{action_id}/run":           sessionOwnedRoute("id"),
	"GET /sessions/{id}/events":                             delegateDrivenSessionRoute("id", SessionAgentReachesItsDelegate),
	"GET /sessions/{id}/attach":                             sessionOwnedRoute("id"),
	"GET /sessions/{id}/attach-token":                       sessionOwnedRoute("id"),
	"GET /sessions/{id}/messages":                           sessionReadRoute(sessionOwnedRoute("id")),
	"GET /sessions/{id}/messages/raw":                       sessionOwnedRoute("id"),
	"GET /sessions/{id}/entries/{eventId}":                  sessionOwnedRoute("id"),
	"GET /sessions/{id}/entries/{eventId}/images/{index}":   sessionOwnedRoute("id"),
	"POST /sessions/{id}/interrupt":                         sessionOwnedRoute("id"),
	"POST /sessions/{id}/resume":                            sessionOwnedRoute("id"),
	"POST /sessions/{id}/stop":                              delegateDrivenSessionRoute("id", SessionAgentReachesItsDelegate),
	"POST /sessions/{id}/mode":                              sessionOwnedRoute("id"),
	"POST /sessions/{id}/compact":                           sessionOwnedRoute("id"),
	"POST /sessions/{id}/fork":                              sessionOwnedRoute("id"),
	"POST /sessions/{id}/rename":                            sessionOwnedRoute("id"),
	"POST /sessions/{id}/config":                            sessionOwnedRoute("id"),
	"POST /sessions/{id}/mark-done":                         sessionOwnedRoute("id"),
	"GET /sessions/{id}/git/repos":                          sessionOwnedRoute("id"),
	"GET /sessions/{id}/git":                                sessionOwnedRoute("id"),
	"GET /sessions/{id}/effective-config":                   sessionOwnedRoute("id"),
	"GET /sessions/{id}/hooks/pending":                      sessionOwnedRoute("id"),
	"POST /sessions/{id}/hooks/{request_id}/resolve":        sessionOwnedRoute("id"),
	"GET /sessions/{id}/signals":                            sessionOwnedRoute("id"),
	"GET /sessions/{id}/brief":                              sessionReadRoute(sessionOwnedRoute("id")),
	"POST /sessions/{id}/loose-ends/{loose_end_id}/answer":  sessionOwnedRoute("id"),
	"POST /sessions/{id}/loose-ends/{loose_end_id}/dismiss": sessionOwnedRoute("id"),
	"POST /sessions/{id}/signals":                           sessionOwnedRoute("id"),
	"GET /sessions/{id}/tools/{tool_use_id}/snapshots":      sessionOwnedRoute("id"),
	"PUT /sessions/{id}/folder":                             operatorRoute("files a session into the shared folder registry"),
	"PUT /sessions/{id}/starter":                            operatorRoute("records which service started a session as another principal, letting it drive the session"),
	"POST /service-principal-tokens":                        operatorRoute("issues a token a service principal calls this server with"),
	"GET /service-principal-tokens":                         operatorRoute("every service principal token's record; never a token"),
	"GET /self":                                             catalogRoute("tells any caller who it acts as"),
	"POST /service-principal-tokens/{id}/revoke":            operatorRoute("stops a service principal token working"),
	"PUT /sessions/{id}/permission-mode":                    operatorRoute("can switch a session to bypass, skipping permission-store"),
	"PUT /sessions/{id}/bypass-permissions":                 operatorRoute("can switch a session to bypass, skipping permission-store"),
	"POST /signals/{id}/resolve":                            signalOwnedRoute("id"),
	"POST /signals/{id}/answer":                             signalOwnedRoute("id"),
	"GET /snapshots/blob/{sha}":                             cannotBeFilteredRoute("content-addressed blob shared across sessions; nothing ties a sha to one session"),

	// Operations.
	"POST /operations":                            {Class: RoutePrincipalCreatesOperation, Reason: "a principal starts operations as itself, in an organization it belongs to"},
	"GET /operations":                             {Class: RoutePrincipalSeesOwnOperationsOnly, Reason: "operation list, narrowed in the store"},
	"GET /operations/{id}":                        operationOwnedRoute("id"),
	"GET /operations/{id}/events":                 operationOwnedRoute("id"),
	"POST /operations/{id}/cancel":                operationOwnedRoute("id"),
	"GET /operations/{id}/children":               operationOwnedRoute("id"),
	"GET /operation-types":                        OpenRoute("names the operation types this server runs and their limits, nothing about anyone; grant-store reads it without a credential to check an operation_type grant"),
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
	"GET /instances":                     {Class: RoutePrincipalReadsCatalog, SessionAgentAccess: SessionAgentReadsCatalog, Reason: "instances to start a session on; narrowed to can_dispatch_on grants and without machine details"},
	"GET /agents":                        catalogRoute("agent-store agents, unfiltered by can_run_as"),
	"GET /agents/{slug}":                 catalogRoute("one agent-store agent, unfiltered by can_run_as"),

	// Identity-carrying store proxies.
	IssuesMountPrefix + "/": {Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "kanban-store at its root, in this process, as the caller's principal or the internal service"},
	MailMountPrefix + "/":   {Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "mailstack, in this process, as the caller's principal or the internal service"},
	// A sender of mail holds an ingress credential mailstack minted, bound to
	// its accounts, and no login; mailstack checks it and refuses anything
	// else.
	MailstackIngressDeliveryPattern: OpenRoute("pushed mail delivery, which mailstack checks against an ingress credential"),

	// Operator.
	"GET /services":                                  operatorRoute("host service inventory"),
	"GET /services/databases/schema":                 operatorRoute("host database schemas"),
	"GET /services/databases/rows":                   operatorRoute("host database rows"),
	"POST /bridge/permission-mode":                   operatorRoute("global permission mode"),
	"POST /bridge/bypass-permissions":                operatorRoute("global permission mode"),
	"GET /folders":                                   operatorRoute("folder registry"),
	"POST /folders":                                  operatorRoute("folder registry"),
	"DELETE /folders/{name}":                         operatorRoute("folder registry"),
	"PUT /folders/{name}":                            operatorRoute("folder registry"),
	"GET /source-folders":                            operatorRoute("purpose to folder mapping"),
	"PUT /source-folders/{source}":                   operatorRoute("purpose to folder mapping"),
	"DELETE /source-folders/{source}":                operatorRoute("purpose to folder mapping"),
	"GET /session-taxonomy/report":                   operatorRoute("lists sessions across every principal"),
	"POST /instances":                                operatorRoute("instance registry"),
	"GET /instances/{id}":                            operatorRoute("instance with machine details"),
	"PUT /instances/{id}":                            operatorRoute("instance registry"),
	"DELETE /instances/{id}":                         operatorRoute("instance registry"),
	"GET /instances/{id}/status":                     operatorRoute("machine reachability"),
	"GET /instances/{id}/sessions":                   operatorRoute("sessions of every principal on an instance"),
	"GET /instances/{id}/credentials":                operatorRoute("credential bindings"),
	"POST /instances/{id}/credentials":               operatorRoute("credential bindings"),
	"DELETE /instances/{id}/credentials/{cred_id}":   operatorRoute("credential bindings"),
	"POST /instances/{id}/oneshot":                   operatorRoute("runs a model call outside any session"),
	"POST /oneshot":                                  operatorRoute("runs a model call outside any session"),
	"POST /images":                                   operatorRoute("makes images outside any session, on the operator's subscription"),
	"POST /hooks":                                    operatorRoute("hook registry"),
	"GET /hooks":                                     operatorRoute("hook registry"),
	"GET /hook-options":                              operatorRoute("hook registry vocabulary"),
	"POST /restart-drain":                            operatorRoute("hold every new turn until a restart, so the work running can finish"),
	"DELETE /restart-drain":                          operatorRoute("stop holding new turns and send the messages held"),
	"GET /restart-readiness":                         operatorRoute("which sessions a restart of this server would cut off; deploy.sh waits on it"),
	"GET /settings":                                  operatorRoute("this server's own configuration, with where each value came from"),
	"PUT /settings/{key}":                            operatorRoute("changes how this server behaves"),
	"GET /hooks/{id}":                                operatorRoute("hook registry"),
	"PATCH /hooks/{id}":                              operatorRoute("hook registry"),
	"DELETE /hooks/{id}":                             operatorRoute("hook registry"),
	"GET /credentials":                               operatorRoute("auth-store credentials"),
	"POST /credentials":                              operatorRoute("auth-store credentials"),
	"DELETE /credentials/{id}":                       operatorRoute("auth-store credentials"),
	"GET /models":                                    operatorRoute("models joined to auth-store credential labels and masked keys"),
	"GET /conformance":                               operatorRoute("harness conformance matrix"),
	"POST /conformance/run":                          operatorRoute("runs harness conformance"),
	"GET /bridge-prefs":                              operatorRoute("server preferences"),
	"GET /effective-config":                          operatorRoute("what a session would be given, before it exists"),
	"PUT /bridge-prefs":                              operatorRoute("server preferences"),
	"POST /admin/file-inactive":                      operatorRoute("housekeeping across every session"),
	"POST /admin/archive-old":                        operatorRoute("housekeeping across every session"),
	"GET /machines":                                  operatorRoute("machine registry"),
	"POST /machines":                                 operatorRoute("machine registry"),
	"GET /machines/{id}":                             operatorRoute("machine registry"),
	"PUT /machines/{id}":                             operatorRoute("machine registry"),
	"PUT /machines/{id}/computer-use":                operatorRoute("machine registry"),
	"DELETE /machines/{id}":                          operatorRoute("machine registry"),
	"GET /machines/stats":                            operatorRoute("machine health readings"),
	"GET /machines/{id}/stats/history":               operatorRoute("machine health readings"),
	"GET /machines/{id}/disk-usage":                  operatorRoute("what fills a machine's drives"),
	"POST /machines/{id}/disk-usage/scans":           operatorRoute("scans a machine's drive"),
	"GET /machines/{id}/disk-usage/entries":          operatorRoute("what fills a machine's drives"),
	"POST /machines/{id}/disk-usage/deletion-checks": operatorRoute("what fills a machine's drives"),
	"POST /machines/{id}/disk-usage/deletions":       operatorRoute("deletes files on a machine for good"),
	"GET /machines/{id}/disk-usage/deletions":        operatorRoute("the record of deletions on a machine"),
	"POST /api/runner/seed/broadcast":                operatorRoute("tells every runner to reconcile"),

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

	// tool-store, skill-store and bundle-store, mounted in this process.
	"GET /tools/health":                                 toolStoreRoute,
	"GET /tools/kinds":                                  toolStoreRoute,
	"GET /tools/tools":                                  toolStoreRoute,
	"POST /tools/tools":                                 toolStoreRoute,
	"GET /tools/tools/{id}":                             toolStoreRoute,
	"PATCH /tools/tools/{id}":                           toolStoreRoute,
	"DELETE /tools/tools/{id}":                          toolStoreRoute,
	"POST /tools/tools/{id}/enable":                     toolStoreRoute,
	"POST /tools/tools/{id}/disable":                    toolStoreRoute,
	"GET /tools/tools/by-name/{name}":                   toolStoreRoute,
	"GET /tools/tools/by-name/{name}/spec":              toolStoreRoute,
	"POST /tools/tools/by-name/{name}/invoke":           toolStoreRoute,
	"GET /tools/tools/by-name/{name}/instances":         toolStoreRoute,
	"POST /tools/harness-tools/observed":                toolStoreRoute,
	"GET /tools/locals":                                 toolStoreRoute,
	"POST /tools/provision":                             toolStoreRoute,
	"GET /tools/instances/{id}/tools":                   toolStoreRoute,
	"POST /tools/instances/{id}/tools/by-name/{name}":   toolStoreRoute,
	"DELETE /tools/instances/{id}/tools/by-name/{name}": toolStoreRoute,
	"GET /skills/health":                                skillStoreRoute,
	"GET /skills/sources":                               skillStoreRoute,
	"POST /skills/sources":                              skillStoreRoute,
	"GET /skills/sources/{id}":                          skillStoreRoute,
	"DELETE /skills/sources/{id}":                       skillStoreRoute,
	"POST /skills/sources/{id}/sync":                    skillStoreRoute,
	"GET /skills/skills":                                skillStoreRoute,
	"GET /skills/skills/{id}":                           skillStoreRoute,
	"DELETE /skills/skills/{id}":                        skillStoreRoute,
	"GET /skills/skills/{id}/files":                     skillStoreRoute,
	"GET /skills/skills/{id}/files/{path...}":           skillStoreRoute,
	"POST /skills/skills/{id}/install":                  skillStoreRoute,
	"POST /skills/skills/{id}/uninstall":                skillStoreRoute,
	"GET /skills/installations":                         skillStoreRoute,
	"GET /skills/seed/profile":                          skillStoreRoute,
	"PUT /skills/seed/profile":                          skillStoreRoute,
	"GET /skills/seed/manifest":                         skillStoreRoute,
	"GET /skills/versions/{vid}/content":                skillStoreRoute,
	"POST /skills/seed/observe":                         skillStoreRoute,
	"POST /skills/seed/drift":                           skillStoreRoute,
	"GET /api/skill-store/seed/manifest":                skillStoreRunnerSeedRoute,
	"GET /api/skill-store/versions/{vid}/content":       skillStoreRunnerSeedRoute,
	"POST /api/skill-store/seed/observe":                skillStoreRunnerSeedRoute,
	"POST /api/skill-store/seed/drift":                  skillStoreRunnerSeedRoute,
	"GET /bundles/health":                               bundleStoreRoute,
	"GET /bundles/member-effects":                       bundleStoreRoute,
	"GET /bundles/bundles":                              bundleStoreRoute,
	"POST /bundles/bundles":                             bundleStoreRoute,
	"GET /bundles/bundles/{id}":                         bundleStoreRoute,
	"DELETE /bundles/bundles/{id}":                      bundleStoreRoute,
	"POST /bundles/bundles/{id}/enable":                 bundleStoreRoute,
	"POST /bundles/bundles/{id}/disable":                bundleStoreRoute,
	"GET /bundles/bundles/by-name/{name}":               bundleStoreRoute,
	"POST /bundles/resolve":                             bundleStoreRoute,

	// memory-store's library routes.
	// prediction-store, mounted in this process. Its ledger has no owner on a
	// row, so any caller could rewrite, resolve or purge anyone's prediction;
	// until rows carry an owning principal it is the operator's, as
	// memory-store is. An administrator's session agent token reaches it.
	"GET /predictions/health":                      predictionStoreRoute,
	"GET /predictions/vocabulary":                  predictionStoreRoute,
	"GET /predictions/categories":                  predictionStoreRoute,
	"GET /predictions/tags":                        predictionStoreRoute,
	"GET /predictions/calibration":                 predictionStoreRoute,
	"GET /predictions/base-rates":                  predictionStoreRoute,
	"GET /predictions/predictions":                 predictionStoreRoute,
	"POST /predictions/predictions":                predictionStoreRoute,
	"GET /predictions/predictions/{id}":            predictionStoreRoute,
	"PATCH /predictions/predictions/{id}":          predictionStoreRoute,
	"DELETE /predictions/predictions/{id}":         predictionStoreRoute,
	"POST /predictions/predictions/{id}/restore":   predictionStoreRoute,
	"GET /predictions/predictions/{id}/estimates":  predictionStoreRoute,
	"POST /predictions/predictions/{id}/estimates": predictionStoreRoute,
	"POST /predictions/predictions/{id}/resolve":   predictionStoreRoute,
	"GET /predictions/predictions/{id}/links":      predictionStoreRoute,
	"POST /predictions/predictions/{id}/links":     predictionStoreRoute,
	"DELETE /predictions/links/{id}":               predictionStoreRoute,

	// notes, mounted in this process. Its notes are the operator's own,
	// private, with no owner on a row, so it is the operator's, as
	// prediction-store is. An administrator's session agent token reaches it.
	"GET /notes/health":                 notesRoute,
	"GET /notes/items":                  notesRoute,
	"POST /notes/items":                 notesRoute,
	"POST /notes/items/rerank":          notesRoute,
	"POST /notes/items/query":           notesRoute,
	"POST /notes/items/batch":           notesRoute,
	"GET /notes/items/query-options":    notesRoute,
	"GET /notes/items/{id}":             notesRoute,
	"PATCH /notes/items/{id}":           notesRoute,
	"DELETE /notes/items/{id}":          notesRoute,
	"GET /notes/items/{id}/revisions":   notesRoute,
	"POST /notes/items/{id}/restore":    notesRoute,
	"POST /notes/items/{id}/hold":       notesRoute,
	"POST /notes/items/{id}/unhold":     notesRoute,
	"GET /notes/items/{id}/occurrences": notesRoute,
	"GET /notes/lists":                  notesRoute,
	"GET /notes/tags":                   notesRoute,
	"GET /notes/search":                 notesRoute,

	// The content of issues, mounted in this process beside the boards and
	// cards of /issues/.
	"GET /issues/items":                  issueItemsRoute,
	"POST /issues/items":                 issueItemsRoute,
	"POST /issues/items/rerank":          issueItemsRoute,
	"POST /issues/items/query":           issueItemsRoute,
	"POST /issues/items/batch":           issueItemsRoute,
	"GET /issues/items/query-options":    issueItemsRoute,
	"GET /issues/items/health":           issueItemsRoute,
	"GET /issues/items/tags":             issueItemsRoute,
	"GET /issues/items/search":           issueItemsRoute,
	"GET /issues/items/{id}":             issueItemsRoute,
	"PATCH /issues/items/{id}":           issueItemsRoute,
	"DELETE /issues/items/{id}":          issueItemsRoute,
	"GET /issues/items/{id}/revisions":   issueItemsRoute,
	"POST /issues/items/{id}/restore":    issueItemsRoute,
	"POST /issues/items/{id}/hold":       issueItemsRoute,
	"POST /issues/items/{id}/unhold":     issueItemsRoute,
	"GET /issues/items/{id}/occurrences": issueItemsRoute,

	// permission-store, mounted in this process.
	"GET /permissions/healthz":       permissionStoreOperatorRoute,
	"GET /permissions/rules":         permissionStoreOperatorRoute,
	"POST /permissions/rules":        permissionStoreOperatorRoute,
	"GET /permissions/rules/{id}":    permissionStoreOperatorRoute,
	"PATCH /permissions/rules/{id}":  permissionStoreOperatorRoute,
	"DELETE /permissions/rules/{id}": permissionStoreOperatorRoute,
	"POST /permissions/evaluate":     permissionStoreOperatorRoute,
	"GET /permissions/audit":         permissionStoreOperatorRoute,
	// sign-in-store, mounted in this process.
	"GET /sign-in/health":                            OpenRoute("says sign-in-store answers, nothing about anyone"),
	"POST /sign-in/link-requests":                    signInStoreBrowserRoute,
	"POST /sign-in/redeem":                           signInStoreBrowserRoute,
	"GET /sign-in/session":                           signInStoreBrowserRoute,
	"POST /sign-in/sign-out":                         signInStoreBrowserRoute,
	"GET /sign-in/discord/start":                     signInStoreBrowserRoute,
	"GET /sign-in/discord/callback":                  signInStoreBrowserRoute,
	"POST /sign-in/sign-in-links":                    signInStoreAdministratorRoute,
	"GET /sign-in/sign-in-links":                     signInStoreAdministratorRoute,
	"GET /sign-in/sessions":                          signInStoreAdministratorRoute,
	"POST /sign-in/sessions/{id}/revoke":             signInStoreAdministratorRoute,
	"POST /sign-in/principals/{id}/discord-messages": signInStoreAdministratorRoute,
	// grant-store, mounted in this process.
	"GET /grants/health":                    grantStoreRoute,
	"GET /grants/relations":                 grantStoreRoute,
	"GET /grants/resource-types":            grantStoreRoute,
	"GET /grants/grants":                    grantStoreRoute,
	"POST /grants/grants":                   grantStoreRoute,
	"GET /grants/grants/{id}":               grantStoreRoute,
	"POST /grants/grants/{id}/revoke":       grantStoreRoute,
	"GET /grants/principals/{id}/effective": grantStoreRoute,
	// project-store, mounted in this process, for the same reason.
	"GET /projects/health":                                       projectStoreRoute,
	"GET /projects/vocabulary":                                   projectStoreRoute,
	"GET /projects/projects":                                     projectStoreRoute,
	"POST /projects/projects":                                    projectStoreRoute,
	"GET /projects/digest":                                       projectStoreRoute,
	"GET /projects/projects/{id}":                                projectStoreRoute,
	"PATCH /projects/projects/{id}":                              projectStoreRoute,
	"GET /projects/projects/{id}/rollup":                         projectStoreRoute,
	"GET /projects/projects/{id}/changes":                        projectStoreRoute,
	"GET /projects/projects/{id}/links":                          projectStoreRoute,
	"POST /projects/projects/{id}/links":                         projectStoreRoute,
	"DELETE /projects/projects/{id}/links/{link_id}":             projectStoreRoute,
	"GET /projects/links":                                        projectStoreRoute,
	"GET /projects/projects/{id}/activity":                       projectStoreRoute,
	"POST /projects/projects/{id}/next-steps":                    projectStoreRoute,
	"GET /projects/projects/{id}/suggestions":                    projectStoreRoute,
	"POST /projects/projects/{id}/suggestions":                   projectStoreRoute,
	"POST /projects/suggestions/{suggestion_id}/accept":          projectStoreRoute,
	"POST /projects/suggestions/{suggestion_id}/dismiss":         projectStoreRoute,
	"GET /projects/projects/{id}/goals":                          projectStoreRoute,
	"POST /projects/projects/{id}/goals":                         projectStoreRoute,
	"GET /projects/goals":                                        projectStoreRoute,
	"GET /projects/goals/{goal_id}":                              projectStoreRoute,
	"PATCH /projects/goals/{goal_id}":                            projectStoreRoute,
	"GET /projects/goals/{goal_id}/changes":                      projectStoreRoute,
	"GET /projects/goals/{goal_id}/links":                        projectStoreRoute,
	"POST /projects/goals/{goal_id}/links":                       projectStoreRoute,
	"DELETE /projects/goals/{goal_id}/links/{link_id}":           projectStoreRoute,
	"GET /projects/goal-links":                                   projectStoreRoute,
	"GET /projects/session-filings":                              projectStoreRoute,
	"POST /projects/session-filings/run":                         projectStoreRoute,
	"POST /projects/session-filings/{session_filing_id}/accept":  projectStoreRoute,
	"POST /projects/session-filings/{session_filing_id}/dismiss": projectStoreRoute,

	// principal-store, mounted in this process.
	"GET /principals/health":                                  principalStoreOperatorRoute,
	"GET /principals/kinds":                                   principalStoreVocabularyRoute,
	"GET /principals/kind-definitions":                        principalStoreVocabularyRoute,
	"GET /principals/availability-reasons":                    principalStoreVocabularyRoute,
	"GET /principals/weekday-codes":                           principalStoreVocabularyRoute,
	"GET /principals/identity-providers":                      principalStoreVocabularyRoute,
	"GET /principals/principals":                              principalStoreOperatorRoute,
	"POST /principals/principals":                             principalStoreOperatorRoute,
	"POST /principals/contacts/resolve":                       principalStoreOperatorRoute,
	"GET /principals/principals/{id}":                         principalStoreOperatorRoute,
	"PATCH /principals/principals/{id}":                       principalStoreOperatorRoute,
	"POST /principals/principals/{id}/disable":                principalStoreOperatorRoute,
	"POST /principals/principals/{id}/enable":                 principalStoreOperatorRoute,
	"GET /principals/principals/{id}/members":                 principalStoreOperatorRoute,
	"PUT /principals/principals/{group}/members/{member}":     principalStoreOperatorRoute,
	"DELETE /principals/principals/{group}/members/{member}":  principalStoreOperatorRoute,
	"GET /principals/principals/{id}/groups":                  principalStoreOperatorRoute,
	"GET /principals/principals/{id}/availability":            principalStoreOperatorRoute,
	"GET /principals/principals/{id}/time-off":                principalStoreOperatorRoute,
	"POST /principals/principals/{id}/time-off":               principalStoreOperatorRoute,
	"DELETE /principals/principals/{id}/time-off/{timeOffID}": principalStoreOperatorRoute,
	"GET /principals/identity-bindings":                       identityBindingOperatorRoute,
	"GET /principals/identity-bindings/resolve":               identityBindingOperatorRoute,
	"GET /principals/identity-bindings/{id}":                  identityBindingOperatorRoute,
	"POST /principals/identity-bindings":                      operatorRoute("decides who an outside identity signs in as"),
	"POST /principals/identity-bindings/{id}/unlink":          operatorRoute("stops an outside identity signing in, and ends its login sessions"),
	"POST /memories":                                          operatorRoute("memory-store is not partitioned by principal"),
	"GET /memories/{id}":                                      operatorRoute("memory-store is not partitioned by principal"),
	"DELETE /memories/{id}":                                   operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/search":                                   operatorRoute("memory-store is not partitioned by principal"),
	"GET /memories/recent":                                    operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/decay":                                    operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/compact":                                  operatorRoute("memory-store is not partitioned by principal"),
	"POST /memories/context":                                  operatorRoute("memory-store is not partitioned by principal"),
}

// IssuesMountPrefix is where kanban-store is mounted, with the remainder
// answered at its root, unchanged: /issues/boards reaches /boards.
// kanban-store runs in this process (kanban_store_routes.go), so it is
// not proxied: the caller reaches it through the request context, not a
// header.
const IssuesMountPrefix = "/issues"

// GrantsMountPrefix is where grant-store's routes are mounted.
// grant-store runs in this process (access_routes.go), so they are not
// proxied: the caller reaches it through the request context, not a header.
const GrantsMountPrefix = "/grants"

// MailMountPrefix is where mailstack's routes are mounted: the
// remainder of the path is answered at mailstack's root, where /api/…
// lives. mailstack runs in this process (mailstack_routes.go), so it is
// not proxied: the caller reaches it through the request context.
const MailMountPrefix = "/mail"

// MailstackIngressDeliveryPattern is pushed delivery, answered by mailstack
// against the ingress credential the sender presents.
const MailstackIngressDeliveryPattern = "POST " + MailMountPrefix + "/api/ingress/deliveries"
