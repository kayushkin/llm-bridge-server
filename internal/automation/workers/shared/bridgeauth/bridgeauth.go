// Package bridgeauth carries a job binary's credential for llm-bridge-server.
//
// llm-bridge-server gates every route: a call with no credential is answered
// 401. Every store a job binary reads and writes is a path under the bridge
// (package bridgestores), and the binary calls all of them as its own service
// principal, with the token the bridge issued for it (POST
// /service-principal-tokens). The bridge treats that token as the principal
// on every route, held to the route rules and its grants; a worker's principal
// is an administrator, because the routes it needs (/issues/items,
// /notes/items, /scheduler/, /models/, /usage/, /principals/) are operator
// routes.
//
// Each binary declares the token in its settings (workersettings) and hands it
// to StampRequestsToBridge once, early in main; every call to the bridge then
// goes out through the stamped default transport, so a caller added later
// cannot quietly forget it.
package bridgeauth

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	// AuthorizationHeader is where llm-bridge-server reads a service
	// principal token, as "Bearer <token>".
	AuthorizationHeader = "Authorization"
	// ServicePrincipalTokenEnvironmentVariable is where a job's command puts
	// the token. This package does not read it.
	ServicePrincipalTokenEnvironmentVariable = "LLM_BRIDGE_SERVICE_PRINCIPAL_TOKEN"
)

// hostScopedTransport stamps the token only on requests to one host.
//
// The host check is the whole point: reminder-coordinator also calls dash with
// the same default transport and dash's own token, and the bridge's token has
// no business being sent to it, nor to anything else a binary calls.
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
	cloned.Header.Set(AuthorizationHeader, "Bearer "+t.token)
	return t.base.RoundTrip(cloned)
}

// StampRequestsToBridge installs that transport as the process-wide default, so
// every call to llm-bridge-server carries the token however it was made:
// http.Post, http.Get, http.DefaultClient, or any client that left Transport
// nil. Call it once, early in main, with the bridge URL the binary will use.
//
// An unset token is not an error here: the bridge's 401 then names the
// missing credential on the first call.
func StampRequestsToBridge(bridgeURL, servicePrincipalToken string) error {
	if servicePrincipalToken == "" {
		return nil
	}
	parsed, err := url.Parse(bridgeURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("bridgeauth: %q is not a URL with a host, so requests to llm-bridge-server cannot be authenticated: %v", bridgeURL, err)
	}
	http.DefaultTransport = &hostScopedTransport{host: parsed.Host, token: servicePrincipalToken, base: http.DefaultTransport}
	http.DefaultClient.Transport = http.DefaultTransport
	return nil
}
