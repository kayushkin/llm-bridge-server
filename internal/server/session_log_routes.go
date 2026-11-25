package server

import (
	"errors"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
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

// EnableSessionLog serves log-store's routes from handler and has the harness
// manager write and read every session event through events. main calls it
// once the module is provisioned, migrated and open, before any session
// starts or resumes. In the server both are the log-store module's server;
// a test may give a fake log-store's handler and the HTTP client over it.
func (s *Server) EnableSessionLog(handler http.Handler, events harness.SessionEventLog) {
	s.sessionLog.Store(&sessionLogModule{handler: handler, client: inprocesshttp.Client(handler)})
	s.harness.SetSessionEventLog(events)
}

// UseSessionEventStore names where the harness manager stores every event
// and where this server reads them back: the stream, the stream head, crash
// recovery, the renamer, the brief pass, the reaper and wakeup redelivery:
// the session log, the only copy. main calls it once, after EnableSessionLog
// and before any session starts or resumes.
func (s *Server) UseSessionEventStore(events sessionevents.Store) {
	s.harness.SetSessionEventStore(events)
}

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

// serveLogStoreMount answers /log-store/<rest> with log-store's own route
// /<rest>, unchanged, for a caller written against the standalone service.
func (s *Server) serveLogStoreMount(w http.ResponseWriter, r *http.Request) {
	module := s.sessionLog.Load()
	if module == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "log_store_off", errSessionLogOff.Error())
		return
	}
	http.StripPrefix("/log-store", module.handler).ServeHTTP(w, r)
}

// registerSessionLogRoutes mounts log-store's own routes. The pattern is
// written out, because the route classification test reads it from a string
// literal.
func (s *Server) registerSessionLogRoutes() {
	s.mux.HandleFunc("/log-store/", s.serveLogStoreMount)
}
