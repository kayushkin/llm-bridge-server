package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// proxiedReadTestPair is two connected servers whose accepting side counts
// the calls that reach a few test routes.
type proxiedReadTestPair struct {
	initiating   *peerTestServer
	initiatingID string
	reads        atomic.Int64
	writes       atomic.Int64
	release      chan struct{}
}

func newProxiedReadTestPair(t *testing.T) *proxiedReadTestPair {
	t.Helper()
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}), http.StatusCreated, "grant")
	pair := &proxiedReadTestPair{initiating: initiating, initiatingID: initiatingID}
	routes := map[string]http.HandlerFunc{
		"GET /a-counted-read": func(w http.ResponseWriter, r *http.Request) {
			count := pair.reads.Add(1)
			if pair.release != nil {
				<-pair.release
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"read":` + strings.Repeat(" ", int(count%2)) + `"` + r.URL.Query().Get("which") + `"}`))
		},
		"POST /a-counted-write": func(w http.ResponseWriter, r *http.Request) {
			pair.writes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		},
	}
	for pattern, handler := range routes {
		accepting.server.mux.HandleFunc(pattern, handler)
		routeAccessRules[pattern] = openRoute("a test route")
		t.Cleanup(func() { delete(routeAccessRules, pattern) })
	}
	return pair
}

func (pair *proxiedReadTestPair) call(t *testing.T, method, rest string) *httptest.ResponseRecorder {
	t.Helper()
	return pair.initiating.requestAsAdministrator(t, method, "/peer-connections/"+pair.initiatingID+"/proxy"+rest, nil)
}

// TestIdenticalProxiedReadsShareOneCall: reads that arrive while one is in
// flight wait for it, and none of them reaches the other server again.
func TestIdenticalProxiedReadsShareOneCall(t *testing.T) {
	pair := newProxiedReadTestPair(t)
	pair.release = make(chan struct{})
	const callers = 6
	var wg sync.WaitGroup
	codes := make(chan int, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- pair.call(t, "GET", "/a-counted-read?which=a").Code
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for pair.reads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give the other callers time to join the read in flight.
	time.Sleep(100 * time.Millisecond)
	close(pair.release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a shared read answered %d, want 200", code)
		}
	}
	if got := pair.reads.Load(); got != 1 {
		t.Fatalf("%d identical reads reached the other server %d times, want once", callers, got)
	}
}

// TestAProxiedReadIsServedAgainWhileFreshAndForgottenAfterAWrite.
func TestAProxiedReadIsServedAgainWhileFreshAndForgottenAfterAWrite(t *testing.T) {
	pair := newProxiedReadTestPair(t)
	first := pair.call(t, "GET", "/a-counted-read?which=a")
	second := pair.call(t, "GET", "/a-counted-read?which=a")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("reads = %d %q and %d %q, want the same answer twice", first.Code, first.Body, second.Code, second.Body)
	}
	if second.Header().Get("Content-Security-Policy") != "sandbox" || second.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("a kept answer lost the headers every proxied answer carries: %v", second.Header())
	}
	if got := pair.reads.Load(); got != 1 {
		t.Fatalf("a fresh answer was read from the other server %d times, want once", got)
	}
	if other := pair.call(t, "GET", "/a-counted-read?which=b"); other.Code != http.StatusOK || pair.reads.Load() != 2 {
		t.Fatalf("a different query = %d after %d reads, want its own call", other.Code, pair.reads.Load())
	}
	if write := pair.call(t, "POST", "/a-counted-write"); write.Code != http.StatusNoContent || pair.writes.Load() != 1 {
		t.Fatalf("a write = %d, %d writes; want it passed through", write.Code, pair.writes.Load())
	}
	pair.call(t, "GET", "/a-counted-read?which=a")
	if got := pair.reads.Load(); got != 3 {
		t.Fatalf("a read after a write was answered from what was kept (%d reads), want a new call", got)
	}

	saved := peerProxySharedAnswerFreshFor
	peerProxySharedAnswerFreshFor = 0
	t.Cleanup(func() { peerProxySharedAnswerFreshFor = saved })
	pair.call(t, "GET", "/a-counted-read?which=a")
	if got := pair.reads.Load(); got != 4 {
		t.Fatalf("a stale answer was served (%d reads), want a new call", got)
	}
}

// TestProxiedCallsPastTheLimitAreRefusedHere: past the connection's allowance
// the proxy answers 429 in JSON itself, and the other server is not called.
func TestProxiedCallsPastTheLimitAreRefusedHere(t *testing.T) {
	savedRate, savedBurst, savedFresh := peerProxyCallsPerSecond, peerProxyCallBurst, peerProxySharedAnswerFreshFor
	peerProxyCallsPerSecond, peerProxyCallBurst, peerProxySharedAnswerFreshFor = 0.001, 2, 0
	t.Cleanup(func() {
		peerProxyCallsPerSecond, peerProxyCallBurst, peerProxySharedAnswerFreshFor = savedRate, savedBurst, savedFresh
	})
	pair := newProxiedReadTestPair(t)
	for i, rest := range []string{"/a-counted-read?which=1", "/a-counted-read?which=2"} {
		if answer := pair.call(t, "GET", rest); answer.Code != http.StatusOK {
			t.Fatalf("call %d within the limit = %d %s", i+1, answer.Code, answer.Body)
		}
	}
	for _, call := range []struct{ method, rest string }{{"GET", "/a-counted-read?which=3"}, {"POST", "/a-counted-write"}} {
		answer := pair.call(t, call.method, call.rest)
		if answer.Code != http.StatusTooManyRequests || !strings.Contains(answer.Body.String(), "peer_call_limit") ||
			!strings.Contains(answer.Header().Get("Content-Type"), "json") || answer.Header().Get("Retry-After") == "" {
			t.Fatalf("%s %s past the limit = %d %q %v, want 429 peer_call_limit in JSON with Retry-After", call.method, call.rest, answer.Code, answer.Body, answer.Header())
		}
	}
	if pair.reads.Load() != 2 || pair.writes.Load() != 0 {
		t.Fatalf("the other server was called %d reads and %d writes, want only the 2 within the limit", pair.reads.Load(), pair.writes.Load())
	}
}

func TestProxiedRequestOnlyReads(t *testing.T) {
	stream := http.Header{"Accept": {"text/event-stream"}}
	for _, c := range []struct {
		method, path string
		header       http.Header
		want         bool
	}{
		{"GET", "/sessions", http.Header{}, true},
		{"GET", "/session-events", stream, false},
		{"POST", "/sessions/validators", http.Header{}, true},
		{"POST", "/sessions/summary", http.Header{}, true},
		{"POST", "/sessions", http.Header{}, false},
		{"POST", "/sessions/br_1/send", http.Header{}, false},
		{"DELETE", "/sessions/br_1", http.Header{}, false},
	} {
		if got := proxiedRequestOnlyReads(c.method, c.path, c.header); got != c.want {
			t.Errorf("proxiedRequestOnlyReads(%s %s, %v) = %v, want %v", c.method, c.path, c.header, got, c.want)
		}
	}
}

func TestSharedProxiedReadKeyTellsCallersAndBodiesApart(t *testing.T) {
	base := sharedProxiedReadKey("c1", "POST", "/sessions/validators", http.Header{}, []byte(`{"ids":["a"]}`))
	named := http.Header{}
	named.Set(peerUserHeader, "principal_000002")
	for name, other := range map[string]string{
		"another body":       sharedProxiedReadKey("c1", "POST", "/sessions/validators", http.Header{}, []byte(`{"ids":["b"]}`)),
		"another user":       sharedProxiedReadKey("c1", "POST", "/sessions/validators", named, []byte(`{"ids":["a"]}`)),
		"another connection": sharedProxiedReadKey("c2", "POST", "/sessions/validators", http.Header{}, []byte(`{"ids":["a"]}`)),
	} {
		if other == base {
			t.Errorf("%s shares a key with the first read", name)
		}
	}
}
