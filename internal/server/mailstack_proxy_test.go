package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The mailstack proxy is the store proxy with one difference: mailstack takes
// a bearer from every caller, so the gateway presents its own after deleting
// the client's. The client's session agent token must never reach mailstack,
// and mailstack must see the verified principal and nothing a client forged.
func TestMailstackProxyPresentsItsOwnBearerAndOnlyThePrincipal(t *testing.T) {
	var mutex sync.Mutex
	var seen []recordedKanbanRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mutex.Lock()
		seen = append(seen, recordedKanbanRequest{Method: r.Method, EscapedPath: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: string(body)})
		mutex.Unlock()
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		w.Header().Set("ETag", `"lm1.r1.html"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<p>body</p>"))
	}))
	t.Cleanup(upstream.Close)

	gated := newGatedTestServer(t, nil)
	gated.server.cfg.MailstackURL = upstream.URL
	gated.server.cfg.MailstackToken = "mailstack-bearer"
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	session := gated.createSessionAs(t, cookie, "")
	agentToken := gated.sessionAgentTokenFor(t, session)

	for _, credential := range []string{"cookie", "session agent token"} {
		mutex.Lock()
		seen = nil
		mutex.Unlock()
		request := httptest.NewRequest(http.MethodGet, "/mailstack/api/messages/lm1/body?account=desk&format=html", nil)
		request.Header.Set("X-Kanban-Store-Service-Token", "forged")
		if credential == "cookie" {
			request.AddCookie(cookie)
		} else {
			request.Header.Set("Authorization", "Bearer "+agentToken)
		}
		response := serve(gated.server, request)
		if response.Code != http.StatusOK || response.Body.String() != "<p>body</p>" {
			t.Fatalf("%s: %d %q", credential, response.Code, response.Body.String())
		}
		if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("ETag") == "" {
			t.Fatalf("%s: mailstack's headers were not passed back", credential)
		}
		mutex.Lock()
		got := append([]recordedKanbanRequest(nil), seen...)
		mutex.Unlock()
		if len(got) != 1 {
			t.Fatalf("%s: mailstack reached %d times, want 1", credential, len(got))
		}
		if got[0].EscapedPath != "/api/messages/lm1/body" || got[0].RawQuery != "account=desk&format=html" {
			t.Fatalf("%s: forwarded as %s ? %s", credential, got[0].EscapedPath, got[0].RawQuery)
		}
		if auth := got[0].Header.Values("Authorization"); len(auth) != 1 || auth[0] != "Bearer mailstack-bearer" {
			t.Fatalf("%s: mailstack saw Authorization %v, want only the gateway's bearer", credential, auth)
		}
		if p := got[0].Header.Values("X-Principal-Id"); len(p) != 1 || p[0] != firstTestPrincipalID {
			t.Fatalf("%s: mailstack saw X-Principal-Id %v", credential, p)
		}
		if got[0].Header.Get("X-Kanban-Store-Service-Token") != "" {
			t.Fatalf("%s: a forged store token reached mailstack", credential)
		}
		if strings.Contains(got[0].Header.Get("Cookie"), loginSessionCookieName) {
			t.Fatalf("%s: the login cookie reached mailstack", credential)
		}
	}
}

func TestMailstackProxyWithoutATokenIsUnavailable(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	gated.server.cfg.MailstackToken = ""
	request := httptest.NewRequest(http.MethodGet, "/mailstack/api/accounts", nil)
	request.AddCookie(loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID)))
	if response := serve(gated.server, request); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", response.Code, response.Body.String())
	}
}

func TestMailstackProxyNeedsAPerson(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	gated.server.cfg.MailstackToken = "mailstack-bearer"
	if response := serve(gated.server, httptest.NewRequest(http.MethodGet, "/mailstack/api/accounts", nil)); response.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d, want 401", response.Code)
	}
}
