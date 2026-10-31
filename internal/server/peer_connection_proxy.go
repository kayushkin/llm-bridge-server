package server

// The peer connection proxy: this server's administrators reach a connected
// server through it.
//
// /peer-connections/{id}/proxy/<rest> forwards to <rest> on the other server,
// by https on a direct connection and over the socket on a dial_out one, with
// the method, query and body unchanged, presenting the peer token the other
// server gave this one. It passes the answer back unchanged, flushing as it
// arrives so an SSE stream stays live. The other server decides everything by
// the peer principal the token acts as there. Ids in the answer stay the other
// server's; nothing is copied into this server's stores.
//
// Any principal of this server may use it (phase 2 of design note d107154a).
// A caller who is not an administrator here is named to the other server in
// peerUserHeader by its own principal id, set here from the verified caller
// and never from a header the client sent; the other server acts for it as
// the principal its administrator mapped it to, allowed only what both that
// principal and this server's peer principal may do there, and refuses it
// when no one has mapped it. An administrator here — or the service token
// alone — sends no user and acts as this whole server's peer principal:
// an administrator already holds everything this server holds, and naming
// them would only narrow what the link was granted to do. The one exception
// is starting a session (proxiedRequestStartsASession): an administrator is
// named there too, so the session runs on the other server as the principal
// they are mapped to and its agent gets that principal's token; unnamed, it
// ran with no principal and its agent could reach nothing there. A connected
// server may not go on through the proxy to a third one.
//
// Only the headers in requestHeadersForwardedToPeers cross. Everything else a
// browser or dash sent — the login cookie, its CSRF token, the service token,
// X-Principal-Id, a session agent's Authorization — addresses this server and
// must never reach another one.

import (
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"slices"
	"strings"
)

// requestHeadersForwardedToPeers is every header a proxied request carries to
// the other server, beside the peer token.
var requestHeadersForwardedToPeers = []string{"Content-Type", "Accept", "Last-Event-ID"}

// responseHeadersPassedBackFromPeers are the only headers of the other
// server's answer that reach the browser. The answer is shown on this server's
// own origin, signed in as this server's user, so anything that could make the
// browser act on it is left out: a cookie, a redirect, a refresh, a policy the
// other server chose. Content-Type passes only as passedBackContentType allows.
var responseHeadersPassedBackFromPeers = []string{"Content-Type", "Cache-Control", "Content-Encoding"}

// contentTypesPassedBackFromPeers are the media types a peer's answer may keep:
// the bridge answers JSON and event streams, and a shared file may be a raster
// image, which the browser draws and never runs. Anything else, HTML above all,
// would run as this server's own page; SVG stays out because it can carry
// script. nosniff and the sandbox policy are set on every answer as well.
var contentTypesPassedBackFromPeers = []string{"application/json", "text/event-stream", "image/png", "image/jpeg", "image/gif", "image/webp"}

// passedBackContentType is the Content-Type the browser gets for a peer's
// answer: the peer's own when its media type is one the bridge answers, else
// application/octet-stream, which the browser only downloads.
func passedBackContentType(peerContentType string) string {
	mediaType, _, err := mime.ParseMediaType(peerContentType)
	if err == nil && slices.Contains(contentTypesPassedBackFromPeers, mediaType) {
		return peerContentType
	}
	return "application/octet-stream"
}

// peerProxyHTTPClient forwards proxied requests by https. Like peerHTTPClient
// it verifies certificates with the system's roots and follows no redirect,
// and it has no overall timeout, so a session's stream is not cut off; the
// caller's request context still ends it.
var peerProxyHTTPClient = &http.Client{CheckRedirect: refusePeerRedirect}

// peerConnectionProxyMountPrefix is the path a connection is mounted at.
func peerConnectionProxyMountPrefix(connectionID string) string {
	return "/peer-connections/" + connectionID + "/proxy"
}

// proxiedRequestStartsASession reports whether a proxied request, by its path
// on the other server, creates a session there.
func proxiedRequestStartsASession(method, path string) bool {
	return method == http.MethodPost && path == "/sessions"
}

// handlePeerConnectionProxy: /peer-connections/{id}/proxy/{rest...}, any
// method.
func (s *Server) handlePeerConnectionProxy(w http.ResponseWriter, r *http.Request) {
	connection, ok := s.livePeerConnectionOf(w, r)
	if !ok {
		return
	}
	path := escapedPathAfterPrefix(r.URL, peerConnectionProxyMountPrefix(connection.ID))
	startsASession := proxiedRequestStartsASession(r.Method, path)
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.ContentLength != 0 {
		body = r.Body
	}
	header := http.Header{}
	for _, name := range requestHeadersForwardedToPeers {
		if values := r.Header.Values(name); len(values) > 0 {
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	if caller, ok := callerOfRequest(r); ok {
		if caller.peer != nil {
			writeJSONError(w, http.StatusForbidden, "route_not_for_peers", "a connected server may not go on through this server to another")
			return
		}
		if caller.principalID != "" && (!caller.isAdministrator || startsASession) {
			header.Set(peerUserHeader, caller.principalID)
		}
	}
	// A principal restricted to its grants, such as a service, calls through
	// only a connection its can_call_through grants name; a person holding
	// none is not restricted, as before the relation existed.
	if callThrough, narrowed, err := s.callThroughGrantsOfRequest(r); err != nil {
		writeJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
		return
	} else if narrowed && !callThrough.permits(connection.ID) {
		writeJSONError(w, http.StatusForbidden, "peer_connection_not_granted", fmt.Sprintf(
			"%s; grant can_call_through on peer_connection %s to call through it", callThrough.describe(), connection.ID))
		return
	}
	response, err := s.callWithHeldAccessThrough(r.Context(), peerProxyHTTPClient, connection, r.Method, path, header, body, r.ContentLength)
	if err != nil {
		writePeerCallError(w, err)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if !headerIsOneOf(key, responseHeadersPassedBackFromPeers) {
			continue
		}
		if strings.EqualFold(key, "Content-Type") {
			w.Header().Set("Content-Type", passedBackContentType(response.Header.Get("Content-Type")))
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	// Whatever the answer is, the browser neither guesses another type for it
	// nor runs it as a page of this origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(response.StatusCode)
	if err := copyFlushingEachRead(w, response.Body); err != nil {
		log.Printf("[peer-proxy] %s %s through %s: copy answer: %v", r.Method, path, connection.ID, err)
	}
}

// headerIsOneOf says whether key names one of headers, in any casing.
func headerIsOneOf(key string, headers []string) bool {
	for _, header := range headers {
		if strings.EqualFold(key, header) {
			return true
		}
	}
	return false
}

// copyFlushingEachRead copies body to w and flushes after every read, so each
// SSE frame reaches the browser when the other server sends it.
func copyFlushingEachRead(w http.ResponseWriter, body io.Reader) error {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			if _, err := w.Write(buffer[:read]); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil {
				return fmt.Errorf("flush: %w", err)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
