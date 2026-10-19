package server

// Entra ID sign-in: OpenID Connect authorization code flow with PKCE.
//
// GET /auth/entra/start records a login attempt (state, nonce, PKCE verifier,
// where to go afterwards) and sends the browser to Entra ID. Entra sends it
// back to GET /auth/entra/callback with a code, which this server redeems at
// the token endpoint with its client secret and the verifier. The ID token
// that comes back is checked against the tenant's signing keys, the exact
// issuer <authority>/<tenant>/v2.0, this app's client id as audience, its
// time claims, the nonce of this attempt and the tid claim. The person is
// then identified by tid and oid — never sub, which Entra makes different per
// application, and never an email or name, which anyone can register in
// another tenant — and principal-store's identity bindings say which
// principal that is. Nothing on this path creates a principal or a binding:
// an identity nobody has linked is refused, and its attempt row tells an
// administrator what to link.
//
// Every attempt that reaches the callback ends with an outcome on its row:
// signed_in, or the code of the refusal.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// entraIdentityProvider is principal-store's name for Entra ID in a binding.
const entraIdentityProvider = "entra"

// loginAttemptLifetime is how long a person has between being sent to Entra
// and coming back.
const loginAttemptLifetime = 10 * time.Minute

// idTokenClockSkew is how far an ID token's iat or nbf may sit in this
// server's future, for clocks that disagree a little.
const idTokenClockSkew = 2 * time.Minute

// entraBrowserBindingCookieName holds a random value whose hash is on the
// login attempt. The callback must come from the browser that started the
// attempt: without this, someone could start a sign-in as themselves, stop
// at the callback, and send that link to a victim, who would then be signed
// in as the sender.
const entraBrowserBindingCookieName = "llm_bridge_entra_sign_in"

// Outcomes recorded on a login attempt. LoginAttemptOutcomeSignedIn is the
// one success.
const (
	loginOutcomeAttemptExpired         = "login_attempt_expired"
	loginOutcomeBrowserBindingMismatch = "browser_binding_mismatch"
	loginOutcomeProviderError          = "provider_error"
	loginOutcomeCodeMissing            = "code_missing"
	loginOutcomeTokenExchangeFailed    = "token_exchange_failed"
	loginOutcomeIDTokenMissing         = "id_token_missing"
	loginOutcomeIDTokenExpired         = "id_token_expired"
	loginOutcomeIDTokenInvalid         = "id_token_invalid"
	loginOutcomeNonceMismatch          = "nonce_mismatch"
	loginOutcomeTenantMismatch         = "tenant_mismatch"
	loginOutcomeIDTokenTimeInvalid     = "id_token_time_invalid"
	loginOutcomeObjectIDMissing        = "object_id_missing"
	loginOutcomeIdentityNotProvisioned = "identity_not_provisioned"
	loginOutcomePrincipalStoreFailed   = "principal_store_unavailable"
	loginOutcomePrincipalNotHuman      = "principal_not_human"
	loginOutcomePrincipalDisabled      = "principal_disabled"
	loginOutcomeLoginSessionFailed     = "login_session_not_recorded"
)

// entraSignIn holds the Entra settings and the provider's discovery document,
// read the first time someone signs in rather than at startup, so this server
// starts while Microsoft is unreachable.
type entraSignIn struct {
	tenantID     string
	clientID     string
	clientSecret string
	redirectURL  string
	issuerURL    string
	httpClient   *http.Client

	mutex    sync.Mutex
	provider *oidc.Provider
}

func newEntraSignIn(cfg *config.Config) *entraSignIn {
	return &entraSignIn{
		tenantID:     strings.ToLower(cfg.EntraTenantID),
		clientID:     cfg.EntraClientID,
		clientSecret: cfg.EntraClientSecret,
		redirectURL:  cfg.EntraRedirectURL,
		issuerURL:    cfg.EntraIssuerURL(),
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}
}

// providerContext carries the HTTP client go-oidc and oauth2 make their
// calls with.
func (entra *entraSignIn) providerContext(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, entra.httpClient)
}

// discoveredProvider reads the issuer's discovery document once and keeps it.
// go-oidc refuses a document whose issuer is not exactly issuerURL. A failed
// read is not kept, so the next sign-in asks again.
func (entra *entraSignIn) discoveredProvider(ctx context.Context) (*oidc.Provider, error) {
	entra.mutex.Lock()
	defer entra.mutex.Unlock()
	if entra.provider != nil {
		return entra.provider, nil
	}
	provider, err := oidc.NewProvider(entra.providerContext(ctx), entra.issuerURL)
	if err != nil {
		return nil, fmt.Errorf("read the discovery document of %s: %w", entra.issuerURL, err)
	}
	entra.provider = provider
	return provider, nil
}

func (entra *entraSignIn) oauth2Config(provider *oidc.Provider) *oauth2.Config {
	endpoint := provider.Endpoint()
	// Entra takes the client secret as a form field; saying so stops the
	// oauth2 package trying the Authorization header first and retrying.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:     entra.clientID,
		ClientSecret: entra.clientSecret,
		Endpoint:     endpoint,
		RedirectURL:  entra.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}
}

// validReturnToPath accepts only a path on this site: one leading "/", not
// "//" (a browser reads that as another host), no backslash (some browsers
// read "/\" as "//"), no control characters, and nothing url.Parse reads as a
// scheme or host.
func validReturnToPath(returnTo string) error {
	if returnTo == "" {
		return errors.New("return_to is required: the path on this site to come back to after signing in, such as /chat")
	}
	if !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") {
		return fmt.Errorf("return_to %q must be a path beginning with a single /", returnTo)
	}
	for _, character := range returnTo {
		if character == '\\' || character < 0x20 || character == 0x7f {
			return fmt.Errorf("return_to %q holds a backslash or a control character", returnTo)
		}
	}
	parsed, err := url.Parse(returnTo)
	if err != nil {
		return fmt.Errorf("return_to %q does not parse: %w", returnTo, err)
	}
	if parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil {
		return fmt.Errorf("return_to %q names a scheme or host; it must be a path on this site", returnTo)
	}
	return nil
}

// returnToWithLoginError adds login_error=<code> to a return_to path that
// passed validReturnToPath, after any query it already has and before any
// fragment.
func returnToWithLoginError(returnTo, code string) string {
	path, fragment, hasFragment := strings.Cut(returnTo, "#")
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
		if strings.HasSuffix(path, "?") || strings.HasSuffix(path, "&") {
			separator = ""
		}
	}
	result := path + separator + "login_error=" + url.QueryEscape(code)
	if hasFragment {
		result += "#" + fragment
	}
	return result
}

// handleEntraSignInStart records an attempt and sends the browser to Entra ID.
func (s *Server) handleEntraSignInStart(w http.ResponseWriter, r *http.Request) {
	entra := s.entraSignIn
	if entra == nil {
		writeJSONError(w, http.StatusNotFound, "entra_sign_in_not_configured",
			"Entra ID sign-in is not configured on this server (LLMBRIDGE_ENTRA_TENANT_ID, _CLIENT_ID, _CLIENT_SECRET, _REDIRECT_URL)")
		return
	}
	returnTo := r.URL.Query().Get("return_to")
	if err := validReturnToPath(returnTo); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_return_to", err.Error())
		return
	}
	provider, err := entra.discoveredProvider(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "entra_discovery_failed", err.Error())
		return
	}
	state, stateErr := randomURLSafeToken(loginSessionRandomBytes)
	nonce, nonceErr := randomURLSafeToken(loginSessionRandomBytes)
	browserBinding, bindingErr := randomURLSafeToken(loginSessionRandomBytes)
	if err := errors.Join(stateErr, nonceErr, bindingErr); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	verifier := oauth2.GenerateVerifier()
	now := s.loginClock().UTC().Truncate(time.Second)
	attempt := store.LoginAttempt{
		Provider:           entraIdentityProvider,
		StateHash:          sha256Hex(state),
		NonceHash:          sha256Hex(nonce),
		CodeVerifier:       verifier,
		BrowserBindingHash: sha256Hex(browserBinding),
		ReturnTo:           returnTo,
		CreatedAt:          now,
		ExpiresAt:          now.Add(loginAttemptLifetime),
		UserAgent:          r.UserAgent(),
		RemoteAddress:      r.RemoteAddr,
	}
	if err := s.store.InsertLoginAttempt(&attempt); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login_attempt_not_recorded", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     entraBrowserBindingCookieName,
		Value:    browserBinding,
		Path:     "/",
		MaxAge:   int(loginAttemptLifetime / time.Second),
		HttpOnly: true,
		Secure:   requestArrivedOverHTTPS(r),
		// Lax, not Strict: the callback is a top-level navigation from
		// Entra's site, and Strict would leave the cookie behind.
		SameSite: http.SameSiteLaxMode,
	})
	authorizationURL := entra.oauth2Config(provider).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authorizationURL, http.StatusFound)
}

// entraIDTokenClaims is the part of an Entra ID token this server reads
// beyond what go-oidc checks. Email is deliberately absent: it identifies
// nobody.
type entraIDTokenClaims struct {
	TenantID          string   `json:"tid"`
	ObjectID          string   `json:"oid"`
	PreferredUsername string   `json:"preferred_username"`
	Name              string   `json:"name"`
	IssuedAt          *float64 `json:"iat"`
	NotBefore         *float64 `json:"nbf"`
}

// handleEntraSignInCallback finishes a sign-in Entra ID sent back.
func (s *Server) handleEntraSignInCallback(w http.ResponseWriter, r *http.Request) {
	entra := s.entraSignIn
	if entra == nil {
		writeJSONError(w, http.StatusNotFound, "entra_sign_in_not_configured", "Entra ID sign-in is not configured on this server")
		return
	}
	query := r.URL.Query()
	state := query.Get("state")
	if state == "" {
		writeJSONError(w, http.StatusBadRequest, "state_missing", "the callback carries no state, so it belongs to no sign-in this server started")
		return
	}
	now := s.loginClock().UTC()
	attempt, err := s.store.ConsumeLoginAttemptByStateHash(sha256Hex(state), now)
	switch {
	case errors.Is(err, store.ErrLoginAttemptNotFound):
		writeJSONError(w, http.StatusBadRequest, "login_attempt_unknown", "the callback's state names no sign-in this server started")
		return
	case errors.Is(err, store.ErrLoginAttemptAlreadyConsumed):
		log.Printf("[login] refused a second callback for %s (state replayed); its outcome was %q", attempt.ID, attempt.Outcome)
		writeJSONError(w, http.StatusBadRequest, "login_attempt_already_used", fmt.Sprintf(
			"sign-in %s was already completed or refused; start again", attempt.ID))
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "login_attempt_unreadable", err.Error())
		return
	}
	// From here on the attempt is consumed, and every way out records how it
	// ended.
	clearEntraBrowserBindingCookie(w, r)
	end := store.LoginAttemptEnd{}
	refuse := func(status int, outcome, detail string) {
		end.Outcome, end.OutcomeDetail = outcome, detail
		if !s.recordLoginAttemptEnd(w, attempt.ID, end) {
			return
		}
		writeJSONError(w, status, outcome, detail)
	}
	refuseByRedirect := func(outcome, detail string) {
		end.Outcome, end.OutcomeDetail = outcome, detail
		if !s.recordLoginAttemptEnd(w, attempt.ID, end) {
			return
		}
		http.Redirect(w, r, returnToWithLoginError(attempt.ReturnTo, outcome), http.StatusFound)
	}

	if !now.Before(attempt.ExpiresAt) {
		refuse(http.StatusBadRequest, loginOutcomeAttemptExpired, fmt.Sprintf("sign-in %s expired at %s; start again", attempt.ID, attempt.ExpiresAt.Format(time.RFC3339)))
		return
	}
	bindingCookie, err := r.Cookie(entraBrowserBindingCookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(sha256Hex(bindingCookie.Value)), []byte(attempt.BrowserBindingHash)) != 1 {
		refuse(http.StatusBadRequest, loginOutcomeBrowserBindingMismatch,
			"the callback did not come from the browser that started this sign-in; start again from this browser")
		return
	}
	if providerError := query.Get("error"); providerError != "" {
		refuseByRedirect(loginOutcomeProviderError, fmt.Sprintf("Entra ID answered %s: %s", providerError, query.Get("error_description")))
		return
	}
	code := query.Get("code")
	if code == "" {
		refuse(http.StatusBadRequest, loginOutcomeCodeMissing, "the callback carries neither a code nor an error")
		return
	}

	provider, err := entra.discoveredProvider(r.Context())
	if err != nil {
		refuse(http.StatusBadGateway, loginOutcomeTokenExchangeFailed, err.Error())
		return
	}
	token, err := entra.oauth2Config(provider).Exchange(entra.providerContext(r.Context()), code, oauth2.VerifierOption(attempt.CodeVerifier))
	if err != nil {
		refuse(http.StatusBadGateway, loginOutcomeTokenExchangeFailed, fmt.Sprintf("redeem the authorization code: %v", err))
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		refuse(http.StatusBadGateway, loginOutcomeIDTokenMissing, "the token endpoint answered without an id_token")
		return
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID:             entra.clientID,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  s.loginClock,
	})
	idToken, err := verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		var expired *oidc.TokenExpiredError
		if errors.As(err, &expired) {
			refuse(http.StatusUnauthorized, loginOutcomeIDTokenExpired, err.Error())
			return
		}
		refuse(http.StatusUnauthorized, loginOutcomeIDTokenInvalid, err.Error())
		return
	}
	if subtle.ConstantTimeCompare([]byte(sha256Hex(idToken.Nonce)), []byte(attempt.NonceHash)) != 1 {
		refuse(http.StatusUnauthorized, loginOutcomeNonceMismatch, "the ID token's nonce is not the one this sign-in sent")
		return
	}
	var claims entraIDTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		refuse(http.StatusUnauthorized, loginOutcomeIDTokenInvalid, fmt.Sprintf("read the ID token's claims: %v", err))
		return
	}
	end.TenantID = strings.ToLower(claims.TenantID)
	end.Subject = strings.ToLower(claims.ObjectID)
	end.PreferredUsername = claims.PreferredUsername
	end.DisplayName = claims.Name
	if end.TenantID != entra.tenantID {
		refuse(http.StatusUnauthorized, loginOutcomeTenantMismatch, fmt.Sprintf(
			"the ID token is for tenant %q; this server signs in people from tenant %s only", claims.TenantID, entra.tenantID))
		return
	}
	latestAcceptable := now.Add(idTokenClockSkew)
	for name, claim := range map[string]*float64{"iat": claims.IssuedAt, "nbf": claims.NotBefore} {
		if claim == nil {
			if name == "iat" {
				refuse(http.StatusUnauthorized, loginOutcomeIDTokenTimeInvalid, "the ID token has no iat claim")
				return
			}
			continue
		}
		if moment := time.Unix(int64(*claim), 0); moment.After(latestAcceptable) {
			refuse(http.StatusUnauthorized, loginOutcomeIDTokenTimeInvalid, fmt.Sprintf(
				"the ID token's %s is %s, later than this server's clock allows", name, moment.UTC().Format(time.RFC3339)))
			return
		}
	}
	if !config.IsGloballyUniqueIdentifier(end.Subject) {
		refuse(http.StatusUnauthorized, loginOutcomeObjectIDMissing, fmt.Sprintf(
			"the ID token's oid %q is not a GUID; the person is identified by it and by nothing else", claims.ObjectID))
		return
	}

	resolution, err := s.principalClient.ResolveIdentityBinding(r.Context(), entraIdentityProvider, end.TenantID, end.Subject)
	if errors.Is(err, principalclient.ErrIdentityNotProvisioned) {
		refuseByRedirect(loginOutcomeIdentityNotProvisioned, fmt.Sprintf(
			"no principal is bound to Entra tenant %s object %s; an administrator links it with POST /admin/identity-bindings", end.TenantID, end.Subject))
		return
	}
	if err != nil {
		refuse(http.StatusBadGateway, loginOutcomePrincipalStoreFailed, err.Error())
		return
	}
	end.IdentityBindingID = resolution.Binding.ID
	end.PrincipalID = resolution.Principal.ID
	if resolution.Principal.Kind != "human" {
		refuseByRedirect(loginOutcomePrincipalNotHuman, fmt.Sprintf(
			"binding %s names %s, a %q principal; only a human signs in", resolution.Binding.ID, resolution.Principal.ID, resolution.Principal.Kind))
		return
	}
	if resolution.Principal.DisabledAt != 0 {
		refuseByRedirect(loginOutcomePrincipalDisabled, fmt.Sprintf(
			"%s was disabled in principal-store at %s", resolution.Principal.ID, time.Unix(resolution.Principal.DisabledAt, 0).UTC().Format(time.RFC3339)))
		return
	}

	session, sessionToken, err := s.recordLoginSession(r, resolution.Principal.ID, store.LoginSessionMethodEntra, resolution.Binding.ID)
	if err != nil {
		refuse(http.StatusInternalServerError, loginOutcomeLoginSessionFailed, err.Error())
		return
	}
	end.Outcome = store.LoginAttemptOutcomeSignedIn
	end.LoginSessionID = session.ID
	if _, err := s.store.FinishLoginAttempt(attempt.ID, end); err != nil {
		// The audit of this sign-in could not be written, so the session it
		// would stand for is ended before anyone holds its cookie.
		if _, revokeErr := s.store.RevokeLoginSession(session.ID, session.PrincipalID, "sign-in could not be recorded", s.loginClock()); revokeErr != nil {
			log.Printf("[login] ERROR: login session %s left live after its sign-in failed to record: %v", session.ID, revokeErr)
		}
		writeJSONError(w, http.StatusInternalServerError, "login_attempt_not_recorded", fmt.Sprintf(
			"sign-in %s could not be recorded, so no login was issued: %v", attempt.ID, err))
		return
	}
	setLoginSessionCookie(w, r, session, sessionToken)
	http.Redirect(w, r, attempt.ReturnTo, http.StatusFound)
}

// recordLoginAttemptEnd writes a refused attempt's outcome. When that fails
// the refusal still stands, and the answer says the record is missing rather
// than pretending it was kept.
func (s *Server) recordLoginAttemptEnd(w http.ResponseWriter, attemptID string, end store.LoginAttemptEnd) bool {
	if _, err := s.store.FinishLoginAttempt(attemptID, end); err != nil {
		log.Printf("[login] ERROR: sign-in %s refused as %s, and the refusal could not be recorded: %v", attemptID, end.Outcome, err)
		writeJSONError(w, http.StatusInternalServerError, "login_attempt_not_recorded", fmt.Sprintf(
			"sign-in %s was refused (%s: %s), and the refusal could not be recorded: %v", attemptID, end.Outcome, end.OutcomeDetail, err))
		return false
	}
	return true
}

func clearEntraBrowserBindingCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     entraBrowserBindingCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   requestArrivedOverHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// loginAttemptListDefaultLimit and loginAttemptListMaximumLimit bound GET
// /auth/login-attempts.
const (
	loginAttemptListDefaultLimit = 100
	loginAttemptListMaximumLimit = 1000
)

// handleListLoginAttempts lists sign-in attempts newest first, for an
// administrator: ?outcome=identity_not_provisioned gives the tid and oid of
// each person refused for want of a binding. The state, nonce and verifier
// are never in the answer.
func (s *Server) handleListLoginAttempts(w http.ResponseWriter, r *http.Request) {
	limit := loginAttemptListDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > loginAttemptListMaximumLimit {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit %q must be a whole number from 1 to %d", raw, loginAttemptListMaximumLimit))
			return
		}
		limit = parsed
	}
	attempts, err := s.store.ListLoginAttempts(r.URL.Query().Get("outcome"), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login_attempts_unreadable", err.Error())
		return
	}
	writeJSON(w, attempts)
}
