// Package bridgeauth carries this repo's credential for llm-bridge-server.
//
// llm-bridge-server gates every route: a call with no credential is answered
// 401. Every binary here that reaches it — the oneshot classifier calls, the
// demo session spawns — speaks to it as an internal service, with the token the
// scheduler unit holds. One place reads the environment, so a caller added
// later cannot quietly forget it.
package bridgeauth

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	// ServiceTokenHeader is where llm-bridge-server reads the token.
	ServiceTokenHeader = "X-LLM-Bridge-Service-Token"
	// ServiceTokenEnvironmentVariable is where the unit carries it; the jobs
	// the scheduler runs inherit it.
	ServiceTokenEnvironmentVariable = "LLMBRIDGE_SERVICE_TOKEN"
)

type serviceTokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *serviceTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set(ServiceTokenHeader, t.token)
	return t.base.RoundTrip(cloned)
}

// AuthenticateClient puts the token on every request the client makes and
// returns it. An unset token leaves the client alone rather than sending a
// blank header, so llm-bridge-server's 401 names the missing header.
func AuthenticateClient(client *http.Client) *http.Client {
	token := os.Getenv(ServiceTokenEnvironmentVariable)
	if token == "" {
		return client
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &serviceTokenTransport{token: token, base: base}
	return client
}

// hostScopedTransport stamps the token only on requests to one host.
//
// The host check is the whole point: these binaries also call noteboard,
// kanban-store, mailstack and model-store with the same default transport, and
// llm-bridge-server's token has no business being sent to any of them.
type hostScopedTransport struct {
	host  string
	token string
	base  http.RoundTripper
}

func (t *hostScopedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !strings.EqualFold(request.URL.Host, t.host) {
		return t.base.RoundTrip(request)
	}
	cloned := request.Clone(request.Context())
	cloned.Header.Set(ServiceTokenHeader, t.token)
	return t.base.RoundTrip(cloned)
}

// StampRequestsToBridge installs that transport as the process-wide default, so
// every call to llm-bridge-server carries the token however it was made:
// http.Post, http.Get, http.DefaultClient, or any client that left Transport
// nil. Call it once, early in main, with the bridge URL the binary will use.
//
// An unset token is not an error here: the binary is then expected to be
// talking to a server that does not gate, and llm-bridge-server's 401 names the
// missing header if it does.
func StampRequestsToBridge(bridgeURL string) error {
	token := os.Getenv(ServiceTokenEnvironmentVariable)
	if token == "" {
		return nil
	}
	parsed, err := url.Parse(bridgeURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("bridgeauth: %q is not a URL with a host, so requests to llm-bridge-server cannot be authenticated: %v", bridgeURL, err)
	}
	http.DefaultTransport = &hostScopedTransport{host: parsed.Host, token: token, base: http.DefaultTransport}
	http.DefaultClient.Transport = http.DefaultTransport
	return nil
}
