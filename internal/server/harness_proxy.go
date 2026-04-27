package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
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

// handleHarnessProxy forwards an authenticated request to the
// corresponding harness backend on the bridge host. Path layout:
//
//	/api/harness-proxy/{harness}/<rest> → <backend>/<rest>
//
// Method, query string, headers, and request body are passed through
// verbatim. Response is streamed back so SSE-style backends keep
// working. The request body is read with a generous limit but no
// rewriting; runners are trusted (they presented a valid
// runner_token earlier on the WS).
func (s *Server) handleHarnessProxy(w http.ResponseWriter, r *http.Request) {
	harness := r.PathValue("harness")
	rest := r.PathValue("rest")
	if harness == "" {
		http.Error(w, "missing harness", http.StatusBadRequest)
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
		// Strip hop-by-hop headers and the runner's bearer (it's for the
		// bridge, not for the upstream).
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
