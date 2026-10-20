package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

const (
	testEntraTenantID      = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	testEntraOtherTenantID = "0b0b0b0b-1111-2222-3333-444444444444"
	testEntraClientID      = "6e6f7a1b-0000-4000-8000-00000000c1d0"
	testEntraClientSecret  = "client-secret-for-tests"
	// testEntraObjectIDOfFirst is bound to firstTestPrincipalID, and
	// testEntraObjectIDOfDisabled to the disabled principal; any other oid is
	// unprovisioned.
	testEntraObjectIDOfFirst    = "aaaaaaaa-1111-4111-8111-111111111111"
	testEntraObjectIDOfDisabled = "dddddddd-9999-4999-8999-999999999999"
	testEntraObjectIDUnbound    = "eeeeeeee-5555-4555-8555-555555555555"
	testEntraSigningKeyID       = "test-signing-key"
)

// fakeEntraProvider is an OpenID Connect provider shaped like Entra ID for
// one tenant: a discovery document, a JWKS holding one RSA key, and a token
// endpoint that redeems codes the test registered, checking the client
// secret and the PKCE verifier as Entra would.
type fakeEntraProvider struct {
	server     *httptest.Server
	signingKey *rsa.PrivateKey

	mutex       sync.Mutex
	codes       map[string]registeredAuthorizationCode
	redemptions int
}

type registeredAuthorizationCode struct {
	codeChallenge string
	idToken       string
}

func newFakeEntraProvider(t *testing.T) *fakeEntraProvider {
	t.Helper()
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeEntraProvider{signingKey: signingKey, codes: map[string]registeredAuthorizationCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+testEntraTenantID+"/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := provider.server.URL + "/" + testEntraTenantID
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                provider.issuer(),
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"jwks_uri":                              base + "/discovery/v2.0/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"pairwise"},
		})
	})
	mux.HandleFunc("GET /"+testEntraTenantID+"/discovery/v2.0/keys", func(w http.ResponseWriter, r *http.Request) {
		publicKey := provider.signingKey.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": testEntraSigningKeyID,
			"n": base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /"+testEntraTenantID+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		refuse := func(reason string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"` + reason + `"}`))
		}
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("client_id") != testEntraClientID ||
			r.PostForm.Get("client_secret") != testEntraClientSecret {
			refuse("client or grant type wrong")
			return
		}
		provider.mutex.Lock()
		registered, known := provider.codes[r.PostForm.Get("code")]
		delete(provider.codes, r.PostForm.Get("code"))
		provider.redemptions++
		provider.mutex.Unlock()
		if !known {
			refuse("unknown or used code")
			return
		}
		verifierHash := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(verifierHash[:]) != registered.codeChallenge {
			refuse("code_verifier does not match the code_challenge")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer", "access_token": "access-token", "expires_in": 3600, "id_token": registered.idToken,
		})
	})
	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

func (provider *fakeEntraProvider) issuer() string {
	return provider.server.URL + "/" + testEntraTenantID + "/v2.0"
}

// signIDToken makes an RS256 JWT of claims with key.
func signIDToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": testEntraSigningKeyID})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// entraTestHarness is a gated server with Entra sign-in pointed at a fake
// provider, and a principal-store that binds two Entra identities.
type entraTestHarness struct {
	gated    *gatedTestServer
	provider *fakeEntraProvider
}

func newEntraTestHarness(t *testing.T) *entraTestHarness {
	t.Helper()
	provider := newFakeEntraProvider(t)
	gated := newGatedTestServerConfigured(t, nil, func(cfg *config.Config) {
		cfg.EntraTenantID = testEntraTenantID
		cfg.EntraClientID = testEntraClientID
		cfg.EntraClientSecret = testEntraClientSecret
		cfg.EntraRedirectURL = "https://bridge.example.test/auth/entra/callback"
		cfg.EntraAuthorityURL = provider.server.URL
	})
	bindings := map[string]string{
		testEntraObjectIDOfFirst:    `{"binding":{"id":"identity_binding_000001","principal_id":"principal_000001","provider":"entra","tenant_id":"` + testEntraTenantID + `","subject":"` + testEntraObjectIDOfFirst + `","linked_at":1,"linked_by":"principal_000003"},"principal":{"id":"principal_000001","kind":"human","acts_as_caller":true,"restricted_to_its_grants":false,"disabled_at":0,"is_administrator":false}}`,
		testEntraObjectIDOfDisabled: `{"binding":{"id":"identity_binding_000002","principal_id":"principal_000009","provider":"entra","tenant_id":"` + testEntraTenantID + `","subject":"` + testEntraObjectIDOfDisabled + `","linked_at":1,"linked_by":"principal_000003"},"principal":{"id":"principal_000009","kind":"human","acts_as_caller":true,"restricted_to_its_grants":false,"disabled_at":1788980427,"is_administrator":true}}`,
	}
	gated.principals.setIdentityBindings(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/identity-bindings/resolve" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		query := r.URL.Query()
		answer, bound := bindings[query.Get("subject")]
		if !bound || query.Get("provider") != "entra" || query.Get("tenant_id") != testEntraTenantID {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: no active binding for that identity"}`))
			return
		}
		_, _ = w.Write([]byte(answer))
	})
	return &entraTestHarness{gated: gated, provider: provider}
}

// startedSignIn is what GET /auth/entra/start handed the browser.
type startedSignIn struct {
	authorizeURL   *url.URL
	state          string
	nonce          string
	codeChallenge  string
	browserBinding *http.Cookie
}

func (harness *entraTestHarness) start(t *testing.T, returnTo string) startedSignIn {
	t.Helper()
	response := serve(harness.gated.server, httptest.NewRequest("GET", "/auth/entra/start?return_to="+url.QueryEscape(returnTo), nil))
	if response.Code != http.StatusFound {
		t.Fatalf("start = %d %s, want 302", response.Code, response.Body.String())
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	started := startedSignIn{authorizeURL: location}
	query := location.Query()
	started.state, started.nonce, started.codeChallenge = query.Get("state"), query.Get("nonce"), query.Get("code_challenge")
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == entraBrowserBindingCookieName {
			started.browserBinding = cookie
		}
	}
	if started.browserBinding == nil {
		t.Fatalf("start set no %s cookie", entraBrowserBindingCookieName)
	}
	return started
}

// standardClaims are the claims Entra would put in an ID token for this sign
// in, by the person with objectID.
func (harness *entraTestHarness) standardClaims(started startedSignIn, objectID string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": harness.provider.issuer(), "aud": testEntraClientID, "sub": "pairwise-subject-not-to-be-used",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nbf": now.Unix(),
		"nonce": started.nonce, "tid": testEntraTenantID, "oid": objectID,
		"preferred_username": "someone@contoso.example", "name": "Some One", "email": "someone@contoso.example",
	}
}

// callback registers a code that redeems to an ID token of claims signed with
// key, then sends the browser back with it.
func (harness *entraTestHarness) callback(t *testing.T, started startedSignIn, claims map[string]any, key *rsa.PrivateKey) *httptest.ResponseRecorder {
	t.Helper()
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	harness.provider.mutex.Lock()
	harness.provider.codes[code] = registeredAuthorizationCode{codeChallenge: started.codeChallenge, idToken: signIDToken(t, key, claims)}
	harness.provider.mutex.Unlock()
	return harness.callbackWithCode(started, code)
}

func (harness *entraTestHarness) callbackWithCode(started startedSignIn, code string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("GET", "/auth/entra/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(started.state), nil)
	request.AddCookie(started.browserBinding)
	return serve(harness.gated.server, request)
}

func (harness *entraTestHarness) onlyAttempt(t *testing.T) store.LoginAttempt {
	t.Helper()
	attempts, err := harness.gated.store.ListLoginAttempts("", 10)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, %v; want exactly one", attempts, err)
	}
	return attempts[0]
}

func loginCookieOrNil(recorder *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == loginSessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	return nil
}

func TestEntraSignInStartsWithPKCEAndAHashedState(t *testing.T) {
	harness := newEntraTestHarness(t)
	started := harness.start(t, "/chat")
	query := started.authorizeURL.Query()
	if started.authorizeURL.Path != "/"+testEntraTenantID+"/oauth2/v2.0/authorize" ||
		query.Get("response_type") != "code" || query.Get("client_id") != testEntraClientID ||
		query.Get("code_challenge_method") != "S256" || started.codeChallenge == "" ||
		query.Get("scope") != "openid profile" || query.Get("redirect_uri") != "https://bridge.example.test/auth/entra/callback" ||
		len(started.state) < 43 || len(started.nonce) < 43 {
		t.Fatalf("authorize URL %s is not an authorization code request with PKCE, state and nonce", started.authorizeURL)
	}
	attempt := harness.onlyAttempt(t)
	if attempt.StateHash != sha256Hex(started.state) || attempt.NonceHash != sha256Hex(started.nonce) || attempt.ReturnTo != "/chat" ||
		attempt.Outcome != "" || attempt.ExpiresAt.Sub(attempt.CreatedAt) != loginAttemptLifetime {
		t.Fatalf("attempt %+v", attempt)
	}
	if strings.Contains(attempt.StateHash, started.state) || attempt.CodeVerifier == "" {
		t.Fatalf("attempt keeps the state itself or no verifier: %+v", attempt)
	}
}

func TestEntraSignInWithABoundIdentityCreatesALoginSessionAndRedirects(t *testing.T) {
	harness := newEntraTestHarness(t)
	started := harness.start(t, "/chat?board=7#top")
	response := harness.callback(t, started, harness.standardClaims(started, testEntraObjectIDOfFirst), harness.provider.signingKey)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/chat?board=7#top" {
		t.Fatalf("callback = %d Location %q %s, want 302 to /chat?board=7#top", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	cookie := loginCookieOrNil(response)
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("callback set no HttpOnly, Lax login cookie: %+v", response.Result().Cookies())
	}
	session, err := harness.gated.store.GetLoginSessionByTokenHash(sha256Hex(cookie.Value))
	if err != nil || session.Method != store.LoginSessionMethodEntra || session.PrincipalID != firstTestPrincipalID ||
		session.IdentityBindingID != "identity_binding_000001" {
		t.Fatalf("login session %+v, %v", session, err)
	}
	attempt := harness.onlyAttempt(t)
	if attempt.Outcome != store.LoginAttemptOutcomeSignedIn || attempt.LoginSessionID != session.ID || attempt.PrincipalID != firstTestPrincipalID ||
		attempt.Subject != testEntraObjectIDOfFirst || attempt.TenantID != testEntraTenantID || attempt.CodeVerifier != "" || attempt.ConsumedAt == nil {
		t.Fatalf("attempt %+v", attempt)
	}

	who := httptest.NewRequest("GET", "/auth/principal", nil)
	who.AddCookie(cookie)
	answer := serve(harness.gated.server, who)
	if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `"method":"entra"`) || !strings.Contains(answer.Body.String(), `"csrf_token":"`) {
		t.Fatalf("GET /auth/principal = %d %s", answer.Code, answer.Body.String())
	}

	// The same callback again: the state is spent.
	replay := harness.callbackWithCode(started, "any-code")
	if replay.Code != http.StatusBadRequest || !strings.Contains(replay.Body.String(), "login_attempt_already_used") || loginCookieOrNil(replay) != nil {
		t.Fatalf("replayed callback = %d %s, want 400 login_attempt_already_used and no cookie", replay.Code, replay.Body.String())
	}
	if harness.provider.redemptions != 1 {
		t.Fatalf("the provider was asked to redeem %d codes, want 1: a replay must stop before the token endpoint", harness.provider.redemptions)
	}
}

func TestEntraSignInRefusesATokenThatDoesNotVerify(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, testCase := range map[string]struct {
		mutate  func(claims map[string]any)
		key     func(harness *entraTestHarness) *rsa.PrivateKey
		outcome string
	}{
		"wrong tenant":   {func(c map[string]any) { c["tid"] = testEntraOtherTenantID }, nil, loginOutcomeTenantMismatch},
		"wrong audience": {func(c map[string]any) { c["aud"] = "someone-elses-app" }, nil, loginOutcomeIDTokenInvalid},
		"wrong issuer": {func(c map[string]any) {
			c["iss"] = "https://login.microsoftonline.com/" + testEntraOtherTenantID + "/v2.0"
		}, nil, loginOutcomeIDTokenInvalid},
		"bad signature":   {nil, func(*entraTestHarness) *rsa.PrivateKey { return otherKey }, loginOutcomeIDTokenInvalid},
		"nonce mismatch":  {func(c map[string]any) { c["nonce"] = "a-nonce-from-another-sign-in" }, nil, loginOutcomeNonceMismatch},
		"expired":         {func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, nil, loginOutcomeIDTokenExpired},
		"issued later":    {func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() }, nil, loginOutcomeIDTokenTimeInvalid},
		"no object id":    {func(c map[string]any) { delete(c, "oid") }, nil, loginOutcomeObjectIDMissing},
		"object id a sub": {func(c map[string]any) { c["oid"] = "pairwise-subject-not-to-be-used" }, nil, loginOutcomeObjectIDMissing},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newEntraTestHarness(t)
			started := harness.start(t, "/chat")
			claims := harness.standardClaims(started, testEntraObjectIDOfFirst)
			if testCase.mutate != nil {
				testCase.mutate(claims)
			}
			key := harness.provider.signingKey
			if testCase.key != nil {
				key = testCase.key(harness)
			}
			response := harness.callback(t, started, claims, key)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), testCase.outcome) {
				t.Fatalf("callback = %d %s, want 401 %s", response.Code, response.Body.String(), testCase.outcome)
			}
			if loginCookieOrNil(response) != nil {
				t.Fatalf("a refused sign-in set a login cookie")
			}
			if attempt := harness.onlyAttempt(t); attempt.Outcome != testCase.outcome || attempt.LoginSessionID != "" {
				t.Fatalf("attempt %+v, want outcome %s and no session", attempt, testCase.outcome)
			}
			if sessions, _ := harness.gated.store.ListLoginSessionsOfPrincipal(firstTestPrincipalID); len(sessions) != 0 {
				t.Fatalf("a refused sign-in created login sessions: %+v", sessions)
			}
		})
	}
}

func TestEntraSignInOfAnUnprovisionedIdentityIsRecordedForAnAdministrator(t *testing.T) {
	harness := newEntraTestHarness(t)
	started := harness.start(t, "/chat?board=7")
	claims := harness.standardClaims(started, testEntraObjectIDUnbound)
	// The email of a provisioned person, which must not sign anyone in.
	claims["email"] = "one@contoso.example"
	claims["preferred_username"] = "one@contoso.example"
	response := harness.callback(t, started, claims, harness.provider.signingKey)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/chat?board=7&login_error=identity_not_provisioned" {
		t.Fatalf("callback = %d Location %q %s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if loginCookieOrNil(response) != nil {
		t.Fatalf("an unprovisioned identity got a login cookie")
	}
	attempt := harness.onlyAttempt(t)
	if attempt.Outcome != loginOutcomeIdentityNotProvisioned || attempt.TenantID != testEntraTenantID || attempt.Subject != testEntraObjectIDUnbound ||
		attempt.PreferredUsername != "one@contoso.example" || attempt.DisplayName != "Some One" || attempt.PrincipalID != "" {
		t.Fatalf("attempt %+v", attempt)
	}

	// principal-store was asked about tid and oid, and nothing else.
	resolves := harness.gated.principals.recordedIdentityBindingRequests()
	if len(resolves) != 1 {
		t.Fatalf("principal-store got %d identity requests, want 1", len(resolves))
	}
	asked, _ := url.ParseQuery(resolves[0].RawQuery)
	if len(asked) != 3 || asked.Get("provider") != "entra" || asked.Get("tenant_id") != testEntraTenantID || asked.Get("subject") != testEntraObjectIDUnbound {
		t.Fatalf("resolve asked %v, want provider, tenant_id and the oid only", asked)
	}
	if strings.Contains(resolves[0].RawQuery, "contoso.example") || strings.Contains(resolves[0].RawQuery, "pairwise") {
		t.Fatalf("resolve carried the email or sub: %s", resolves[0].RawQuery)
	}

	administratorCookie := harness.gated.loginAs(t, administratorTestPrincipalID)
	listed := harness.gated.requestAs(t, administratorCookie, "GET", "/auth/login-attempts?outcome=identity_not_provisioned", nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), testEntraObjectIDUnbound) {
		t.Fatalf("administrator's list = %d %s", listed.Code, listed.Body.String())
	}
	for _, secret := range []string{"state", "nonce", "verifier", attempt.StateHash, started.state} {
		if strings.Contains(listed.Body.String(), `"`+secret) || strings.Contains(listed.Body.String(), secret+`"`) {
			t.Errorf("the attempt list exposes %q: %s", secret, listed.Body.String())
		}
	}
	ordinaryCookie := harness.gated.loginAs(t, secondTestPrincipalID)
	if refused := harness.gated.requestAs(t, ordinaryCookie, "GET", "/auth/login-attempts", nil); refused.Code != http.StatusForbidden {
		t.Fatalf("a non-administrator listing attempts = %d, want 403", refused.Code)
	}
}

func TestEntraSignInOfADisabledPrincipalIsRefused(t *testing.T) {
	harness := newEntraTestHarness(t)
	started := harness.start(t, "/chat")
	response := harness.callback(t, started, harness.standardClaims(started, testEntraObjectIDOfDisabled), harness.provider.signingKey)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/chat?login_error=principal_disabled" || loginCookieOrNil(response) != nil {
		t.Fatalf("callback = %d Location %q cookies %v", response.Code, response.Header().Get("Location"), response.Result().Cookies())
	}
	if attempt := harness.onlyAttempt(t); attempt.Outcome != loginOutcomePrincipalDisabled || attempt.PrincipalID != disabledTestPrincipalID ||
		attempt.IdentityBindingID != "identity_binding_000002" {
		t.Fatalf("attempt %+v", attempt)
	}
}

func TestEntraSignInRefusesAReturnToOffThisSite(t *testing.T) {
	harness := newEntraTestHarness(t)
	for _, returnTo := range []string{"", "//evil.com", "//evil.com/chat", "https://evil.com/", "/\\evil.com", "chat", "javascript:alert(1)", "/chat\r\nSet-Cookie: x=y"} {
		response := serve(harness.gated.server, httptest.NewRequest("GET", "/auth/entra/start?return_to="+url.QueryEscape(returnTo), nil))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_return_to") {
			t.Errorf("return_to %q = %d %s, want 400 invalid_return_to", returnTo, response.Code, response.Body.String())
		}
	}
	if attempts, _ := harness.gated.store.ListLoginAttempts("", 10); len(attempts) != 0 {
		t.Fatalf("a refused start recorded %d attempts", len(attempts))
	}
}

func TestEntraCallbackFromAnotherBrowserIsRefused(t *testing.T) {
	harness := newEntraTestHarness(t)
	started := harness.start(t, "/chat")
	started.browserBinding = &http.Cookie{Name: entraBrowserBindingCookieName, Value: "a-victims-browser-has-a-different-value"}
	response := harness.callback(t, started, harness.standardClaims(started, testEntraObjectIDOfFirst), harness.provider.signingKey)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), loginOutcomeBrowserBindingMismatch) || loginCookieOrNil(response) != nil {
		t.Fatalf("callback from another browser = %d %s", response.Code, response.Body.String())
	}
	if harness.provider.redemptions != 0 {
		t.Fatalf("the code was redeemed for a callback from another browser")
	}
}

func TestEntraCallbackWithAnUnknownStateIsRefused(t *testing.T) {
	harness := newEntraTestHarness(t)
	response := serve(harness.gated.server, httptest.NewRequest("GET", "/auth/entra/callback?code=x&state=never-issued", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "login_attempt_unknown") {
		t.Fatalf("unknown state = %d %s", response.Code, response.Body.String())
	}
}

func TestEntraSignInIsNotFoundWhenNotConfigured(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for _, path := range []string{"/auth/entra/start?return_to=/chat", "/auth/entra/callback?state=x&code=y"} {
		if response := serve(gated.server, httptest.NewRequest("GET", path, nil)); response.Code != http.StatusNotFound ||
			!strings.Contains(response.Body.String(), "entra_sign_in_not_configured") {
			t.Errorf("%s = %d %s, want 404 entra_sign_in_not_configured", path, response.Code, response.Body.String())
		}
	}
}

func TestReturnToWithLoginErrorKeepsQueryAndFragment(t *testing.T) {
	for returnTo, want := range map[string]string{
		"/chat":         "/chat?login_error=x",
		"/chat?a=1":     "/chat?a=1&login_error=x",
		"/chat?":        "/chat?login_error=x",
		"/chat#part":    "/chat?login_error=x#part",
		"/chat?a=1#p?q": "/chat?a=1&login_error=x#p?q",
	} {
		if got := returnToWithLoginError(returnTo, "x"); got != want {
			t.Errorf("returnToWithLoginError(%q) = %q, want %q", returnTo, got, want)
		}
	}
}
