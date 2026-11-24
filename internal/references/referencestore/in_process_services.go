package referencestore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
)

// InProcessServices are the owning services that run in the same process as
// this store. A lookup in one is handed to Handler as a request for the
// service's path prefix and the type's route, and never leaves the process.
//
// Handler answers as the server's internal service: the server that builds it
// gives every request it hands on that server's own trust, so no credential
// is applied here. A request for an owner-decided type still carries the
// caller in PrincipalIDHeader, so the owner narrows the answer to what the
// caller may see, as it did over HTTP.
type InProcessServices struct {
	Handler http.Handler
	// PathPrefixes maps a service, as a type row names it, to where Handler
	// serves that service's routes: "/prediction-store" for prediction-store,
	// "" for routes at Handler's root.
	PathPrefixes map[string]string
}

// serviceEndpoint is where one service's routes are asked, and how.
type serviceEndpoint struct {
	client    *http.Client
	baseURL   string
	inProcess bool
}

// endpoint is where service answers: in this process when InProcess names
// it, else at its configured address. An unconfigured service is an error,
// never a relative request.
func (r *Resolver) endpoint(service string) (serviceEndpoint, error) {
	if prefix, inProcess := r.InProcess.PathPrefixes[service]; inProcess {
		return serviceEndpoint{
			client:    inprocesshttp.Client(r.InProcess.Handler),
			baseURL:   inprocesshttp.BaseURL + prefix,
			inProcess: true,
		}, nil
	}
	baseURL, err := r.serviceURL(service)
	if err != nil {
		return serviceEndpoint{}, err
	}
	return serviceEndpoint{client: r.HTTPClient, baseURL: baseURL}, nil
}
