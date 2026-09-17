package server

// Identity-carrying store proxies.
//
// kanban-store and grant-store trust X-Principal-Id and filter what a caller
// may see and do by it. These proxies are the only place that header is set:
// from the principal request authorization established (a login cookie or a
// session agent token), never from anything the client sent. Registered only
// when demo login is enabled, behind authorizeAndServe.
//
// Two things make this safe only behind the proxy: every identity header and
// credential the client sent is deleted before forwarding, and the stores must
// not be reachable except through here — a caller who can reach one directly
// can send any X-Principal-Id it likes.

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
)

const (
	// kanbanProxyMountPrefix is where kanban-store is mounted; the remainder of
	// the path is forwarded under kanban-store's /api/.
	kanbanProxyMountPrefix = "/kanban"
	// grantStoreProxyMountPrefix is where grant-store is mounted; the remainder
	// of the path is forwarded under grant-store's root, where all its routes
	// live (/grants, /principals/{id}/effective, /relations, /resource-types).
	grantStoreProxyMountPrefix = "/grant-store"
)

// principalIdentityHeader is the header the stores read the caller's principal
// id from. It is set by the proxy and nowhere else.
const principalIdentityHeader = "X-Principal-Id"

// requestHeadersNeverForwardedToStores are deleted from every proxied request,
// whichever store it is for: identity the proxy alone sets, every store's
// service token (so a client cannot claim the unrestricted identity of any
// store), this server's own service token, and the Authorization header that
// carried a session agent token — it addresses this gateway, not the store.
var requestHeadersNeverForwardedToStores = []string{
	principalIdentityHeader,
	"X-Kanban-Store-Service-Token",
	grantclient.ServiceTokenHeader,
	serviceTokenHeader,
	"Authorization",
}

// hopByHopHeaders are connection-scoped and not forwarded (RFC 9110 §7.6.1);
// the outbound connection sets its own.
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// storeProxyHTTPClient forwards to the stores. It does not follow redirects: a
// redirect is part of the store's answer and is passed back unchanged. No
// overall timeout, so a slow body is not cut off mid-stream; the caller's
// request context still cancels it.
var storeProxyHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// principalIdentityStoreProxy describes one store mounted behind the gateway.
type principalIdentityStoreProxy struct {
	// storeName names the store in errors and logs, e.g. "kanban-store".
	storeName string
	// mountPrefix is the gateway path the store is mounted under, without a
	// trailing slash.
	mountPrefix string
	// upstreamBaseURL is the store's base URL.
	upstreamBaseURL string
	// upstreamPathPrefix is prepended to the path after mountPrefix: "/api"
	// for kanban-store, "" for grant-store.
	upstreamPathPrefix string
	// unavailableErrorCode is the error code of the 502 answered when the
	// store cannot be reached.
	unavailableErrorCode string
}

func (s *Server) handleKanbanStoreProxyAsPrincipal(w http.ResponseWriter, r *http.Request) {
	s.serveStoreProxyAsPrincipal(w, r, principalIdentityStoreProxy{
		storeName:            "kanban-store",
		mountPrefix:          kanbanProxyMountPrefix,
		upstreamBaseURL:      s.cfg.KanbanStoreURL,
		upstreamPathPrefix:   "/api",
		unavailableErrorCode: "kanban_store_unavailable",
	})
}

// handleGrantStoreProxyAsPrincipal forwards /grant-store/<rest> to grant-store
// /<rest>: grant-store's routes are rooted at /, so /grant-store/grants,
// /grant-store/grants/{id}/revoke, /grant-store/principals/{id}/effective,
// /grant-store/relations and /grant-store/resource-types all reach it.
func (s *Server) handleGrantStoreProxyAsPrincipal(w http.ResponseWriter, r *http.Request) {
	s.serveStoreProxyAsPrincipal(w, r, principalIdentityStoreProxy{
		storeName:            "grant-store",
		mountPrefix:          grantStoreProxyMountPrefix,
		upstreamBaseURL:      s.cfg.GrantStoreURL,
		upstreamPathPrefix:   "",
		unavailableErrorCode: "grant_store_unavailable",
	})
}

// serveStoreProxyAsPrincipal forwards <mountPrefix>/<rest> to
// <upstreamBaseURL><upstreamPathPrefix>/<rest> as the principal request
// authorization put on the request, with method, query, body and every other
// header unchanged, and passes the store's answer back unchanged.
func (s *Server) serveStoreProxyAsPrincipal(w http.ResponseWriter, r *http.Request, proxy principalIdentityStoreProxy) {
	principalID, restricted := principalRestrictingRequest(r)
	if !restricted {
		// Only the service token passes authorization without a principal.
		writeJSONError(w, http.StatusForbidden, "store_proxy_requires_a_principal", fmt.Sprintf(
			"%s/ forwards to %s as a principal, and the service token is not one; an internal service calls %s directly with that store's own service token",
			proxy.mountPrefix, proxy.storeName, proxy.storeName))
		return
	}

	target := strings.TrimSuffix(proxy.upstreamBaseURL, "/") + proxy.upstreamPathPrefix + escapedPathAfterPrefix(r.URL, proxy.mountPrefix)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	outbound, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "proxy_request_invalid", fmt.Sprintf("build request to %s %s: %v", proxy.storeName, target, err))
		return
	}
	outbound.ContentLength = r.ContentLength
	outbound.Header = r.Header.Clone()
	for _, header := range hopByHopHeaders {
		outbound.Header.Del(header)
	}
	// Header.Del canonicalises, so it removes any casing a client sent through
	// a normal client; this loop also removes a non-canonical key set directly
	// on the map.
	for key := range outbound.Header {
		for _, removed := range requestHeadersNeverForwardedToStores {
			if strings.EqualFold(key, removed) {
				delete(outbound.Header, key)
			}
		}
	}
	outbound.Header.Set(principalIdentityHeader, principalID)
	removeCookieFromRequestHeader(outbound.Header, demoLoginCookieName)

	response, err := storeProxyHTTPClient.Do(outbound)
	if err != nil {
		log.Printf("[%s-proxy] %s %s as %s: %v", proxy.storeName, r.Method, target, principalID, err)
		writeJSONError(w, http.StatusBadGateway, proxy.unavailableErrorCode, fmt.Sprintf("%s unreachable at %s: %v", proxy.storeName, proxy.upstreamBaseURL, err))
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("[%s-proxy] %s %s as %s: copy response body: %v", proxy.storeName, r.Method, target, principalID, err)
	}
}
