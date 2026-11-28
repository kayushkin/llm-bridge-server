package server

// access: principal-store, grant-store, sign-in-store and permission-store,
// run in this process (internal/access). This file mounts their routes under
// /principals/, /grants/, /sign-in/ and /permissions/,
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

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/permissions"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/access/signin"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// errAccessOff is every use of an access store on a server whose access
// stores are not open: one built by New and never given EnableAccess, which
// only a test does, since main refuses to start without them.
var errAccessOff = errors.New("access is off: principal-store, grant-store, sign-in-store and permission-store are not open on this server (" +
	config.PostgresURLEnvironmentVariable + ")")

// principalDirectory is principal-store as this server reads and writes it:
// *principals.Store in production, a fake in tests. It answers a disabled
// principal like any other — a departed human's grants are dormant, not
// invalid — and principals.ErrNotFound for one it does not have, or for an
// identity no active binding names.
type principalDirectory interface {
	Get(ctx context.Context, id string, includeDisabledMemberships bool) (*principals.Principal, error)
	ListGroups(ctx context.Context, memberID string, includeDisabled bool) ([]*principals.Principal, error)
	ResolveIdentity(ctx context.Context, provider, tenantID, subject string) (*principals.IdentityBindingResolution, error)
	Create(ctx context.Context, input *principals.Principal) (*principals.Principal, error)
	Patch(ctx context.Context, id string, patch principals.Patch) (*principals.Principal, error)
	AddMember(ctx context.Context, groupID, memberID string) (created bool, err error)
	RemoveMember(ctx context.Context, groupID, memberID string) error
}

// permissionEvaluator is permission-store's verdict on one tool call;
// *permissions.Engine is the one in production. A failure is an error, never a
// verdict: no rule was consulted.
type permissionEvaluator interface {
	Evaluate(ctx context.Context, request permissions.EvaluateRequest) (permissions.EvaluateResponse, error)
}

// accessRouteHandlers is what answers the mounted routes.
type accessRouteHandlers struct {
	principals  *principals.Handlers
	grants      *grants.Handlers
	permissions *permissions.Handlers
	// signIn is nil when sign-in is not configured (signInConfiguration).
	signIn *signin.Handlers
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
	s.principalStore = stores.Principals
	s.grantStore = stores.Grants
	s.grantsPrincipalDirectory = grantsDirectory
	s.grantsResourceChecker = checker
	s.permissionEngine = stores.PermissionEngine

	principals := principals.NewHandlers(stores.Principals)
	handlers := &accessRouteHandlers{
		principals:         principals,
		grants:             grants.NewHandlers(stores.Grants, grantsDirectory, checker),
		permissions:        permissions.NewHandlers(stores.Permissions, stores.PermissionEngine),
		principalStoreRoot: access.IdentityBindingRoutesAtTheirOwnPaths(principals),
	}
	directoryConfig, handlersConfig, offReason := s.signInConfiguration()
	if offReason == "" {
		directory := signin.NewDirectory(access.SignInPrincipalDirectory(stores.Principals), directoryConfig)
		handlers.signIn = signin.NewHandlers(stores.SignIn, directory, handlersConfig)
	} else {
		handlers.signInOffReason = offReason
	}
	s.access.Store(handlers)
	s.referenceReadable.principals.Store(stores.Principals)
}

// signInConfiguration is sign-in-store's wiring from this server's settings,
// or why sign-in is off. The sign-in page and the cookie domain name a site,
// so neither has a default: without both, a link would point nowhere and a
// session cookie would belong to no domain.
func (s *Server) signInConfiguration() (signin.DirectoryConfig, signin.HandlersConfig, string) {
	if s.cfg.SignInPageURL == "" || s.cfg.SignInCookieDomain == "" {
		return signin.DirectoryConfig{}, signin.HandlersConfig{}, fmt.Sprintf(
			"sign-in is off: %s and %s must both be set", config.SignInPageURLEnvironmentVariable, config.SignInCookieDomainEnvironmentVariable)
	}
	return signin.DirectoryConfig{
			People:                signInPeople{server: s},
			MultichatURL:          s.cfg.MultichatURL,
			AuthStoreURL:          s.cfg.AuthStoreURL,
			AuthStoreToken:        s.cfg.AuthStoreToken,
			MultichatCredentialID: s.cfg.SignInMultichatCredentialID,
		}, signin.HandlersConfig{
			SignInPageURL: s.cfg.SignInPageURL,
			CookieDomain:  s.cfg.SignInCookieDomain,
			Discord: signin.DiscordConfig{
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
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "access_off", errAccessOff.Error())
		return nil, false
	}
	return handlers, true
}

// principalStoreRoute answers one principal-store route. A request that may
// change a principal forgets every cached principal afterwards, so a person
// disabled or demoted here is refused on their next request rather than after
// the cache's 30 seconds.
func (s *Server) principalStoreRoute(route func(*principals.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
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
	if _, identified := requestcaller.PrincipalIdentityOfRequest(r); identified {
		return false
	}
	if principalIdentityHeaderNameIn(r.Header) != "" {
		return false
	}
	presented := r.Header.Values(requestcaller.ServiceTokenHeader)
	return len(presented) == 1 && s.cfg.ServiceToken != "" &&
		subtle.ConstantTimeCompare([]byte(presented[0]), []byte(s.cfg.ServiceToken)) == 1
}

// grantStoreRoute answers one grant-store route as the caller: the verified
// principal, narrowed by grant-store's own rules, or the internal service,
// unrestricted, as grant-store's service token was. A request with neither
// reaches grant-store with no caller, and grant-store answers it 401.
func (s *Server) grantStoreRoute(route func(*grants.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers, ok := s.accessHandlersOrRefuse(w)
		if !ok {
			return
		}
		ctx := r.Context()
		if principalID, identified := requestcaller.PrincipalIdentityOfRequest(r); identified {
			ctx = grants.ContextWithCaller(ctx, grants.Caller{PrincipalID: principalID})
		} else if s.requestIsTheInternalServiceActingAsItself(r) {
			ctx = grants.ContextWithCaller(ctx, grants.Caller{InternalService: true})
		}
		route(handlers.grants, w, r.WithContext(ctx))
	}
}

// signInStoreRoute answers one sign-in-store browser route. They check the
// browser's own cookie or secret themselves.
func (s *Server) signInStoreRoute(route func(*signin.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers, ok := s.accessHandlersOrRefuse(w)
		if !ok {
			return
		}
		if handlers.signIn == nil {
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "sign_in_off", handlers.signInOffReason)
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
func (s *Server) signInStoreAdministratorRoute(route func(*signin.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.signInStoreRoute(func(handlers *signin.Handlers, w http.ResponseWriter, r *http.Request) {
		if principalID, identified := requestcaller.PrincipalIdentityOfRequest(r); identified {
			r = r.WithContext(signin.ContextWithAdministrator(r.Context(), principalID))
		}
		route(handlers, w, r)
	})
}

// permissionStoreRoute answers one permission-store route.
func (s *Server) permissionStoreRoute(route func(*permissions.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
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
	s.mux.HandleFunc("GET /principals/health", s.principalStoreRoute((*principals.Handlers).Health))
	s.mux.HandleFunc("GET /principals/kinds", s.principalStoreRoute((*principals.Handlers).Kinds))
	s.mux.HandleFunc("GET /principals/kind-definitions", s.principalStoreRoute((*principals.Handlers).KindDefinitions))
	s.mux.HandleFunc("GET /principals/availability-reasons", s.principalStoreRoute((*principals.Handlers).AvailabilityReasons))
	s.mux.HandleFunc("GET /principals/weekday-codes", s.principalStoreRoute((*principals.Handlers).WeekdayCodes))
	s.mux.HandleFunc("GET /principals/principals", s.principalStoreRoute((*principals.Handlers).ListPrincipals))
	s.mux.HandleFunc("POST /principals/principals", s.principalStoreRoute((*principals.Handlers).CreatePrincipal))
	s.mux.HandleFunc("POST /principals/contacts/resolve", s.principalStoreRoute((*principals.Handlers).ResolveContact))
	s.mux.HandleFunc("GET /principals/principals/{id}", s.principalStoreRoute((*principals.Handlers).GetPrincipal))
	s.mux.HandleFunc("PATCH /principals/principals/{id}", s.principalStoreRoute((*principals.Handlers).PatchPrincipal))
	s.mux.HandleFunc("POST /principals/principals/{id}/disable", s.principalStoreRoute((*principals.Handlers).DisablePrincipal))
	s.mux.HandleFunc("POST /principals/principals/{id}/enable", s.principalStoreRoute((*principals.Handlers).EnablePrincipal))
	s.mux.HandleFunc("GET /principals/principals/{id}/members", s.principalStoreRoute((*principals.Handlers).ListMembers))
	s.mux.HandleFunc("PUT /principals/principals/{group}/members/{member}", s.principalStoreRoute((*principals.Handlers).PutMember))
	s.mux.HandleFunc("DELETE /principals/principals/{group}/members/{member}", s.principalStoreRoute((*principals.Handlers).DeleteMember))
	s.mux.HandleFunc("GET /principals/principals/{id}/groups", s.principalStoreRoute((*principals.Handlers).ListGroups))
	s.mux.HandleFunc("GET /principals/principals/{id}/availability", s.principalStoreRoute((*principals.Handlers).Availability))
	s.mux.HandleFunc("GET /principals/principals/{id}/time-off", s.principalStoreRoute((*principals.Handlers).ListTimeOff))
	s.mux.HandleFunc("POST /principals/principals/{id}/time-off", s.principalStoreRoute((*principals.Handlers).AddTimeOff))
	s.mux.HandleFunc("DELETE /principals/principals/{id}/time-off/{timeOffID}", s.principalStoreRoute((*principals.Handlers).DeleteTimeOff))
	s.mux.HandleFunc("GET /principals/identity-providers", s.principalStoreRoute((*principals.Handlers).IdentityProviders))
	s.mux.HandleFunc("GET /principals/identity-bindings", s.principalStoreRoute((*principals.Handlers).ListIdentityBindings))
	s.mux.HandleFunc("GET /principals/identity-bindings/resolve", s.principalStoreRoute((*principals.Handlers).ResolveIdentity))
	s.mux.HandleFunc("GET /principals/identity-bindings/{id}", s.principalStoreRoute((*principals.Handlers).GetIdentityBinding))
	// Linking and unlinking go through the administration handlers, which
	// stamp linked_by and unlinked_by with the caller rather than the body,
	// and end the login sessions of an unlinked identity. Mounted straight,
	// these two would let an administrator record another as the linker and
	// leave an unlinked identity's sessions signed in.
	s.mux.HandleFunc("POST /principals/identity-bindings", s.handleLinkIdentityBinding)
	s.mux.HandleFunc("POST /principals/identity-bindings/{id}/unlink", s.handleUnlinkIdentityBinding)

	// grant-store.
	s.mux.HandleFunc("GET /grants/health", s.grantStoreRoute((*grants.Handlers).Health))
	s.mux.HandleFunc("GET /grants/relations", s.grantStoreRoute((*grants.Handlers).Relations))
	s.mux.HandleFunc("GET /grants/resource-types", s.grantStoreRoute((*grants.Handlers).ResourceTypes))
	s.mux.HandleFunc("GET /grants/grants", s.grantStoreRoute((*grants.Handlers).ListGrants))
	s.mux.HandleFunc("POST /grants/grants", s.grantStoreRoute((*grants.Handlers).CreateGrant))
	s.mux.HandleFunc("GET /grants/grants/{id}", s.grantStoreRoute((*grants.Handlers).GetGrant))
	s.mux.HandleFunc("POST /grants/grants/{id}/revoke", s.grantStoreRoute((*grants.Handlers).RevokeGrant))
	s.mux.HandleFunc("GET /grants/principals/{id}/effective", s.grantStoreRoute((*grants.Handlers).Effective))

	// sign-in-store.
	s.mux.HandleFunc("GET /sign-in/health", s.signInStoreRoute((*signin.Handlers).Health))
	s.mux.HandleFunc("POST /sign-in/link-requests", s.signInStoreRoute((*signin.Handlers).RequestLink))
	s.mux.HandleFunc("POST /sign-in/redeem", s.signInStoreRoute((*signin.Handlers).Redeem))
	s.mux.HandleFunc("GET /sign-in/session", s.signInStoreRoute((*signin.Handlers).CurrentSession))
	s.mux.HandleFunc("POST /sign-in/sign-out", s.signInStoreRoute((*signin.Handlers).SignOut))
	s.mux.HandleFunc("GET /sign-in/discord/start", s.signInStoreRoute((*signin.Handlers).StartDiscord))
	s.mux.HandleFunc("GET /sign-in/discord/callback", s.signInStoreRoute((*signin.Handlers).FinishDiscord))
	s.mux.HandleFunc("POST /sign-in/sign-in-links", s.signInStoreAdministratorRoute((*signin.Handlers).MakeLink))
	s.mux.HandleFunc("GET /sign-in/sign-in-links", s.signInStoreAdministratorRoute((*signin.Handlers).ListLinks))
	s.mux.HandleFunc("GET /sign-in/sessions", s.signInStoreAdministratorRoute((*signin.Handlers).ListSessions))
	s.mux.HandleFunc("POST /sign-in/sessions/{id}/revoke", s.signInStoreAdministratorRoute((*signin.Handlers).RevokeSession))
	s.mux.HandleFunc("POST /sign-in/principals/{id}/discord-messages", s.signInStoreAdministratorRoute((*signin.Handlers).SendDiscordMessage))

	// permission-store.
	s.mux.HandleFunc("GET /permissions/healthz", s.permissionStoreRoute((*permissions.Handlers).Health))
	s.mux.HandleFunc("GET /permissions/rules", s.permissionStoreRoute((*permissions.Handlers).ListRules))
	s.mux.HandleFunc("POST /permissions/rules", s.permissionStoreRoute((*permissions.Handlers).CreateRule))
	s.mux.HandleFunc("GET /permissions/rules/{id}", s.permissionStoreRoute((*permissions.Handlers).GetRule))
	s.mux.HandleFunc("PATCH /permissions/rules/{id}", s.permissionStoreRoute((*permissions.Handlers).UpdateRule))
	s.mux.HandleFunc("DELETE /permissions/rules/{id}", s.permissionStoreRoute((*permissions.Handlers).DeleteRule))
	s.mux.HandleFunc("POST /permissions/evaluate", s.permissionStoreRoute((*permissions.Handlers).Evaluate))
	s.mux.HandleFunc("GET /permissions/audit", s.permissionStoreRoute((*permissions.Handlers).ListAudit))
}

// writePrincipalStoreCallError answers a call into principal-store that did not
// reach its handler: 503 when access is off, 500 otherwise.
func writePrincipalStoreCallError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAccessOff) {
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "access_off", err.Error())
		return
	}
	httpjson.WriteJSONError(w, http.StatusInternalServerError, "principal_store_call_failed", err.Error())
}
