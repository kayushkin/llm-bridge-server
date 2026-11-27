package signin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	operatorDiscordUser = "104357516833472512"
	strangerDiscordUser = "999999999999999999"
)

// startDiscordRoundTrip calls GET /discord/start and returns the state Discord
// would carry back and the attempt cookie.
func (service *testService) startDiscordRoundTrip(t *testing.T, purpose string, cookies ...*http.Cookie) (string, *http.Cookie) {
	t.Helper()
	request := httptest.NewRequest("GET", "/discord/start?purpose="+purpose+"&return_to="+url.QueryEscape("https://dash.example.test/"), nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	service.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound {
		t.Fatalf("start %s = %d %s", purpose, recorder.Code, recorder.Body)
	}
	location, _ := url.Parse(recorder.Header().Get("Location"))
	if location.Query().Get("scope") != "identify" || location.Query().Get("redirect_uri") != testDiscordRedirect {
		t.Fatalf("authorize URL = %s", location)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == discordAttemptCookieName {
			return location.Query().Get("state"), cookie
		}
	}
	t.Fatal("no attempt cookie")
	return "", nil
}

// finishDiscordRoundTrip calls GET /discord/callback as Discord would send the
// browser back, and returns where it was sent and the cookies set.
func (service *testService) finishDiscordRoundTrip(t *testing.T, state, discordUser string, cookies ...*http.Cookie) (*url.URL, []*http.Cookie) {
	t.Helper()
	request := httptest.NewRequest("GET", "/discord/callback?code="+discordUser+"&state="+url.QueryEscape(state), nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	service.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", recorder.Code, recorder.Body)
	}
	location, _ := url.Parse(recorder.Header().Get("Location"))
	return location, recorder.Result().Cookies()
}

func TestLinkingDiscordThenSigningInWithIt(t *testing.T) {
	service := newTestService(t)
	// Not linked yet: Discord proves who they are, but nobody here is them.
	state, attempt := service.startDiscordRoundTrip(t, discordPurposeSignIn)
	back, _ := service.finishDiscordRoundTrip(t, state, operatorDiscordUser, attempt)
	if back.Query().Get("sign_in_error") != discordErrorNotLinked || back.Host != "dash.example.test" {
		t.Fatalf("an unlinked account = %s, want dash with %s", back, discordErrorNotLinked)
	}

	// Signed in with a link, the operator links their own Discord account.
	session := sessionCookieFrom(t, service.do(t, "POST", "/redeem", map[string]string{"secret": service.makeLink(t, testAdministrator)}, nil, nil))
	state, attempt = service.startDiscordRoundTrip(t, discordPurposeLink, session)
	back, _ = service.finishDiscordRoundTrip(t, state, operatorDiscordUser, attempt)
	if back.Query().Get("discord_linked") != "1" || service.world.discordBindings[operatorDiscordUser] != testAdministrator || service.world.linkedBy[operatorDiscordUser] != testAdministrator {
		t.Fatalf("link = %s, bindings %v linked by %v", back, service.world.discordBindings, service.world.linkedBy)
	}

	// Now Discord alone signs them in.
	state, attempt = service.startDiscordRoundTrip(t, discordPurposeSignIn)
	back, cookies := service.finishDiscordRoundTrip(t, state, operatorDiscordUser, attempt)
	var signedIn *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == SessionCookieName && cookie.Value != "" {
			signedIn = cookie
		}
	}
	if back.String() != "https://dash.example.test/" || signedIn == nil || signedIn.Domain != "example.test" {
		t.Fatalf("sign in with Discord = %s, cookie %+v", back, signedIn)
	}
	current := service.do(t, "GET", "/session", nil, nil, signedIn)
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"method":"discord"`) || current.Header().Get(SignedInPrincipalHeader) != testAdministrator {
		t.Fatalf("the Discord session = %d %s", current.Code, current.Body)
	}

	// The same account cannot be linked to someone else.
	person := sessionCookieFrom(t, service.do(t, "POST", "/redeem", map[string]string{"secret": service.makeLink(t, testPerson)}, nil, nil))
	state, attempt = service.startDiscordRoundTrip(t, discordPurposeLink, person)
	back, _ = service.finishDiscordRoundTrip(t, state, operatorDiscordUser, attempt)
	if back.Query().Get("sign_in_error") != discordErrorAlreadyLinked || service.world.discordBindings[operatorDiscordUser] != testAdministrator {
		t.Fatalf("linking a taken account = %s", back)
	}
}

func TestADiscordRoundTripIsSpentOnceAndOnlyInItsOwnBrowser(t *testing.T) {
	service := newTestService(t)
	service.world.discordBindings[operatorDiscordUser] = testAdministrator
	state, attempt := service.startDiscordRoundTrip(t, discordPurposeSignIn)
	back, cookies := service.finishDiscordRoundTrip(t, state, operatorDiscordUser)
	if back.Query().Get("sign_in_error") != discordErrorAttempt {
		t.Fatalf("a callback without the attempt cookie = %s", back)
	}
	for _, cookie := range cookies {
		if cookie.Name == SessionCookieName && cookie.Value != "" {
			t.Fatal("a callback in another browser signed someone in")
		}
	}
	back, _ = service.finishDiscordRoundTrip(t, state, operatorDiscordUser, attempt)
	if back.Query().Get("sign_in_error") != discordErrorAttempt || back.Host != "example.test" {
		t.Fatalf("a spent attempt used again = %s, want the sign-in page with %s", back, discordErrorAttempt)
	}
	state, attempt = service.startDiscordRoundTrip(t, discordPurposeSignIn)
	request := httptest.NewRequest("GET", "/discord/callback?error=access_denied&state="+url.QueryEscape(state), nil)
	request.AddCookie(attempt)
	recorder := httptest.NewRecorder()
	service.handler.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Header().Get("Location"), discordErrorDenied) {
		t.Fatalf("Discord denied = %s", recorder.Header().Get("Location"))
	}
}

func TestDiscordStartRefusesAForeignReturnAndALinkWithNoSession(t *testing.T) {
	service := newTestService(t)
	for _, target := range []string{"https://evil.example/", "http://dash.example.test/", "https://example.test.evil.example/"} {
		answer := service.do(t, "GET", "/discord/start?return_to="+url.QueryEscape(target), nil, nil, nil)
		if answer.Code != http.StatusBadRequest {
			t.Errorf("return_to %s = %d, want 400", target, answer.Code)
		}
	}
	if answer := service.do(t, "GET", "/discord/start?purpose=link", nil, nil, nil); answer.Code != http.StatusUnauthorized {
		t.Errorf("link with no session = %d, want 401", answer.Code)
	}
}
