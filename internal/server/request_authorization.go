package server

// Request authorization.
//
// This server is the whole backend of a multi-user product, and every request
// passes through authorizeAndServe before the mux dispatches it. There is no
// way to switch that off: the credentials it needs are mandatory at startup
// (config.ValidateRequestAuthorizationSettings).
//
// Every route is classified in RouteAccessRules, keyed by the exact pattern
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
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/server/routeaccess"

	"github.com/kayushkin/llm-bridge-server/internal/access/login"

	"github.com/kayushkin/llm-bridge-server/internal/peers"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"
)

// authorizeAndServe is ServeHTTP: every request reaches the mux through here.
func (s *Server) authorizeAndServe(w http.ResponseWriter, r *http.Request) {
	_, pattern := s.mux.Handler(r)
	if pattern == "" {
		// No route matches: the mux answers 404, 405 or a redirect, none of
		// which reaches a handler.
		s.mux.ServeHTTP(w, r)
		return
	}

	presentedPeerTokens, peerTokenPresent := r.Header[http.CanonicalHeaderKey(requestcaller.PeerTokenHeader)]
	presentedPeerUsers, peerUserPresent := r.Header[http.CanonicalHeaderKey(requestcaller.PeerUserHeader)]
	if peerUserPresent && !peerTokenPresent {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "peer_user_without_peer_token", fmt.Sprintf(
			"%s names a connected server's user and means nothing without %s", requestcaller.PeerUserHeader, requestcaller.PeerTokenHeader))
		return
	}
	if _, serviceTokenPresent := r.Header[http.CanonicalHeaderKey(requestcaller.ServiceTokenHeader)]; peerTokenPresent && serviceTokenPresent {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "ambiguous_credentials", fmt.Sprintf(
			"%s and %s were both sent; a request comes from one caller", requestcaller.ServiceTokenHeader, requestcaller.PeerTokenHeader))
		return
	}

	if presentedServiceToken, present := r.Header[http.CanonicalHeaderKey(requestcaller.ServiceTokenHeader)]; present {
		if len(presentedServiceToken) != 1 || subtle.ConstantTimeCompare([]byte(presentedServiceToken[0]), []byte(s.cfg.ServiceToken)) != 1 {
			httpjson.WriteJSONError(w, http.StatusUnauthorized, "invalid_service_token", requestcaller.ServiceTokenHeader+" does not match this server's service token")
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
			httpjson.WriteJSONError(w, http.StatusBadRequest, "ambiguous_principal_header", fmt.Sprintf(
				"%s was sent %d times (%v); a request acts as one principal", assertedPrincipalHeaderName, len(assertedPrincipalIDs), assertedPrincipalIDs))
			return
		}
		caller, credentialError := s.callerAssertedByInternalService(r.Context(), strings.TrimSpace(assertedPrincipalIDs[0]))
		if credentialError != nil {
			credentialError.Write(w)
			return
		}
		rule, classified := routeaccess.RouteAccessRules[pattern]
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
		httpjson.WriteJSONError(w, http.StatusUnauthorized, "principal_header_without_service_token", fmt.Sprintf(
			"%s says which principal a request acts as and is believed only from a caller presenting %s; sign in with GET /auth/entra/start or POST /auth/demo-login instead",
			headerName, requestcaller.ServiceTokenHeader))
		return
	}

	rule, classified := routeaccess.RouteAccessRules[pattern]
	if !classified {
		refuseUnclassifiedRoute(w, pattern)
		return
	}

	// The harness callbacks take no credential: they are for harness children,
	// sidecars and runner tunnels on this host. A request that came through the
	// public reverse proxy carries the headers it sets, and is refused here
	// whatever else it presents, so a mistake in the proxy's rules cannot open
	// them. The runner's own routes are exempt (answersThroughPublicProxy).
	if rule.Class == routeaccess.RouteHarnessCallback && !rule.AnswersThroughPublicProxy && requestcaller.RequestCameThroughPublicProxy(r) {
		httpjson.WriteJSONError(w, http.StatusForbidden, "harness_callback_not_public", fmt.Sprintf(
			"%s is called by this host's own processes and is not answered through the public proxy", pattern))
		return
	}

	// A request another server sent over a peer socket acts only as the peer
	// its token names, or is a connection call: the routes open to everyone
	// and the harness callbacks are for this host's own callers, never for a
	// peer, however it arrived.
	socketConnectionID, arrivedOverPeerSocket := peers.PeerSocketConnectionOfRequest(r)
	if arrivedOverPeerSocket && !peerTokenPresent && rule.Class != routeaccess.RoutePeerConnectionCall {
		httpjson.WriteJSONError(w, http.StatusForbidden, "peer_socket_requires_peer_token", fmt.Sprintf(
			"a request over a peer socket must present %s, or be a connection call; %s is neither", requestcaller.PeerTokenHeader, pattern))
		return
	}

	// Another llm-bridge-server acting as the peer principal its token was
	// issued for. From here it is held to the route rules exactly as a person
	// signed in as that principal would be.
	if peerTokenPresent {
		if len(presentedPeerTokens) != 1 || strings.TrimSpace(presentedPeerTokens[0]) == "" {
			httpjson.WriteJSONError(w, http.StatusBadRequest, "ambiguous_peer_token", fmt.Sprintf(
				"%s was sent %d times or empty; a request presents one peer token", requestcaller.PeerTokenHeader, len(presentedPeerTokens)))
			return
		}
		if _, hasAuthorization := r.Header["Authorization"]; hasAuthorization {
			httpjson.WriteJSONError(w, http.StatusBadRequest, "ambiguous_credentials", fmt.Sprintf(
				"%s and an Authorization header were both sent; a request comes from one caller", requestcaller.PeerTokenHeader))
			return
		}
		if !arrivedOverPeerSocket && s.cfg.DemoLoginEnabled {
			httpjson.WriteJSONError(w, http.StatusConflict, "demo_login_enabled",
				"a peer token by https is refused while demo login is on: it is a call from another machine, and demo login signs anyone in as anyone; a peer reaches this server over the socket this server dialed")
			return
		}
		if rule.Class == routeaccess.RouteHarnessCallback || rule.Class == routeaccess.RoutePeerConnectionCall {
			httpjson.WriteJSONError(w, http.StatusForbidden, "route_not_for_peers", fmt.Sprintf(
				"%s is called by this host's own processes or as a connection call, never with a peer token", pattern))
			return
		}
		if rule.SignsIn {
			httpjson.WriteJSONError(w, http.StatusForbidden, "route_not_for_peers", fmt.Sprintf(
				"%s signs a person in or out of this server, and another server never does that", pattern))
			return
		}
		caller, credentialError := s.callerOfPeerToken(r.Context(), presentedPeerTokens[0], socketConnectionID, arrivedOverPeerSocket)
		if credentialError != nil {
			credentialError.Write(w)
			return
		}
		if !arrivedOverPeerSocket {
			// Over a socket, serveRequest has already read and stamped the id and name.
			s.peers.NoteOtherServerIdentity(caller.Peer.ConnectionID, r.Header)
			s.peers.StampThisServersIdentity(w.Header())
		}
		if peerUserPresent {
			if len(presentedPeerUsers) != 1 || strings.TrimSpace(presentedPeerUsers[0]) == "" {
				httpjson.WriteJSONError(w, http.StatusBadRequest, "ambiguous_peer_user", fmt.Sprintf(
					"%s was sent %d times or empty; a request names one user", requestcaller.PeerUserHeader, len(presentedPeerUsers)))
				return
			}
			caller, credentialError = s.callerForPeerUser(r.Context(), caller, strings.TrimSpace(presentedPeerUsers[0]))
			if credentialError != nil {
				credentialError.Write(w)
				return
			}
		}
		trackedRequest, forget := s.peerRequestsInFlight.Track(r, caller.Peer)
		defer forget()
		s.serveAsCaller(w, trackedRequest, pattern, rule, caller)
		return
	}

	switch rule.Class {
	case routeaccess.RouteOpenToEveryone, routeaccess.RouteHarnessCallback, routeaccess.RoutePeerConnectionCall:
		s.mux.ServeHTTP(w, r)
		return
	}

	// A session agent token and a service principal token go to the principal
	// path below on every route; any other bearer on these routes is a session
	// posting token.
	_, hasAuthorization := r.Header["Authorization"]
	presentedBearer := bearerTokenOf(r)
	presentsPostingToken := hasAuthorization && !login.IsSessionAgentToken(presentedBearer) && !login.IsServicePrincipalToken(presentedBearer)
	if presentsPostingToken && rule.Class == routeaccess.RouteSessionOwnerOrItsAgentPostsIntoSession {
		caller, credentialError := s.postingAgentOfRequest(r, pattern, rule)
		if credentialError != nil {
			credentialError.Write(w)
			return
		}
		s.mux.ServeHTTP(w, requestcaller.WithCaller(r, caller))
		return
	}

	if presentsPostingToken && rule.SessionAgentAccess != routeaccess.SessionAgentNotAccepted {
		s.serveDelegatingAgent(w, r, pattern, rule)
		return
	}

	principalID, loginSessionID, sessionAgentTokenSessionID, credentialError := s.principalOfRequest(r, rule)
	if credentialError != nil {
		credentialError.Write(w)
		return
	}
	caller, credentialError := s.callerActingAsPrincipal(r.Context(), principalID)
	if credentialError != nil {
		credentialError.Write(w)
		return
	}
	if sessionAgentTokenSessionID != "" && rule.Class != routeaccess.RoutePrincipalOrSessionAgentThroughStoreProxy {
		if caller, credentialError = s.administratorOfSessionAgentToken(r, pattern, caller, sessionAgentTokenSessionID); credentialError != nil {
			credentialError.Write(w)
			return
		}
	}
	caller.LoginSessionID = loginSessionID
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
func (s *Server) administratorOfSessionAgentToken(r *http.Request, pattern string, caller requestcaller.Caller, sessionID string) (requestcaller.Caller, *requestcaller.CredentialError) {
	caller, credentialError := s.administratorOnlyIfSessionPeerIsOne(r.Context(), caller, sessionID)
	if credentialError != nil {
		return requestcaller.Caller{}, credentialError
	}
	if !caller.IsAdministrator {
		return requestcaller.Caller{}, requestcaller.NewCredentialError(http.StatusUnauthorized, "authorization_header_not_accepted", fmt.Sprintf(
			"a session agent token is accepted on %s only for a principal principal-store marks is_administrator, and %s is not one; anyone's is accepted on %s/, %s/, %s/ and %s/",
			pattern, caller.PrincipalID, routeaccess.IssuesMountPrefix, routeaccess.GrantsMountPrefix, routeaccess.MailMountPrefix, referencesMountPrefix))
	}
	if requestcaller.RequestCameThroughPublicProxy(r) {
		return requestcaller.Caller{}, requestcaller.NewCredentialError(http.StatusForbidden, "session_agent_token_not_public", fmt.Sprintf(
			"an administrator's session agent token reaches %s only from this host, never through the public proxy", pattern))
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

// refuseUnclassifiedRoute answers a route nobody has written a rule for.
func refuseUnclassifiedRoute(w http.ResponseWriter, pattern string) {
	httpjson.WriteJSONError(w, http.StatusForbidden, "route_not_classified", fmt.Sprintf(
		"route %q has no access rule, so only the service token may call it", pattern))
}

// serveAsCaller applies the route's rule to a caller acting as a principal and
// dispatches, or writes the refusal the rule decides on.
func (s *Server) serveAsCaller(w http.ResponseWriter, r *http.Request, pattern string, rule routeaccess.RouteAccessRule, caller requestcaller.Caller) {
	switch rule.Class {
	case routeaccess.RouteOpenToEveryone, routeaccess.RouteHarnessCallback:
		s.mux.ServeHTTP(w, requestcaller.WithCaller(r, caller))
		return
	}
	if caller.Peer != nil && rule.Class == routeaccess.RoutePrincipalActsThroughPeerConnection {
		httpjson.WriteJSONError(w, http.StatusForbidden, "route_not_for_peers", fmt.Sprintf(
			"%s reaches a server connected to this one, and a connected server may not go on through it to another", pattern))
		return
	}
	if caller.IsAdministrator {
		// principal-store says this person may do anything here, so no
		// per-resource check applies. The identity still travels with the
		// request: the store proxies set X-Principal-Id from it, and
		// kanban-store and grant-store make their own administrator check.
		s.mux.ServeHTTP(w, requestcaller.WithCaller(r, caller))
		return
	}
	if caller.Peer.ActsForRemoteUser() && routeClassIsRefusedToPeerUsers(rule.Class) {
		httpjson.WriteJSONError(w, http.StatusForbidden, "route_not_for_peer_users", fmt.Sprintf(
			"%s answers as one principal from a store or record this server cannot narrow to what both the connected server and its mapped user may do, so a connected server's user is refused it (%s)", pattern, rule.Reason))
		return
	}
	switch rule.Class {
	case routeaccess.RouteOperatorOnly:
		httpjson.WriteJSONError(w, http.StatusForbidden, "operator_route", fmt.Sprintf(
			"operator route: use the service token (%s: %s)", pattern, rule.Reason))
		return
	case routeaccess.RoutePrincipalRefusedCannotBeFiltered:
		httpjson.WriteJSONError(w, http.StatusForbidden, "route_not_available_to_principals", fmt.Sprintf(
			"%s cannot be narrowed to one principal's sessions (%s), so it is refused rather than answered across principals; use the service token", pattern, rule.Reason))
		return
	case routeaccess.RoutePrincipalOwnsSession, routeaccess.RouteSessionOwnerOrItsAgentPostsIntoSession:
		sessionID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.OwnedResourcePathValueName)
		if err != nil {
			httpjson.WriteJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		reached, err := s.peers.SessionReachedByCaller(r.Context(), caller, sessionID, peers.RequestOnlyReads(r, pattern))
		if err != nil {
			httpjson.WriteJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
			return
		}
		if !reached {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	case routeaccess.RoutePrincipalOwnsSignal:
		signalID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.OwnedResourcePathValueName)
		if err != nil {
			httpjson.WriteJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		signal, err := s.store.GetSignal(signalID)
		if err != nil {
			http.Error(w, "signal not found", http.StatusNotFound)
			return
		}
		if !requestcaller.SessionScopeOfCaller(caller).ReachesSessionID(s.store, signal.SessionID) {
			observes := false
			if peers.RequestOnlyReads(r, pattern) {
				if observes, err = s.peers.MayObserveSession(r.Context(), caller, signal.SessionID); err != nil {
					httpjson.WriteJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
					return
				}
			}
			if !observes {
				http.Error(w, "signal not found", http.StatusNotFound)
				return
			}
		}
	case routeaccess.RoutePrincipalOwnsOperation:
		operationID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.OwnedResourcePathValueName)
		if err != nil {
			httpjson.WriteJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		if s.operations != nil && !s.operationRoutes.OperationIsOwnedByPrincipal(operationID, caller.PrincipalID) {
			http.Error(w, "operation not found", http.StatusNotFound)
			return
		}
	case routeaccess.RoutePrincipalSeesOwnSessionsOnly:
		if peers.RequestOnlyReads(r, pattern) {
			observes, err := s.peers.MayObserveEverySession(r, caller)
			if err != nil {
				httpjson.WriteJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
				return
			}
			caller.ObservesEverySession = observes
		}
	case routeaccess.RoutePrincipalCreatesSession, routeaccess.RoutePrincipalReadsCatalog, routeaccess.RoutePrincipalActsThroughPeerConnection,
		routeaccess.RoutePrincipalOrSessionAgentThroughStoreProxy, routeaccess.RoutePrincipalCreatesOperation, routeaccess.RoutePrincipalSeesOwnOperationsOnly,
		routeaccess.RoutePrincipalManagesOwnLoginSessions, routeaccess.RoutePrincipalSeesPeerConnectionsItMayCallThrough:
	default:
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "route_rule_invalid", fmt.Sprintf("route %q has an access class this server does not handle", pattern))
		return
	}
	s.mux.ServeHTTP(w, requestcaller.WithCaller(r, caller))
}

// principalIdentityHeaderNameIn returns the key a request carries
// X-Principal-Id under, in whatever casing it was set, and "" when it carries
// none. Header.Get would miss a non-canonical key set directly on the map, and
// a header this server refuses to believe must be found however it is spelled.
func principalIdentityHeaderNameIn(header http.Header) string {
	for key, values := range header {
		if !strings.EqualFold(key, requestcaller.PrincipalIdentityHeader) {
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

// principalOfRequest establishes which principal a request acts as, from its
// login cookie or its session agent token, and returns the login session's id
// when the cookie was the credential, or the session agent token's session
// when the token was. A cookie-authenticated request with an unsafe method is
// refused here unless it carries the session's CSRF token. Whether the token
// may be used on this route is the caller's decision: anyone's on the store
// proxies, an administrator's elsewhere.
func (s *Server) principalOfRequest(r *http.Request, rule routeaccess.RouteAccessRule) (string, string, string, *requestcaller.CredentialError) {
	if _, hasAuthorization := r.Header["Authorization"]; hasAuthorization {
		token := bearerTokenOf(r)
		// A service's own token is accepted on every route, as the principal
		// it was issued for (service_principal_tokens.go).
		if login.IsServicePrincipalToken(token) {
			principalID, credentialError := s.login.PrincipalOfServicePrincipalToken(r.Context(), r, token)
			return principalID, "", "", credentialError
		}
		if rule.Class != routeaccess.RoutePrincipalOrSessionAgentThroughStoreProxy && !login.IsSessionAgentToken(token) {
			return "", "", "", requestcaller.NewCredentialError(http.StatusUnauthorized, "authorization_header_not_accepted", fmt.Sprintf(
				"an Authorization header is accepted on %s/, %s/, %s/ and %s/ as a session agent token, and elsewhere only as an administrator's session agent token; this route takes the login cookie or %s",
				routeaccess.IssuesMountPrefix, routeaccess.GrantsMountPrefix, routeaccess.MailMountPrefix, referencesMountPrefix, requestcaller.ServiceTokenHeader))
		}
		if token == "" {
			return "", "", "", requestcaller.NewCredentialError(http.StatusUnauthorized, "invalid_session_agent_token",
				"the Authorization header must be \"Bearer <session agent token>\"")
		}
		claims, err := s.login.SessionAgentTokenClaimsOfLiveSession(token)
		if err != nil {
			return "", "", "", requestcaller.NewCredentialError(http.StatusUnauthorized, "invalid_session_agent_token", err.Error())
		}
		return claims.PrincipalID, "", claims.SessionID, nil
	}
	session, refusal := s.login.LiveLoginSessionOfRequest(r)
	if refusal != nil {
		return "", "", "", refusal
	}
	if refusal := login.CsrfRefusalOf(r, session); refusal != nil {
		return "", "", "", refusal
	}
	return session.PrincipalID, session.ID, "", nil
}

// postingAgentOfRequest verifies a session posting token and holds it to the
// session in the path. A token for another session is 404, the same answer a
// principal gets for a session that is not theirs.
func (s *Server) postingAgentOfRequest(r *http.Request, pattern string, rule routeaccess.RouteAccessRule) (requestcaller.Caller, *requestcaller.CredentialError) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return requestcaller.Caller{}, requestcaller.NewCredentialError(http.StatusUnauthorized, "invalid_session_posting_token",
			"the Authorization header must be \"Bearer <session posting token>\"")
	}
	tokenSession, credentialError := s.login.VerifiedPostingSession(token)
	if credentialError != nil {
		return requestcaller.Caller{}, credentialError
	}
	tokenSessionID := tokenSession.SessionID
	pathSessionID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.OwnedResourcePathValueName)
	if err != nil {
		return requestcaller.Caller{}, requestcaller.NewCredentialError(http.StatusInternalServerError, "route_rule_invalid", err.Error())
	}
	session, err := s.store.GetSession(pathSessionID)
	if err != nil || session.SessionID != tokenSessionID {
		return requestcaller.Caller{}, requestcaller.NewCredentialError(http.StatusNotFound, "session_not_found", "session not found")
	}
	return requestcaller.Caller{PostingAgentOfSessionID: tokenSessionID}, nil
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

// routeClassesRefusedToPeerUsers are the classes whose answer comes from a
// store or record this server cannot narrow to the intersection of two
// principals, so a connected server's user is refused them: the store
// proxies act as the principal in another store, and operations and login
// sessions are a principal's own with no peer origin.
var routeClassesRefusedToPeerUsers = []routeaccess.RouteAccessClass{
	routeaccess.RoutePrincipalOrSessionAgentThroughStoreProxy,
	routeaccess.RoutePrincipalCreatesOperation,
	routeaccess.RoutePrincipalOwnsOperation,
	routeaccess.RoutePrincipalSeesOwnOperationsOnly,
	routeaccess.RoutePrincipalManagesOwnLoginSessions,
}

func routeClassIsRefusedToPeerUsers(class routeaccess.RouteAccessClass) bool {
	for _, refused := range routeClassesRefusedToPeerUsers {
		if refused == class {
			return true
		}
	}
	return false
}
