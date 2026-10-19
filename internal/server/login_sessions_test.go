package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func loginSessionsOfList(t *testing.T, recorder *httptest.ResponseRecorder) []loginSessionView {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	var views []loginSessionView
	if err := json.Unmarshal(recorder.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	return views
}

func loginSessionIDOfCookie(t *testing.T, gated *gatedTestServer, cookie *http.Cookie) string {
	t.Helper()
	session, err := gated.store.GetLoginSessionByTokenHash(sha256Hex(cookie.Value))
	if err != nil {
		t.Fatalf("no login session for the cookie: %v", err)
	}
	return session.ID
}

func TestTheDatabaseKeepsOnlyAHashOfTheLoginToken(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	login := demoLogin(t, gated.server, firstTestPrincipalID)
	cookie := loginCookieFrom(t, login)
	if len(cookie.Value) < 43 {
		t.Fatalf("cookie token %q is shorter than 32 random bytes encoded", cookie.Value)
	}
	session, err := gated.store.GetLoginSessionByTokenHash(sha256Hex(cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	if session.TokenHash == cookie.Value || strings.Contains(session.TokenHash, cookie.Value) {
		t.Fatalf("the stored token hash holds the token itself")
	}
	if session.Method != store.LoginSessionMethodDemo || session.PrincipalID != firstTestPrincipalID || session.IdentityBindingID != "" {
		t.Fatalf("stored %+v", session)
	}
	var body loggedInPrincipalResponse
	if err := json.Unmarshal(login.Body.Bytes(), &body); err != nil || body.CSRFToken == "" || body.CSRFToken != session.CSRFToken || body.LoginSessionID != session.ID {
		t.Fatalf("login body %s, want the session's id and CSRF token", login.Body.String())
	}
	if strings.Contains(login.Body.String(), session.TokenHash) {
		t.Fatalf("login body holds the token hash")
	}
}

func TestLogoutRevokesTheSessionOnTheServerSoACopiedCookieStopsWorking(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := gated.loginAs(t, firstTestPrincipalID)
	if response := gated.requestAs(t, cookie, "GET", "/sessions", nil); response.Code != http.StatusOK {
		t.Fatalf("before logout = %d %s", response.Code, response.Body.String())
	}

	withoutToken := httptest.NewRequest("POST", "/auth/logout", nil)
	withoutToken.AddCookie(cookie)
	if response := serve(gated.server, withoutToken); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "csrf_token_missing") {
		t.Fatalf("logout without the CSRF token = %d %s, want 403 csrf_token_missing", response.Code, response.Body.String())
	}

	logout := gated.requestAs(t, cookie, "POST", "/auth/logout", nil)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", logout.Code, logout.Body.String())
	}
	if cleared := clearedLoginCookieFrom(t, logout); cleared.MaxAge >= 0 {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}
	// The browser dropped the cookie; a copy taken before logout is now dead.
	after := gated.requestAs(t, cookie, "GET", "/sessions", nil)
	if after.Code != http.StatusUnauthorized || !strings.Contains(after.Body.String(), "login_session_revoked") {
		t.Fatalf("old cookie after logout = %d %s, want 401 login_session_revoked", after.Code, after.Body.String())
	}
	row, err := gated.store.GetLoginSession(loginSessionIDOfCookie(t, gated, cookie))
	if err != nil || row.RevokedAt == nil || row.RevokedBy != firstTestPrincipalID || row.RevokeReason != "logout" {
		t.Fatalf("row after logout = %+v, %v", row, err)
	}
}

func TestAPrincipalListsAndRevokesOnlyItsOwnLoginSessions(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	firstCookie := gated.loginAs(t, firstTestPrincipalID)
	firstOtherDevice := gated.loginAs(t, firstTestPrincipalID)
	secondCookie := gated.loginAs(t, secondTestPrincipalID)

	listed := loginSessionsOfList(t, gated.requestAs(t, firstCookie, "GET", "/auth/sessions", nil))
	if len(listed) != 2 {
		t.Fatalf("first lists %d sessions, want its own 2: %+v", len(listed), listed)
	}
	currentCount := 0
	for _, view := range listed {
		if view.PrincipalID != firstTestPrincipalID || view.State != "live" {
			t.Errorf("listed %+v", view)
		}
		if view.Current {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Errorf("%d sessions marked current, want 1", currentCount)
	}
	raw := gated.requestAs(t, firstCookie, "GET", "/auth/sessions", nil).Body.String()
	for _, secretField := range []string{"token_hash", "csrf_token", "TokenHash", "CSRFToken"} {
		if strings.Contains(raw, secretField) {
			t.Errorf("the list carries %s: %s", secretField, raw)
		}
	}

	if response := gated.requestAs(t, firstCookie, "GET", "/auth/sessions?principal_id="+secondTestPrincipalID, nil); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "not_administrator") {
		t.Fatalf("listing someone else's = %d %s, want 403 not_administrator", response.Code, response.Body.String())
	}

	secondSessionID := loginSessionIDOfCookie(t, gated, secondCookie)
	refused := gated.requestAs(t, firstCookie, "POST", "/auth/sessions/"+secondSessionID+"/revoke", map[string]string{"reason": "mine now"})
	if refused.Code != http.StatusNotFound {
		t.Fatalf("revoking someone else's = %d %s, want 404", refused.Code, refused.Body.String())
	}
	if response := gated.requestAs(t, secondCookie, "GET", "/sessions", nil); response.Code != http.StatusOK {
		t.Fatalf("the other principal's session was ended by a refused revoke: %d", response.Code)
	}

	otherDeviceID := loginSessionIDOfCookie(t, gated, firstOtherDevice)
	if response := gated.requestAs(t, firstCookie, "POST", "/auth/sessions/"+otherDeviceID+"/revoke", map[string]string{}); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "reason_required") {
		t.Fatalf("revoke without a reason = %d %s, want 400 reason_required", response.Code, response.Body.String())
	}
	revoked := gated.requestAs(t, firstCookie, "POST", "/auth/sessions/"+otherDeviceID+"/revoke", map[string]string{"reason": "lost phone"})
	if revoked.Code != http.StatusOK || !strings.Contains(revoked.Body.String(), `"state":"revoked"`) {
		t.Fatalf("revoking its own other session = %d %s", revoked.Code, revoked.Body.String())
	}
	if response := gated.requestAs(t, firstOtherDevice, "GET", "/sessions", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("the revoked device still works: %d", response.Code)
	}
	again := gated.requestAs(t, firstCookie, "POST", "/auth/sessions/"+otherDeviceID+"/revoke", map[string]string{"reason": "again"})
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), "login_session_already_revoked") {
		t.Fatalf("second revoke = %d %s, want 409", again.Code, again.Body.String())
	}
}

func TestAnAdministratorListsAndRevokesAnyonesLoginSessions(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	victimCookie := gated.loginAs(t, firstTestPrincipalID)

	listed := loginSessionsOfList(t, gated.requestAs(t, administratorCookie, "GET", "/auth/sessions?principal_id="+firstTestPrincipalID, nil))
	if len(listed) != 1 || listed[0].PrincipalID != firstTestPrincipalID || listed[0].Current {
		t.Fatalf("administrator listed %+v, want the one session of %s, not marked current", listed, firstTestPrincipalID)
	}
	revoked := gated.requestAs(t, administratorCookie, "POST", "/auth/sessions/"+listed[0].ID+"/revoke", map[string]string{"reason": "left the company"})
	if revoked.Code != http.StatusOK {
		t.Fatalf("administrator revoke = %d %s", revoked.Code, revoked.Body.String())
	}
	row, _ := gated.store.GetLoginSession(listed[0].ID)
	if row.RevokedBy != administratorTestPrincipalID || row.RevokeReason != "left the company" {
		t.Fatalf("row %+v, want revoked by the administrator with the reason", row)
	}
	if response := gated.requestAs(t, victimCookie, "GET", "/sessions", nil); response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "login_session_revoked") {
		t.Fatalf("revoked session = %d %s, want 401 login_session_revoked", response.Code, response.Body.String())
	}

	// The service token on its own may list anyone's, but must say whose,
	// and may not revoke as nobody.
	if response := gated.requestAsService(t, "GET", "/auth/sessions"); response.Code != http.StatusBadRequest {
		t.Fatalf("service token listing nobody's = %d, want 400", response.Code)
	}
	if response := gated.requestAsService(t, "GET", "/auth/sessions?principal_id="+firstTestPrincipalID); response.Code != http.StatusOK {
		t.Fatalf("service token listing = %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest("POST", "/auth/sessions/"+listed[0].ID+"/revoke", strings.NewReader(`{"reason":"x"}`))
	request.Header.Set(serviceTokenHeader, testServiceToken)
	if response := serve(gated.server, request); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "revoke_requires_a_principal") {
		t.Fatalf("service token revoking as nobody = %d %s", response.Code, response.Body.String())
	}
}

func TestACookieAuthenticatedUnsafeRequestNeedsItsCSRFToken(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := gated.loginAs(t, firstTestPrincipalID)
	createBody := func() *strings.Reader {
		data, _ := json.Marshal(msg.CreateSessionRequest{Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeChat, Origin: "csrf-test",
			Harness: msg.HarnessMock, InstanceID: "inst_test"})
		return strings.NewReader(string(data))
	}

	for name, check := range map[string]struct {
		headerValues []string
		code         string
	}{
		"no token":    {nil, "csrf_token_missing"},
		"empty token": {[]string{""}, "csrf_token_missing"},
		"wrong token": {[]string{"not-the-token"}, "csrf_token_mismatch"},
		"two tokens":  {[]string{"a", "b"}, "csrf_token_mismatch"},
		"other's token": {[]string{func() string {
			other := gated.loginAs(t, secondTestPrincipalID)
			token, _ := csrfTokenByLoginCookieValue.Load(other.Value)
			return token.(string)
		}()}, "csrf_token_mismatch"},
	} {
		request := httptest.NewRequest("POST", "/sessions", createBody())
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		for _, value := range check.headerValues {
			request.Header.Add(csrfTokenHeader, value)
		}
		response := serve(gated.server, request)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), check.code) {
			t.Errorf("%s: POST /sessions = %d %s, want 403 %s", name, response.Code, response.Body.String(), check.code)
		}
	}
	if sessions, _ := gated.store.ListSessions(); len(sessions) != 0 {
		t.Fatalf("a refused request created %d sessions", len(sessions))
	}

	withToken := httptest.NewRequest("POST", "/sessions", createBody())
	withToken.Header.Set("Content-Type", "application/json")
	addLoginCookie(withToken, cookie)
	if response := serve(gated.server, withToken); response.Code != http.StatusCreated {
		t.Fatalf("POST /sessions with the CSRF token = %d %s, want 201", response.Code, response.Body.String())
	}

	// A safe method needs no token.
	read := httptest.NewRequest("GET", "/sessions", nil)
	read.AddCookie(cookie)
	if response := serve(gated.server, read); response.Code != http.StatusOK {
		t.Fatalf("GET with the cookie and no token = %d", response.Code)
	}

	// The service token is not the cookie and is not held to it, even when a
	// stale login cookie rides along.
	asService := httptest.NewRequest("POST", "/sessions", createBody())
	asService.Header.Set("Content-Type", "application/json")
	asService.Header.Set(serviceTokenHeader, testServiceToken)
	asService.AddCookie(cookie)
	if response := serve(gated.server, asService); response.Code != http.StatusCreated {
		t.Fatalf("POST /sessions with the service token = %d %s, want 201", response.Code, response.Body.String())
	}
	asServiceForPrincipal := httptest.NewRequest("POST", "/sessions", createBody())
	asServiceForPrincipal.Header.Set("Content-Type", "application/json")
	asServiceForPrincipal.Header.Set(serviceTokenHeader, testServiceToken)
	asServiceForPrincipal.Header.Set(principalIdentityHeader, firstTestPrincipalID)
	if response := serve(gated.server, asServiceForPrincipal); response.Code != http.StatusCreated {
		t.Fatalf("POST /sessions with the service token for a principal = %d %s, want 201", response.Code, response.Body.String())
	}
}

func TestGetAuthPrincipalGivesAReloadedPageItsCSRFToken(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	login := demoLogin(t, gated.server, firstTestPrincipalID)
	cookie := loginCookieFrom(t, login)
	var fromLogin loggedInPrincipalResponse
	_ = json.Unmarshal(login.Body.Bytes(), &fromLogin)

	who := httptest.NewRequest("GET", "/auth/principal", nil)
	who.AddCookie(cookie)
	answer := serve(gated.server, who)
	var fromPrincipal loggedInPrincipalResponse
	if answer.Code != http.StatusOK || json.Unmarshal(answer.Body.Bytes(), &fromPrincipal) != nil {
		t.Fatalf("GET /auth/principal = %d %s", answer.Code, answer.Body.String())
	}
	if fromPrincipal.CSRFToken == "" || fromPrincipal.CSRFToken != fromLogin.CSRFToken || fromPrincipal.Method != store.LoginSessionMethodDemo ||
		fromPrincipal.LoginSessionID != fromLogin.LoginSessionID {
		t.Fatalf("GET /auth/principal = %+v, login = %+v; want the same session and CSRF token", fromPrincipal, fromLogin)
	}
}

func TestDemoLoginSwitchedOffIs404AndSetsNoCookie(t *testing.T) {
	directory := t.TempDir()
	bridgeStore, err := store.New(filepath.Join(directory, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bridgeStore.Close() })
	cfg := &config.Config{
		ImagesDir:         filepath.Join(directory, "images"),
		BridgePrefsPath:   filepath.Join(directory, "prefs.json"),
		LogStoreURL:       "http://localhost:0",
		PrincipalStoreURL: standardPrincipalDirectory(t).server.URL,
	}
	testAuthorizationConfig(cfg)
	cfg.DemoLoginEnabled = false
	server := New(bridgeStore, nil, nil, nil, nil, testModelStore(t), nil, cfg)

	response := demoLogin(t, server, "principal_000001")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "demo_login_disabled") {
		t.Fatalf("demo login switched off = %d %s, want 404 demo_login_disabled", response.Code, response.Body.String())
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatalf("a switched-off demo login set a cookie")
	}
}
