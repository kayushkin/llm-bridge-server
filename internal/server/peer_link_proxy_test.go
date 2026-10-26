package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

const peerProxyTestToken = "peer-token-held-by-the-calling-server"

// linkedTo gives a calling server a link to the server at baseURL, with
// token in a fake auth-store, and trusts certificateClient's test
// certificate for proxied calls. It returns the link.
func linkedTo(t *testing.T, calling *gatedTestServer, baseURL, token string, certificateClient *http.Client) store.PeerLink {
	t.Helper()
	authStore, authStoreServer := newFakeAuthStore(t)
	authStore.tokens["cred_peer"] = token
	calling.server.authClient = authstoreclient.New(authStoreServer.URL, "test-token", "llm-bridge-server")
	previous := peerProxyHTTPClient
	peerProxyHTTPClient = &http.Client{Transport: certificateClient.Transport, CheckRedirect: refusePeerRedirect}
	t.Cleanup(func() { peerProxyHTTPClient = previous })
	link := store.PeerLink{
		BaseURL: baseURL, DisplayName: "far server", RemotePeerPrincipalID: peerTestPrincipalID,
		RemotePeerTokenID: "peer_token_000001", CredentialID: "cred_peer",
		CreatedBy: administratorTestPrincipalID, CreatedAt: time.Now().UTC(),
	}
	if err := calling.store.InsertPeerLink(&link); err != nil {
		t.Fatal(err)
	}
	return link
}

// requestThroughDash is a call as dash makes it for one of its users: the
// service token asserting the principal, plus whatever a browser sent.
func requestThroughDash(method, path, principalID string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, path, body)
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, principalID)
	return request
}

// recordedPeerRequest is what the far server saw.
type recordedPeerRequest struct {
	method, path, query, body string
	header                    http.Header
}

func TestThePeerProxyForwardsTheRequestAndOnlyThePeerToken(t *testing.T) {
	var mutex sync.Mutex
	var seen recordedPeerRequest
	far := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mutex.Lock()
		seen = recordedPeerRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Clone()}
		mutex.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "far_session", Value: "x"})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Far-Server", "yes")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"id":"far_session_1"}`)
	}))
	t.Cleanup(far.Close)
	calling := newGatedTestServer(t, nil)
	link := linkedTo(t, calling, far.URL, peerProxyTestToken, far.Client())

	request := requestThroughDash("POST", "/peer-links/"+link.ID+"/proxy/sessions/a%2Fb/send?wait=1", administratorTestPrincipalID, strings.NewReader(`{"message":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", loginSessionCookieName+"=local-login; other=1")
	request.Header.Set("X-CSRF-Token", "local-csrf")
	request.Header.Set("Authorization", "Bearer local-agent-token")
	answer := serve(calling.server, request)

	if answer.Code != http.StatusAccepted || answer.Body.String() != `{"id":"far_session_1"}` {
		t.Fatalf("answer = %d %s, want the far server's 202 and body unchanged", answer.Code, answer.Body)
	}
	if answer.Header().Get("X-Far-Server") != "yes" || answer.Header().Get("Set-Cookie") != "" {
		t.Fatalf("answer headers = %v: the far server's headers pass back, its cookie does not", answer.Header())
	}
	mutex.Lock()
	defer mutex.Unlock()
	if seen.method != "POST" || seen.path != "/sessions/a%2Fb/send" || seen.query != "wait=1" || seen.body != `{"message":"hi"}` {
		t.Fatalf("the far server saw %s %s?%s %q", seen.method, seen.path, seen.query, seen.body)
	}
	if seen.header.Get(peerTokenHeader) != peerProxyTestToken || seen.header.Get("Content-Type") != "application/json" {
		t.Fatalf("the far server saw headers %v: it needs the peer token and the content type", seen.header)
	}
	for _, local := range []string{serviceTokenHeader, principalIdentityHeader, "Cookie", "X-CSRF-Token", "Authorization"} {
		if value := seen.header.Get(local); value != "" {
			t.Errorf("%s = %q reached the far server; it addresses this server only", local, value)
		}
	}
}

func TestThePeerProxyIsForAdministratorsAndLiveLinks(t *testing.T) {
	far := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the far server was called: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(far.Close)
	calling := newGatedTestServer(t, nil)
	link := linkedTo(t, calling, far.URL, peerProxyTestToken, far.Client())

	if answer := serve(calling.server, requestThroughDash("GET", "/peer-links/"+link.ID+"/proxy/sessions", firstTestPrincipalID, nil)); answer.Code != http.StatusForbidden {
		t.Fatalf("a person who is not an administrator = %d %s, want 403: they would act with the whole server's rights there", answer.Code, answer.Body)
	}
	if answer := serve(calling.server, requestThroughDash("GET", "/peer-links/peer_link_999999/proxy/sessions", administratorTestPrincipalID, nil)); answer.Code != http.StatusNotFound {
		t.Fatalf("an unknown link = %d %s, want 404", answer.Code, answer.Body)
	}
	if _, err := calling.store.RemovePeerLink(link.ID, administratorTestPrincipalID, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if answer := serve(calling.server, requestThroughDash("GET", "/peer-links/"+link.ID+"/proxy/sessions", administratorTestPrincipalID, nil)); answer.Code != http.StatusConflict {
		t.Fatalf("a removed link = %d %s, want 409", answer.Code, answer.Body)
	}
}

func TestThePeerProxyFollowsNoRedirect(t *testing.T) {
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a redirect carried the request to another host, with %s = %q", peerTokenHeader, r.Header.Get(peerTokenHeader))
	}))
	t.Cleanup(elsewhere.Close)
	far := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(far.Close)
	calling := newGatedTestServer(t, nil)
	link := linkedTo(t, calling, far.URL, peerProxyTestToken, far.Client())

	answer := serve(calling.server, requestThroughDash("GET", "/peer-links/"+link.ID+"/proxy/sessions", administratorTestPrincipalID, nil))
	if answer.Code != http.StatusFound || !strings.HasPrefix(answer.Header().Get("Location"), elsewhere.URL) {
		t.Fatalf("answer = %d %v, want the 302 passed back unfollowed", answer.Code, answer.Header())
	}
	if peerHTTPClient.CheckRedirect == nil {
		t.Fatal("peerHTTPClient follows redirects, so /check and redeem would carry the token to another host")
	}
}

// TestThePeerProxyStreamsEachFrameAsItArrives holds the far server's stream
// open after one frame and reads that frame through the proxy: a proxy that
// buffered would never hand it over.
func TestThePeerProxyStreamsEachFrameAsItArrives(t *testing.T) {
	release := make(chan struct{})
	far := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "41" {
			t.Errorf("Last-Event-ID = %q, want 41 passed on so the stream resumes", r.Header.Get("Last-Event-ID"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 42\ndata: {\"type\":\"turn\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(far.Close)
	defer close(release)
	calling := newGatedTestServer(t, nil)
	link := linkedTo(t, calling, far.URL, peerProxyTestToken, far.Client())
	callingOverHTTP := httptest.NewServer(calling.server)
	t.Cleanup(callingOverHTTP.Close)

	request, err := http.NewRequest("GET", callingOverHTTP.URL+"/peer-links/"+link.ID+"/proxy/sessions/s1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, administratorTestPrincipalID)
	request.Header.Set("Last-Event-ID", "41")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q", response.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "id: 42\n" {
		t.Fatalf("first line = %q %v, want the frame while the far stream is still open", first, err)
	}
}

// TestAPeerLinkReachesTheOtherServerThroughTheProxy runs a real link: the
// issuing server answers who the link acts as, through the proxy.
func TestAPeerLinkReachesTheOtherServerThroughTheProxy(t *testing.T) {
	issuing := newPeerTestServer(t, map[string][]string{}, true)
	issuingOverTLS := httptest.NewTLSServer(issuing.server)
	t.Cleanup(issuingOverTLS.Close)
	previousClient := peerHTTPClient
	peerHTTPClient = issuingOverTLS.Client()
	t.Cleanup(func() { peerHTTPClient = previousClient })
	token := issuing.inviteAndRedeem(t)

	calling := newGatedTestServer(t, nil)
	link := linkedTo(t, calling, issuingOverTLS.URL, token.Token, issuingOverTLS.Client())

	answer := serve(calling.server, requestThroughDash("GET", "/peer-links/"+link.ID+"/proxy/peer/self", administratorTestPrincipalID, nil))
	if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), peerTestPrincipalID) {
		t.Fatalf("GET through the proxy = %d %s, want the issuing server naming its peer principal", answer.Code, answer.Body)
	}
	tokens := serve(calling.server, requestThroughDash("GET", "/peer-links/"+link.ID+"/proxy/peer-tokens", administratorTestPrincipalID, nil))
	if tokens.Code != http.StatusOK || !strings.Contains(tokens.Body.String(), token.PeerTokenID) {
		t.Fatalf("an administrator peer's operator route through the proxy = %d %s", tokens.Code, tokens.Body)
	}
}
