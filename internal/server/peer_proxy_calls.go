package server

// How often this server calls a connected server through the proxy, decided
// here rather than by each browser tab.
//
// Every open tab of the chat reads a connected server's sessions through the
// proxy on its own timers. Measured 2026-11-02 in one hour: 553 proxied calls
// to one connected server from two browsers, 253 of them the sessions
// validator sweep, one a minute from each tab. Nothing here shared or limited
// them, so the number of open tabs set how hard the other machine was asked.
//
// Two rules now sit between the browser and the other server:
//
//   - Identical reads share one call. A read is a GET that is not an event
//     stream, or a POST to one of proxiedPostsThatOnlyRead. Reads the same
//     caller sends while one is in flight wait for it, and a successful
//     answer is served again for peerProxySharedAnswerFreshFor. Any other
//     request through the connection forgets the kept answers, so a read
//     after a write goes to the other server.
//   - Each connection may be called at most peerProxyCallsPerSecond times a
//     second, with bursts of up to peerProxyCallBurst. A call past that is
//     answered 429 here, in JSON, without reaching the other server; the
//     chat shows it as the server's answer and does not retry it.
//
// Whether the other server answers at all is watched by the socket probes
// (peer_socket.go), which refuse calls at once while it does not.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"golang.org/x/time/rate"
)

// Vars so tests can change them.
var (
	peerProxySharedAnswerFreshFor = 5 * time.Second
	peerProxyCallsPerSecond       = 5.0
	peerProxyCallBurst            = 60
	// peerProxyLargestSharedAnswer is the most of an answer kept in memory to
	// share. A larger one goes to the caller that asked for it, and the callers
	// waiting on it make their own calls.
	peerProxyLargestSharedAnswer = 8 << 20
	// peerProxyLargestSharedReadBody is the most of a POST read's body read to
	// tell one read from another; a larger body is refused.
	peerProxyLargestSharedReadBody int64 = 1 << 20
	// peerProxySharedReadTimeout bounds a shared read, which runs on past the
	// caller that started it so the callers waiting on it still get an answer.
	peerProxySharedReadTimeout = 2 * time.Minute
)

// proxiedPostsThatOnlyRead are the POST routes, by path on the other server,
// that only read: they carry a list of ids in the body because a long query
// string is refused by the web server in front.
var proxiedPostsThatOnlyRead = []string{"/sessions/validators", "/sessions/summary"}

// errPeerCallLimit is a call refused here because the connection has had its
// share of calls for now.
var errPeerCallLimit = errors.New("this server limits how often it calls a connected server")

// proxiedRequestOnlyReads reports whether a proxied request, by its method,
// path on the other server and the headers it forwards, only reads.
func proxiedRequestOnlyReads(method, path string, header http.Header) bool {
	switch method {
	case http.MethodGet:
		return !strings.Contains(header.Get("Accept"), "text/event-stream")
	case http.MethodPost:
		return slices.Contains(proxiedPostsThatOnlyRead, path)
	}
	return false
}

// peerProxyAnswer is an answer kept to share.
type peerProxyAnswer struct {
	status int
	header http.Header
	body   []byte
	at     time.Time
}

// peerProxyRead is one read in flight. done closes when it settles: with
// answer, with err, or with neither when the answer could not be shared — an
// event stream, or one too large to keep.
type peerProxyRead struct {
	done   chan struct{}
	answer *peerProxyAnswer
	err    error
}

// peerProxyCalls is the proxy's shared reads and call limits, per connection.
// The zero value is ready to use.
type peerProxyCalls struct {
	mutex    sync.Mutex
	inFlight map[string]*peerProxyRead
	kept     map[string]*peerProxyAnswer
	limiters map[string]*rate.Limiter
}

// allow takes one call from the connection's allowance, reporting false when
// there is none left now.
func (calls *peerProxyCalls) allow(connectionID string) bool {
	calls.mutex.Lock()
	defer calls.mutex.Unlock()
	if calls.limiters == nil {
		calls.limiters = map[string]*rate.Limiter{}
	}
	limiter := calls.limiters[connectionID]
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(peerProxyCallsPerSecond), peerProxyCallBurst)
		calls.limiters[connectionID] = limiter
	}
	return limiter.Allow()
}

// forgetAnswersOf drops every answer kept for the connection.
func (calls *peerProxyCalls) forgetAnswersOf(connectionID string) {
	prefix := connectionID + "\x00"
	calls.mutex.Lock()
	defer calls.mutex.Unlock()
	for key := range calls.kept {
		if strings.HasPrefix(key, prefix) {
			delete(calls.kept, key)
		}
	}
}

// joinOrStart returns an answer still fresh, or the read in flight to wait
// on, or — when there is neither — a new read the caller must settle.
func (calls *peerProxyCalls) joinOrStart(key string) (fresh *peerProxyAnswer, waitOn *peerProxyRead, started *peerProxyRead) {
	calls.mutex.Lock()
	defer calls.mutex.Unlock()
	if answer := calls.kept[key]; answer != nil {
		if time.Since(answer.at) < peerProxySharedAnswerFreshFor {
			return answer, nil, nil
		}
		delete(calls.kept, key)
	}
	if read := calls.inFlight[key]; read != nil {
		return nil, read, nil
	}
	if calls.inFlight == nil {
		calls.inFlight = map[string]*peerProxyRead{}
	}
	read := &peerProxyRead{done: make(chan struct{})}
	calls.inFlight[key] = read
	return nil, nil, read
}

// settle ends a read, keeping a successful answer.
func (calls *peerProxyCalls) settle(key string, read *peerProxyRead) {
	calls.mutex.Lock()
	defer calls.mutex.Unlock()
	delete(calls.inFlight, key)
	if read.answer != nil && read.answer.status >= 200 && read.answer.status < 300 {
		if calls.kept == nil {
			calls.kept = map[string]*peerProxyAnswer{}
		}
		// Expired answers go when a new one is kept, so the map holds at most
		// what was read in the last few seconds.
		for other, answer := range calls.kept {
			if time.Since(answer.at) >= peerProxySharedAnswerFreshFor {
				delete(calls.kept, other)
			}
		}
		calls.kept[key] = read.answer
	}
	close(read.done)
}

// sharedProxiedReadKey names a read: the connection, what is asked, and
// everything that can change the answer — the user named to the other server,
// the headers forwarded and the body.
func sharedProxiedReadKey(connectionID, method, pathAndQuery string, header http.Header, body []byte) string {
	hash := sha256.New()
	for _, name := range append([]string{peerUserHeader}, requestHeadersForwardedToPeers...) {
		fmt.Fprintf(hash, "%s=%q\n", name, header.Values(name))
	}
	hash.Write(body)
	return connectionID + "\x00" + method + " " + pathAndQuery + "\x00" + hex.EncodeToString(hash.Sum(nil))
}

// writePeerProxyAnswer writes a kept answer.
func writePeerProxyAnswer(w http.ResponseWriter, answer *peerProxyAnswer) {
	for key, values := range answer.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(answer.status)
	_, _ = w.Write(answer.body)
}

// serveSharedProxiedRead answers a read through the proxy, sharing one call
// to the other server among every identical read.
func (s *Server) serveSharedProxiedRead(w http.ResponseWriter, r *http.Request, connection store.PeerConnection, pathAndQuery string, header http.Header) {
	var body []byte
	if r.Method != http.MethodGet && r.ContentLength != 0 {
		read, err := io.ReadAll(io.LimitReader(r.Body, peerProxyLargestSharedReadBody+1))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "unreadable_body", err.Error())
			return
		}
		if int64(len(read)) > peerProxyLargestSharedReadBody {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("a read through the proxy carries at most %d bytes", peerProxyLargestSharedReadBody))
			return
		}
		body = read
	}
	key := sharedProxiedReadKey(connection.ID, r.Method, pathAndQuery, header, body)
	fresh, waitOn, started := s.peerProxyCalls.joinOrStart(key)
	if fresh != nil {
		writePeerProxyAnswer(w, fresh)
		return
	}
	if waitOn != nil {
		select {
		case <-waitOn.done:
		case <-r.Context().Done():
			return
		}
		if waitOn.answer != nil {
			writePeerProxyAnswer(w, waitOn.answer)
			return
		}
		if waitOn.err != nil {
			writePeerCallError(w, waitOn.err)
			return
		}
		// The answer was too large to keep: make this caller's own call.
		s.callPeerAndPassBack(w, r, connection, pathAndQuery, header, body)
		return
	}

	if !s.peerProxyCalls.allow(connection.ID) {
		started.err = errPeerCallLimit
		s.peerProxyCalls.settle(key, started)
		writePeerCallError(w, errPeerCallLimit)
		return
	}
	// The read runs on if the caller that started it goes away, so the callers
	// waiting on it are not left with that caller's cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), peerProxySharedReadTimeout)
	defer cancel()
	response, err := s.callWithHeldAccessThrough(ctx, peerProxyHTTPClient, connection, r.Method, pathAndQuery, header, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		started.err = err
		s.peerProxyCalls.settle(key, started)
		writePeerCallError(w, err)
		return
	}
	defer response.Body.Close()
	passedBack := http.Header{}
	passBackPeerAnswerHeaders(passedBack, response.Header)
	if mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type")); mediaType == "text/event-stream" {
		// A stream never ends, so it cannot be kept: it goes to this caller as
		// it comes, and the callers waiting on it make their own calls.
		s.peerProxyCalls.settle(key, started)
		for name, values := range passedBack {
			w.Header()[name] = values
		}
		w.WriteHeader(response.StatusCode)
		if err := copyFlushingEachRead(w, response.Body); err != nil {
			log.Printf("[peer-proxy] %s %s through %s: copy answer: %v", r.Method, pathAndQuery, connection.ID, err)
		}
		return
	}
	answerBody, err := io.ReadAll(io.LimitReader(response.Body, int64(peerProxyLargestSharedAnswer)+1))
	if err != nil {
		started.err = fmt.Errorf("read the other server's answer: %w", err)
		s.peerProxyCalls.settle(key, started)
		writePeerCallError(w, started.err)
		return
	}
	if len(answerBody) > peerProxyLargestSharedAnswer {
		// Too large to keep: this caller gets it as it comes, and the callers
		// waiting on it make their own calls.
		s.peerProxyCalls.settle(key, started)
		for name, values := range passedBack {
			w.Header()[name] = values
		}
		w.WriteHeader(response.StatusCode)
		if _, err := w.Write(answerBody); err != nil {
			return
		}
		if err := copyFlushingEachRead(w, response.Body); err != nil {
			log.Printf("[peer-proxy] %s %s through %s: copy answer: %v", r.Method, pathAndQuery, connection.ID, err)
		}
		return
	}
	started.answer = &peerProxyAnswer{status: response.StatusCode, header: passedBack, body: answerBody, at: time.Now()}
	s.peerProxyCalls.settle(key, started)
	writePeerProxyAnswer(w, started.answer)
}

// callPeerAndPassBack makes one call of its own for a read that could not be
// shared, within the connection's limit.
func (s *Server) callPeerAndPassBack(w http.ResponseWriter, r *http.Request, connection store.PeerConnection, pathAndQuery string, header http.Header, body []byte) {
	if !s.peerProxyCalls.allow(connection.ID) {
		writePeerCallError(w, errPeerCallLimit)
		return
	}
	response, err := s.callWithHeldAccessThrough(r.Context(), peerProxyHTTPClient, connection, r.Method, pathAndQuery, header, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		writePeerCallError(w, err)
		return
	}
	defer response.Body.Close()
	passBackPeerAnswerHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if err := copyFlushingEachRead(w, response.Body); err != nil {
		log.Printf("[peer-proxy] %s %s through %s: copy answer: %v", r.Method, pathAndQuery, connection.ID, err)
	}
}
