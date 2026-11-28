package server

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

// sessionLogModule is the log-store module as this server reaches it.
type sessionLogModule struct {
	// handler serves log-store's own routes (/api/v1/…, /health).
	handler http.Handler
	// client reaches handler in process, for the routes this server answers
	// from log-store's answer.
	client *http.Client
}

// errSessionLogOff is the answer to a read of log-store before
// EnableSessionLog: no request goes anywhere.
var errSessionLogOff = errors.New("the session event log is not enabled in this server")

// getFromLogStore asks log-store, in this process, for target, written
// against inprocesshttp.BaseURL.
func (s *Server) getFromLogStore(target string) (*http.Response, error) {
	module := s.sessionLog.Load()
	if module == nil {
		return nil, errSessionLogOff
	}
	return module.client.Get(target)
}

// logStoreBaseURL is where requests to log-store in this process are
// written. No request leaves the process, so it names nothing on the network.
const logStoreBaseURL = inprocesshttp.BaseURL

// serveLogStoreMount answers /session-log/<rest> with log-store's own route
// /<rest>, unchanged, for a caller written against the standalone service.
func (s *Server) serveLogStoreMount(w http.ResponseWriter, r *http.Request) {
	module := s.sessionLog.Load()
	if module == nil {
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "log_store_off", errSessionLogOff.Error())
		return
	}
	http.StripPrefix("/session-log", module.handler).ServeHTTP(w, r)
}

// registerSessionLogRoutes mounts log-store's own routes. The pattern is
// written out, because the route classification test reads it from a string
// literal.
func (s *Server) registerSessionLogRoutes() {
	s.mux.HandleFunc("/session-log/", s.serveLogStoreMount)
}

// handleSearchSessions proxies /sessions/search to log-store's /api/v1/sessions/search.
func (s *Server) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
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

// handleSessionAggregates proxies /sessions/aggregates to log-store. Kept
// separate from proxyToLogStore because that handler infers the log-store
// path from the URL's {id}/{endpoint} shape, which doesn't apply here.
func (s *Server) handleSessionAggregates(w http.ResponseWriter, r *http.Request) {
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

// proxyToLogStore proxies a session's log-store routes (/sessions/{id}/entries/{eventId})
// to log-store unchanged.
func (s *Server) proxyToLogStore(w http.ResponseWriter, r *http.Request) {
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
	endpoint := logStoreEndpointFromPath(r.URL.EscapedPath())
	if endpoint == "" {
		http.Error(w, "not a log-store session route", http.StatusNotFound)
		return
	}

	// logStoreSessionURL escapes the id, because that is where it becomes a
	// URL. The lookups above keep the real one. Measured: Go's mux matches on
	// the escaped path, so an id holding a %2F really does reach this handler.
	target := logStoreSessionURL(logStoreBaseURL, id, endpoint)
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
