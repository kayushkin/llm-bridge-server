package referencestore

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// inProcessBaseURL is the address of an in-process request. Handler sees only
// its path; the host names nothing.
const inProcessBaseURL = "http://in-process"

// inProcessTransport hands a request to a handler and keeps its answer in
// memory. The answers it carries are a record, a search or one picture, each
// read to a bound the caller sets.
type inProcessTransport struct{ handler http.Handler }

func (transport inProcessTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.handler == nil {
		return nil, fmt.Errorf("no in-process handler for %s", request.URL.Path)
	}
	recorder := &answerRecorder{header: http.Header{}}
	transport.handler.ServeHTTP(recorder, request)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return &http.Response{
		Status:        strconv.Itoa(recorder.status) + " " + http.StatusText(recorder.status),
		StatusCode:    recorder.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        recorder.header,
		Body:          io.NopCloser(bytes.NewReader(recorder.body.Bytes())),
		ContentLength: int64(recorder.body.Len()),
		Request:       request,
	}, nil
}

// answerRecorder is the http.ResponseWriter an in-process request is
// answered into.
type answerRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (recorder *answerRecorder) Header() http.Header { return recorder.header }

func (recorder *answerRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
}

func (recorder *answerRecorder) Write(data []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.body.Write(data)
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
			client: &http.Client{Transport: inProcessTransport{handler: r.InProcess.Handler},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
			baseURL:   inProcessBaseURL + prefix,
			inProcess: true,
		}, nil
	}
	baseURL, err := r.serviceURL(service)
	if err != nil {
		return serviceEndpoint{}, err
	}
	return serviceEndpoint{client: r.HTTPClient, baseURL: baseURL}, nil
}
