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
// them would only narrow what the link was granted to do. A connected server
// may not go on through the proxy to a third one.
//
// Only the headers in requestHeadersForwardedToPeers cross. Everything else a
// browser or dash sent — the login cookie, its CSRF token, the service token,
// X-Principal-Id, a session agent's Authorization — addresses this server and
// must never reach another one.

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// requestHeadersForwardedToPeers is every header a proxied request carries to
// the other server, beside the peer token.
var requestHeadersForwardedToPeers = []string{"Content-Type", "Accept", "Last-Event-ID"}

// responseHeadersNeverPassedBackFromPeers are left out of the answer: a cookie
// the other server sets is for its own origin, never this one's.
var responseHeadersNeverPassedBackFromPeers = []string{"Set-Cookie"}

// peerProxyHTTPClient forwards proxied requests by https. Like peerHTTPClient
// it verifies certificates with the system's roots and follows no redirect,
// and it has no overall timeout, so a session's stream is not cut off; the
// caller's request context still ends it.
var peerProxyHTTPClient = &http.Client{CheckRedirect: refusePeerRedirect}

// peerConnectionProxyMountPrefix is the path a connection is mounted at.
func peerConnectionProxyMountPrefix(connectionID string) string {
	return "/peer-connections/" + connectionID + "/proxy"
}

// handlePeerConnectionProxy: /peer-connections/{id}/proxy/{rest...}, any
// method.
func (s *Server) handlePeerConnectionProxy(w http.ResponseWriter, r *http.Request) {
	connection, ok := s.livePeerConnectionOf(w, r)
	if !ok {
		return
	}
	path := escapedPathAfterPrefix(r.URL, peerConnectionProxyMountPrefix(connection.ID))
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
		if caller.principalID != "" && !caller.isAdministrator {
			header.Set(peerUserHeader, caller.principalID)
		}
	}
	response, err := s.callWithHeldAccessThrough(r.Context(), peerProxyHTTPClient, connection, r.Method, path, header, body, r.ContentLength)
	if err != nil {
		writePeerCallError(w, err)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if headerIsOneOf(key, hopByHopHeaders) || headerIsOneOf(key, responseHeadersNeverPassedBackFromPeers) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
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
