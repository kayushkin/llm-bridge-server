package sessions

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
)

// The session event log, moved in from the standalone log-store: its routes
// answered in this process, the bridge's own session routes (messages,
// entries, search, aggregates, validators, the recent bundle) served from it
// with no socket between them, and every event the harness manager writes
// stored through it in process.

// SessionLogModule is the log-store module as this server reaches it.
type SessionLogModule struct {
	// Handler serves log-store's own routes (/api/v1/…, /health).
	Handler http.Handler
	// Client reaches handler in process, for the routes this server answers
	// from log-store's answer.
	Client *http.Client
}

// errSessionLogOff is the answer to a read of log-store before
// EnableSessionLog: no request goes anywhere.
var errSessionLogOff = errors.New("the session event log is not enabled in this server")

// getFromLogStore asks log-store, in this process, for target, written
// against inprocesshttp.BaseURL.
func (s *Service) getFromLogStore(target string) (*http.Response, error) {
	module := s.SessionLog()
	if module == nil {
		return nil, errSessionLogOff
	}
	return module.Client.Get(target)
}

// logStoreBaseURL is where requests to log-store in this process are
// written. No request leaves the process, so it names nothing on the network.
const logStoreBaseURL = inprocesshttp.BaseURL

// serveLogStoreMount answers /session-log/<rest> with log-store's own route
// /<rest>, unchanged, for a caller written against the standalone service.
func (s *Service) serveLogStoreMount(w http.ResponseWriter, r *http.Request) {
	module := s.SessionLog()
	if module == nil {
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "log_store_off", errSessionLogOff.Error())
		return
	}
	http.StripPrefix("/session-log", module.Handler).ServeHTTP(w, r)
}

// HandleSearchSessions proxies /sessions/search to log-store's /api/v1/sessions/search.
func (s *Service) HandleSearchSessions(w http.ResponseWriter, r *http.Request) {
	target := logStoreBaseURL + "/api/v1/sessions/search"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := s.getFromLogStore(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// HandleSessionAggregates proxies /sessions/aggregates to log-store. Kept
// separate from ProxyToLogStore because that handler infers the log-store
// path from the URL's {id}/{endpoint} shape, which doesn't apply here.
func (s *Service) HandleSessionAggregates(w http.ResponseWriter, r *http.Request) {
	target := logStoreBaseURL + "/api/v1/sessions/aggregates"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := s.getFromLogStore(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ProxyToLogStore proxies a session's log-store routes (/sessions/{id}/entries/{eventId})
// to log-store unchanged.
func (s *Service) ProxyToLogStore(w http.ResponseWriter, r *http.Request) {
	// id is the real session id, decoded — that is what every in-process
	// lookup wants.
	id := r.PathValue("id")
	// endpoint is the route's own literal — everything after the id segment, which
	// is one segment for "messages" and "history" and TWO for "messages/raw".
	//
	// This used to be `path.Base(r.URL.EscapedPath())`, on the stated assumption that
	// the literal is "always the last segment". That held for exactly as long as every
	// proxied route was three segments: the first two-segment route, `/messages/raw`,
	// proxied to `/api/v1/sessions/{id}/raw` and 404ed. Read from the ESCAPED path, so
	// an id holding a %2F cannot move the split.
	endpoint := LogStoreEndpointFromPath(r.URL.EscapedPath())
	if endpoint == "" {
		http.Error(w, "not a log-store session route", http.StatusNotFound)
		return
	}

	// LogStoreSessionURL escapes the id, because that is where it becomes a
	// URL. The lookups above keep the real one. Measured: Go's mux matches on
	// the escaped path, so an id holding a %2F really does reach this handler.
	target := LogStoreSessionURL(logStoreBaseURL, id, endpoint)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	resp, err := s.getFromLogStore(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
