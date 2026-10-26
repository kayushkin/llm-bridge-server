package server

// The peer link proxy: this server's administrators reach a linked server
// through it.
//
// /peer-links/{id}/proxy/<rest> forwards to <link base URL>/<rest> with the
// method, query and body unchanged, presenting the link's peer token, and
// passes the other server's answer back unchanged, flushing as it arrives so
// an SSE stream stays live. The other server decides everything by the peer
// principal the token acts as there: its grants, its groups and its
// administrator flag. Ids in the answer stay the other server's; nothing is
// copied into this server's stores.
//
// Only administrators may use it. The other server sees one principal for the
// whole of this server (phase 1 of design note
// d107154a-49ac-471b-a1b7-fc7975de37f3), so a person who is not an
// administrator here would otherwise act with the peer's rights there. Phase 2
// names the person as well, and can open this route wider.
//
// Only the headers in requestHeadersForwardedToPeers cross. Everything else a
// browser or dash sent — the login cookie, its CSRF token, the service token,
// X-Principal-Id, a session agent's Authorization — addresses this server and
// must never reach another one.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// requestHeadersForwardedToPeers is every header a proxied request carries to
// the other server, beside the peer token: what the body is, what answer is
// wanted, and where a reconnecting SSE stream resumes.
var requestHeadersForwardedToPeers = []string{"Content-Type", "Accept", "Last-Event-ID"}

// responseHeadersNeverPassedBackFromPeers are left out of the answer: a cookie
// the other server sets is for its own origin, never this one's.
var responseHeadersNeverPassedBackFromPeers = []string{"Set-Cookie"}

// peerProxyHTTPClient forwards proxied requests. Like peerHTTPClient it
// verifies certificates with the system's roots and follows no redirect, and
// it has no overall timeout, so a session's stream is not cut off; the
// caller's request context still ends it.
var peerProxyHTTPClient = &http.Client{CheckRedirect: refusePeerRedirect}

// refusePeerRedirect passes a redirect back as the answer instead of
// following it: Go would carry X-LLM-Bridge-Peer-Token to whatever host the
// redirect names.
func refusePeerRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// peerLinkProxyMountPrefix is the path the link with this id is mounted at.
func peerLinkProxyMountPrefix(linkID string) string {
	return "/peer-links/" + linkID + "/proxy"
}

// handlePeerLinkProxy: /peer-links/{id}/proxy/{rest...}, any method.
func (s *Server) handlePeerLinkProxy(w http.ResponseWriter, r *http.Request) {
	link, err := s.store.GetPeerLink(r.PathValue("id"))
	if errors.Is(err, store.ErrPeerLinkNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_link_not_found", "no peer link has id "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if link.RemovedAt != nil {
		writeJSONError(w, http.StatusConflict, "peer_link_removed", fmt.Sprintf("%s was removed at %s", link.ID, link.RemovedAt.UTC().Format("2006-01-02T15:04:05Z07:00")))
		return
	}

	path := escapedPathAfterPrefix(r.URL, peerLinkProxyMountPrefix(link.ID))
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.ContentLength != 0 {
		body = r.Body
	}
	response, err := s.callPeerWithHeaders(r.Context(), peerProxyHTTPClient, link, r.Method, path, body, func(outbound *http.Request) {
		outbound.ContentLength = r.ContentLength
		for _, header := range requestHeadersForwardedToPeers {
			if value := r.Header.Values(header); len(value) > 0 {
				outbound.Header[http.CanonicalHeaderKey(header)] = append([]string(nil), value...)
			}
		}
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "peer_unreachable", err.Error())
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
		log.Printf("[peer-proxy] %s %s%s through %s: copy answer: %v", r.Method, link.BaseURL, path, link.ID, err)
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
// SSE frame reaches the browser when the other server sends it rather than
// when a buffer fills.
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
