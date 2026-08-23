package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// harnessBackendURL maps a harness short-name to the URL of its locally-
// running backend service. The proxy forwards everything under
// /api/harness-proxy/{harness}/* to the matching upstream so service-
// style harnesses (inber, hermes, …) can run their backend once on the
// bridge host and have wrappers on remote runners hit it without
// duplicating state, credentials, or persistent storage.
//
// Each entry is overridable via env (LLMBRIDGE_HARNESS_PROXY_<NAME>),
// since a self-hoster might run inber on a non-default port. Empty
// string disables the proxy for that harness.
func harnessBackendURL(name string) string {
	envKey := "LLMBRIDGE_HARNESS_PROXY_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	switch strings.ToLower(name) {
	case "inber":
		return "http://localhost:8200"
	case "hermes":
		return "http://localhost:8500"
	default:
		return ""
	}
}

// restEscapesBackendPath reports whether rest, resolved as a path
// relative to the backend's own path, climbs above it.
//
// It has to be asked at all because neither layer either side of this
// handler removes a dot-segment. Go's ServeMux cleans a *raw* `../` with
// a redirect, but leaves a percent-encoded `..%2f` alone and PathValue
// then hands the handler the decoded `../`; Go's HTTP client does not
// clean the outgoing path either. So a `..` in rest survives verbatim
// into the upstream request, and against a backend URL carrying a path
// prefix it addresses whatever sits above that prefix.
//
// Only rest is judged, and only as a predicate — the caller still
// forwards it unchanged. Cleaning the whole target would collapse an
// empty segment in the operator's own backend URL and silently reroute
// requests that work today.
//
// A dot-segment that resolves back inside is not an escape and is left
// alone: it addresses the same place under the prefix either way.
func restEscapesBackendPath(rest string) bool {
	cleaned := path.Clean(strings.TrimLeft(rest, "/"))
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}

// handleHarnessProxy forwards a request to the corresponding harness
// backend on the bridge host. Path layout:
//
//	/api/harness-proxy/{harness}/<rest> → <backend>/<rest>
//
// This route authenticates nobody. Its two sibling seed proxies,
// proxyAgentStore and proxySkillStore, registered a few lines above it
// in server.go, both call authorizeRunnerRequest and answer 401. This
// one has no gate, and neither ServeHTTP nor the mux supplies one.
//
// What made that look settled is that runners reach the bridge over
// /api/runner/ws having presented a runner_token, and the header loop
// below strips an incoming Authorization as though one were always
// there. But this is plain HTTP on the same mux, and nothing ties a
// request arriving on it to that handshake, so the caller is whoever
// can reach the listener. Whether to add the gate or leave the route
// open on purpose is still open — noteboard card f4e5e1ef — and the
// wider exposure it sits inside is f02351f2.
//
// Method, query string, headers and request body pass through
// verbatim. The body streams straight to the upstream under no size
// limit: this handler sets none, and nothing wraps the mux. The
// response streams back, so SSE-style backends keep working.
//
// The one thing not passed through is a <rest> that climbs above the
// backend's own path — see restEscapesBackendPath — which is refused
// with 400 rather than forwarded.
func (s *Server) handleHarnessProxy(w http.ResponseWriter, r *http.Request) {
	harness := r.PathValue("harness")
	rest := r.PathValue("rest")
	if harness == "" {
		http.Error(w, "missing harness", http.StatusBadRequest)
		return
	}
	if restEscapesBackendPath(rest) {
		http.Error(w, "proxy path escapes the harness backend", http.StatusBadRequest)
		return
	}

	backend := harnessBackendURL(harness)
	if backend == "" {
		http.Error(w, fmt.Sprintf("no backend proxy registered for harness %q", harness), http.StatusNotFound)
		return
	}

	target, err := url.Parse(backend)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid backend URL: %v", err), http.StatusInternalServerError)
		return
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(rest, "/")
	target.RawQuery = r.URL.RawQuery

	// 5 min ceiling so SSE / long-lived sessions don't get cut by a
	// client-side default. Caller is expected to manage their own
	// timeouts via context cancellation.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("build proxy request: %v", err), http.StatusInternalServerError)
		return
	}
	for k, vs := range r.Header {
		// Strip hop-by-hop headers, and the caller's Authorization with
		// them: it addresses the bridge, not the upstream. On this route
		// the bridge does not read it either — see the doc comment above.
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade", "authorization":
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(out)
	if err != nil {
		log.Printf("[harness-proxy] %s %s → %s: %v", r.Method, harness, target, err)
		http.Error(w, fmt.Sprintf("proxy upstream: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
