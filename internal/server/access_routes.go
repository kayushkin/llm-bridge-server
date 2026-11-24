package server

// access: principal-store, grant-store, sign-in-store and permission-store,
// run in this process (internal/access). This file mounts their routes under
// /principal-store/, /grant-store/, /sign-in-store/ and /permission-store/,
// the same paths behind each prefix as the standalone store's, and joins the
// stores to the rest of the server: the principal behind every request, the
// grants a session is held to, and the verdict on a tool call are now calls
// into these stores, not requests over HTTP.
//
// Who reaches each route is decided by routeAccessRules, as for every route.
// What the stores used to read from headers a gateway set — X-Principal-Id,
// X-Grant-Store-Service-Token, X-Sign-In-Store-Service-Token — they now read
// from the request context, set here from the caller request authorization
// verified. A client cannot set a context value, so there is nothing left to
// strip.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	permstore "github.com/kayushkin/llm-bridge-server/internal/access/permissionstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/signinstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/permclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
)

// errAccessOff is every use of an access store on a server whose access
// stores are not open: one built by New and never given EnableAccess, which
// only a test does, since main refuses to start without them.
var errAccessOff = errors.New("access is off: principal-store, grant-store, sign-in-store and permission-store are not open on this server (" +
	config.PostgresURLEnvironmentVariable + ")")

// principalDirectory is principal-store as this server reads and writes it;
// principalclient.Client is the one in production.
type principalDirectory interface {
	CheckExists(ctx context.Context, principalID string) error
	Get(ctx context.Context, principalID string) (principalclient.Principal, error)
	GroupIDsOf(ctx context.Context, principalID string) ([]string, error)
	ResolveIdentityBinding(ctx context.Context, provider, tenantID, subject string) (principalclient.IdentityResolution, error)
	Create(ctx context.Context, kind, displayName string) (string, error)
	SetAdministrator(ctx context.Context, principalID string, isAdministrator bool) error
	AddMember(ctx context.Context, groupID, memberID string) error
	RemoveMember(ctx context.Context, groupID, memberID string) error
}

// grantReader is grant-store as this server reads and writes it;
// grantclient.Client is the one in production.
type grantReader interface {
	EffectiveResourceIDs(ctx context.Context, principalID, relation, resourceType string) ([]string, error)
	Create(ctx context.Context, input grantclient.GrantInput) (string, error)
}

// permissionEvaluator is permission-store's verdict on one tool call;
// permclient.Client is the one in production.
type permissionEvaluator interface {
	Evaluate(ctx context.Context, request permclient.Request) (*permclient.Result, error)
}

// accessRouteHandlers is what answers the mounted routes.
type accessRouteHandlers struct {
	principals  *principalstore.Handlers
	grants      *grantstore.Handlers
	permissions *permstore.Handlers
	// signIn is nil when sign-in is not configured (signInConfiguration).
	signIn *signinstore.Handlers
	// signInOffReason says why signIn is nil.
	signInOffReason string
	// principalStoreRoot answers principal-store's routes at their own root
	// paths, for the identity binding administration routes, which pass a
	// request through to the store and its answer back unchanged.
	principalStoreRoot http.Handler
}

// EnableAccess joins the open access stores to this server: the clients every
// other handler reads principals, grants and verdicts through, and the
// mounted routes. main calls it once, before the server listens.
func (s *Server) EnableAccess(stores *access.Stores) {
	grantsDirectory := access.GrantsPrincipalDirectory(stores.Principals)
	checker := s.grantResourceChecker(stores.Principals)
	s.principalClient = principalclient.New(stores.Principals)
	s.grantClient = grantclient.New(stores.Grants, grantsDirectory, checker)
	s.permClient = permclient.New(stores.PermissionEngine)

	principals := principalstore.NewHandlers(stores.Principals)
	handlers := &accessRouteHandlers{
		principals:         principals,
		grants:             grantstore.NewHandlers(stores.Grants, grantsDirectory, checker),
		permissions:        permstore.NewHandlers(stores.Permissions, stores.PermissionEngine),
		principalStoreRoot: access.IdentityBindingRoutesAtTheirOwnPaths(principals),
	}
	directoryConfig, handlersConfig, offReason := s.signInConfiguration()
	if offReason == "" {
		directory := signinstore.NewDirectory(access.SignInPrincipalDirectory(stores.Principals), directoryConfig)
		handlers.signIn = signinstore.NewHandlers(stores.SignIn, directory, handlersConfig)
	} else {
		handlers.signInOffReason = offReason
	}
	s.access.Store(handlers)
}

// signInConfiguration is sign-in-store's wiring from this server's settings,
// or why sign-in is off. The sign-in page and the cookie domain name a site,
// so neither has a default: without both, a link would point nowhere and a
// session cookie would belong to no domain.
func (s *Server) signInConfiguration() (signinstore.DirectoryConfig, signinstore.HandlersConfig, string) {
	if s.cfg.SignInPageURL == "" || s.cfg.SignInCookieDomain == "" {
		return signinstore.DirectoryConfig{}, signinstore.HandlersConfig{}, fmt.Sprintf(
			"sign-in is off: %s and %s must both be set", config.SignInPageURLEnvironmentVariable, config.SignInCookieDomainEnvironmentVariable)
	}
	return signinstore.DirectoryConfig{
			PeopleStoreURL:        s.cfg.PeopleStoreURL,
			MultichatURL:          s.cfg.MultichatURL,
			AuthStoreURL:          s.cfg.AuthStoreURL,
			AuthStoreToken:        s.cfg.AuthStoreToken,
			MultichatCredentialID: s.cfg.SignInMultichatCredentialID,
		}, signinstore.HandlersConfig{
			SignInPageURL: s.cfg.SignInPageURL,
			CookieDomain:  s.cfg.SignInCookieDomain,
			Discord: signinstore.DiscordConfig{
				ApplicationID:            s.cfg.SignInDiscordApplicationID,
				ClientSecretCredentialID: s.cfg.SignInDiscordClientSecretCredentialID,
				RedirectURL:              s.cfg.SignInDiscordRedirectURL,
			},
		}, ""
}

// accessHandlersOrRefuse is the mounted routes' handlers, or a 503 when the
// access stores are not open.
func (s *Server) accessHandlersOrRefuse(w http.ResponseWriter) (*accessRouteHandlers, bool) {
	handlers := s.access.Load()
	if handlers == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "access_off", errAccessOff.Error())
		return nil, false
	}
	return handlers, true
}

// principalStoreRoute answers one principal-store route. A request that may
// change a principal forgets every cached principal afterwards, so a person
// disabled or demoted here is refused on their next request rather than after
// the cache's 30 seconds.
func (s *Server) principalStoreRoute(route func(*principalstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers, ok := s.accessHandlersOrRefuse(w)
		if !ok {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			defer s.principalLookupCache.forgetEveryone()
		}
		route(handlers.principals, w, r)
	}
}

// requestIsTheInternalServiceActingAsItself reports whether the request
// presented this server's service token and named no principal. Request
// authorization has already refused a wrong token; this checks the token
// again rather than infer it from a missing caller, so no other way of
// arriving without a caller can be mistaken for it.
func (s *Server) requestIsTheInternalServiceActingAsItself(r *http.Request) bool {
	if _, identified := principalIdentityOfRequest(r); identified {
		return false
	}
	if principalIdentityHeaderNameIn(r.Header) != "" {
		return false
	}
	presented := r.Header.Values(serviceTokenHeader)
	return len(presented) == 1 && s.cfg.ServiceToken != "" &&
		subtle.ConstantTimeCompare([]byte(presented[0]), []byte(s.cfg.ServiceToken)) == 1
}

// grantStoreRoute answers one grant-store route as the caller: the verified
// principal, narrowed by grant-store's own rules, or the internal service,
// unrestricted, as grant-store's service token was. A request with neither
// reaches grant-store with no caller, and grant-store answers it 401.
func (s *Server) grantStoreRoute(route func(*grantstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers, ok := s.accessHandlersOrRefuse(w)
		if !ok {
			return
		}
		ctx := r.Context()
		if principalID, identified := principalIdentityOfRequest(r); identified {
			ctx = grantstore.ContextWithCaller(ctx, grantstore.Caller{PrincipalID: principalID})
		} else if s.requestIsTheInternalServiceActingAsItself(r) {
			ctx = grantstore.ContextWithCaller(ctx, grantstore.Caller{InternalService: true})
		}
		route(handlers.grants, w, r.WithContext(ctx))
	}
}

// signInStoreRoute answers one sign-in-store browser route. They check the
// browser's own cookie or secret themselves.
func (s *Server) signInStoreRoute(route func(*signinstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers, ok := s.accessHandlersOrRefuse(w)
		if !ok {
			return
		}
		if handlers.signIn == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "sign_in_off", handlers.signInOffReason)
			return
		}
		route(handlers.signIn, w, r)
	}
}

// signInStoreAdministratorRoute answers one sign-in-store administrator route
// as the verified principal, which the route rules have already required to be
// an administrator. The service token naming nobody reaches it with no
// administrator, and sign-in-store answers 403: every link and revocation
// records the administrator who made it.
func (s *Server) signInStoreAdministratorRoute(route func(*signinstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.signInStoreRoute(func(handlers *signinstore.Handlers, w http.ResponseWriter, r *http.Request) {
		if principalID, identified := principalIdentityOfRequest(r); identified {
			r = r.WithContext(signinstore.ContextWithAdministrator(r.Context(), principalID))
		}
		route(handlers, w, r)
	})
}

// permissionStoreRoute answers one permission-store route.
func (s *Server) permissionStoreRoute(route func(*permstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers, ok := s.accessHandlersOrRefuse(w)
		if !ok {
			return
		}
		route(handlers.permissions, w, r)
	}
}

// registerAccessRoutes mounts the four stores. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerAccessRoutes() {
	// principal-store.
	s.mux.HandleFunc("GET /principal-store/health", s.principalStoreRoute((*principalstore.Handlers).Health))
	s.mux.HandleFunc("GET /principal-store/kinds", s.principalStoreRoute((*principalstore.Handlers).Kinds))
	s.mux.HandleFunc("GET /principal-store/kind-definitions", s.principalStoreRoute((*principalstore.Handlers).KindDefinitions))
	s.mux.HandleFunc("GET /principal-store/availability-reasons", s.principalStoreRoute((*principalstore.Handlers).AvailabilityReasons))
	s.mux.HandleFunc("GET /principal-store/weekday-codes", s.principalStoreRoute((*principalstore.Handlers).WeekdayCodes))
	s.mux.HandleFunc("GET /principal-store/principals", s.principalStoreRoute((*principalstore.Handlers).ListPrincipals))
	s.mux.HandleFunc("POST /principal-store/principals", s.principalStoreRoute((*principalstore.Handlers).CreatePrincipal))
	s.mux.HandleFunc("POST /principal-store/contacts/resolve", s.principalStoreRoute((*principalstore.Handlers).ResolveContact))
	s.mux.HandleFunc("GET /principal-store/principals/{id}", s.principalStoreRoute((*principalstore.Handlers).GetPrincipal))
	s.mux.HandleFunc("PATCH /principal-store/principals/{id}", s.principalStoreRoute((*principalstore.Handlers).PatchPrincipal))
	s.mux.HandleFunc("POST /principal-store/principals/{id}/disable", s.principalStoreRoute((*principalstore.Handlers).DisablePrincipal))
	s.mux.HandleFunc("POST /principal-store/principals/{id}/enable", s.principalStoreRoute((*principalstore.Handlers).EnablePrincipal))
	s.mux.HandleFunc("GET /principal-store/principals/{id}/members", s.principalStoreRoute((*principalstore.Handlers).ListMembers))
	s.mux.HandleFunc("PUT /principal-store/principals/{group}/members/{member}", s.principalStoreRoute((*principalstore.Handlers).PutMember))
	s.mux.HandleFunc("DELETE /principal-store/principals/{group}/members/{member}", s.principalStoreRoute((*principalstore.Handlers).DeleteMember))
	s.mux.HandleFunc("GET /principal-store/principals/{id}/groups", s.principalStoreRoute((*principalstore.Handlers).ListGroups))
	s.mux.HandleFunc("GET /principal-store/principals/{id}/availability", s.principalStoreRoute((*principalstore.Handlers).Availability))
	s.mux.HandleFunc("GET /principal-store/principals/{id}/time-off", s.principalStoreRoute((*principalstore.Handlers).ListTimeOff))
	s.mux.HandleFunc("POST /principal-store/principals/{id}/time-off", s.principalStoreRoute((*principalstore.Handlers).AddTimeOff))
	s.mux.HandleFunc("DELETE /principal-store/principals/{id}/time-off/{timeOffID}", s.principalStoreRoute((*principalstore.Handlers).DeleteTimeOff))
	s.mux.HandleFunc("GET /principal-store/identity-providers", s.principalStoreRoute((*principalstore.Handlers).IdentityProviders))
	s.mux.HandleFunc("GET /principal-store/identity-bindings", s.principalStoreRoute((*principalstore.Handlers).ListIdentityBindings))
	s.mux.HandleFunc("GET /principal-store/identity-bindings/resolve", s.principalStoreRoute((*principalstore.Handlers).ResolveIdentity))
	s.mux.HandleFunc("GET /principal-store/identity-bindings/{id}", s.principalStoreRoute((*principalstore.Handlers).GetIdentityBinding))
	// Linking and unlinking go through the administration handlers, which
	// stamp linked_by and unlinked_by with the caller rather than the body,
	// and end the login sessions of an unlinked identity. Mounted straight,
	// these two would let an administrator record another as the linker and
	// leave an unlinked identity's sessions signed in.
	s.mux.HandleFunc("POST /principal-store/identity-bindings", s.handleLinkIdentityBinding)
	s.mux.HandleFunc("POST /principal-store/identity-bindings/{id}/unlink", s.handleUnlinkIdentityBinding)

	// grant-store.
	s.mux.HandleFunc("GET /grant-store/health", s.grantStoreRoute((*grantstore.Handlers).Health))
	s.mux.HandleFunc("GET /grant-store/relations", s.grantStoreRoute((*grantstore.Handlers).Relations))
	s.mux.HandleFunc("GET /grant-store/resource-types", s.grantStoreRoute((*grantstore.Handlers).ResourceTypes))
	s.mux.HandleFunc("GET /grant-store/grants", s.grantStoreRoute((*grantstore.Handlers).ListGrants))
	s.mux.HandleFunc("POST /grant-store/grants", s.grantStoreRoute((*grantstore.Handlers).CreateGrant))
	s.mux.HandleFunc("GET /grant-store/grants/{id}", s.grantStoreRoute((*grantstore.Handlers).GetGrant))
	s.mux.HandleFunc("POST /grant-store/grants/{id}/revoke", s.grantStoreRoute((*grantstore.Handlers).RevokeGrant))
	s.mux.HandleFunc("GET /grant-store/principals/{id}/effective", s.grantStoreRoute((*grantstore.Handlers).Effective))

	// sign-in-store.
	s.mux.HandleFunc("GET /sign-in-store/health", s.signInStoreRoute((*signinstore.Handlers).Health))
	s.mux.HandleFunc("POST /sign-in-store/link-requests", s.signInStoreRoute((*signinstore.Handlers).RequestLink))
	s.mux.HandleFunc("POST /sign-in-store/redeem", s.signInStoreRoute((*signinstore.Handlers).Redeem))
	s.mux.HandleFunc("GET /sign-in-store/session", s.signInStoreRoute((*signinstore.Handlers).CurrentSession))
	s.mux.HandleFunc("POST /sign-in-store/sign-out", s.signInStoreRoute((*signinstore.Handlers).SignOut))
	s.mux.HandleFunc("GET /sign-in-store/discord/start", s.signInStoreRoute((*signinstore.Handlers).StartDiscord))
	s.mux.HandleFunc("GET /sign-in-store/discord/callback", s.signInStoreRoute((*signinstore.Handlers).FinishDiscord))
	s.mux.HandleFunc("POST /sign-in-store/sign-in-links", s.signInStoreAdministratorRoute((*signinstore.Handlers).MakeLink))
	s.mux.HandleFunc("GET /sign-in-store/sign-in-links", s.signInStoreAdministratorRoute((*signinstore.Handlers).ListLinks))
	s.mux.HandleFunc("GET /sign-in-store/sessions", s.signInStoreAdministratorRoute((*signinstore.Handlers).ListSessions))
	s.mux.HandleFunc("POST /sign-in-store/sessions/{id}/revoke", s.signInStoreAdministratorRoute((*signinstore.Handlers).RevokeSession))
	s.mux.HandleFunc("POST /sign-in-store/principals/{id}/discord-messages", s.signInStoreAdministratorRoute((*signinstore.Handlers).SendDiscordMessage))

	// permission-store.
	s.mux.HandleFunc("GET /permission-store/healthz", s.permissionStoreRoute((*permstore.Handlers).Health))
	s.mux.HandleFunc("GET /permission-store/rules", s.permissionStoreRoute((*permstore.Handlers).ListRules))
	s.mux.HandleFunc("POST /permission-store/rules", s.permissionStoreRoute((*permstore.Handlers).CreateRule))
	s.mux.HandleFunc("GET /permission-store/rules/{id}", s.permissionStoreRoute((*permstore.Handlers).GetRule))
	s.mux.HandleFunc("PATCH /permission-store/rules/{id}", s.permissionStoreRoute((*permstore.Handlers).UpdateRule))
	s.mux.HandleFunc("DELETE /permission-store/rules/{id}", s.permissionStoreRoute((*permstore.Handlers).DeleteRule))
	s.mux.HandleFunc("POST /permission-store/evaluate", s.permissionStoreRoute((*permstore.Handlers).Evaluate))
	s.mux.HandleFunc("GET /permission-store/audit", s.permissionStoreRoute((*permstore.Handlers).ListAudit))
}

// writePrincipalStoreCallError answers a call into principal-store that did not
// reach its handler: 503 when access is off, 500 otherwise.
func writePrincipalStoreCallError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAccessOff) {
		writeJSONError(w, http.StatusServiceUnavailable, "access_off", err.Error())
		return
	}
	writeJSONError(w, http.StatusInternalServerError, "principal_store_call_failed", err.Error())
}
