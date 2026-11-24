// Package inprocesshttp answers an HTTP client's requests with a handler in
// this process, with no socket between them.
//
// A store that moved into this server keeps the HTTP client its callers were
// written against: the client's requests, refusals and answers stay exactly
// what they were, because the store's own handler still makes them. Only the
// network goes. kanban-store reaches noteboard this way, this server reaches
// kanban-store and file-store, project-store reaches repo-store and
// work-graph-store, and reference-store reaches the stores in this process.
// No credential is presented: whoever builds the client decides which handler
// it reaches, and so what it may ask.
package inprocesshttp

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// BaseURL is the address of an in-process request. The handler sees only the
// path and query; the host names nothing.
const BaseURL = "http://in-process"

// Client is an http.Client over handler that does not follow redirects, so a
// caller reads the handler's own answer.
func Client(handler http.Handler) *http.Client {
	return &http.Client{
		Transport:     Transport{Handler: handler},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Transport is an http.RoundTripper that serves every request with Handler.
// The request's context travels with it unchanged, so a value the caller put
// there (who is calling, a deadline) reaches the handler. The answer's body
// streams as the handler writes it, so a large file is not held in memory.
type Transport struct {
	Handler http.Handler
}

// RoundTrip serves request with the handler. A request with no body is given
// an empty one, as a server would. It returns once the handler has written
// its status, or has returned without writing one (200).
func (transport Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.Handler == nil {
		return nil, fmt.Errorf("no in-process handler for %s %s", request.Method, request.URL.Path)
	}
	served := request.Clone(request.Context())
	if served.Body == nil {
		served.Body = http.NoBody
	}
	served.RequestURI = request.URL.RequestURI()
	served.Host = request.URL.Host
	reader, writer := io.Pipe()
	answer := &streamingAnswer{header: http.Header{}, body: writer, started: make(chan struct{})}
	go func() {
		defer func() {
			if request.Body != nil {
				request.Body.Close()
			}
			if recovered := recover(); recovered != nil {
				answer.start(http.StatusInternalServerError)
				writer.CloseWithError(fmt.Errorf("the in-process handler of %s panicked: %v", request.URL.Path, recovered))
				return
			}
			answer.start(http.StatusOK)
			writer.Close()
		}()
		transport.Handler.ServeHTTP(answer, served)
	}()
	select {
	case <-answer.started:
	case <-request.Context().Done():
		reader.CloseWithError(request.Context().Err())
		return nil, request.Context().Err()
	}
	return &http.Response{
		Status:        strconv.Itoa(answer.status) + " " + http.StatusText(answer.status),
		StatusCode:    answer.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        answer.sentHeader,
		Body:          reader,
		ContentLength: -1,
		Request:       request,
	}, nil
}

// streamingAnswer is the http.ResponseWriter a handler answers an in-process
// request into. Its status and headers are frozen when the handler first
// writes, as a server sends them then.
type streamingAnswer struct {
	header     http.Header
	sentHeader http.Header
	status     int
	body       *io.PipeWriter
	once       sync.Once
	started    chan struct{}
}

func (answer *streamingAnswer) Header() http.Header { return answer.header }

func (answer *streamingAnswer) start(status int) {
	answer.once.Do(func() {
		answer.status = status
		answer.sentHeader = answer.header.Clone()
		close(answer.started)
	})
}

func (answer *streamingAnswer) WriteHeader(status int) { answer.start(status) }

func (answer *streamingAnswer) Write(data []byte) (int, error) {
	answer.start(http.StatusOK)
	return answer.body.Write(data)
}
