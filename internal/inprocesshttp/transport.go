// Package inprocesshttp hands an HTTP request to a handler in this process
// instead of sending it over a socket. A client of a store that moved into
// this server keeps the requests it made and the answers it read, while
// nothing leaves the process and no credential is presented: the handler is
// the store's own, reached past the server's route rules, so whoever builds
// the client decides what it may ask.
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

// Transport is an http.RoundTripper over Handler. The answer's body streams
// as the handler writes it, so a large file is not held in memory.
type Transport struct {
	Handler http.Handler
}

// Client is an http.Client over handler that does not follow redirects, so a
// caller reads the handler's own answer.
func Client(handler http.Handler) *http.Client {
	return &http.Client{
		Transport:     Transport{Handler: handler},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (transport Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.Handler == nil {
		return nil, fmt.Errorf("no in-process handler for %s %s", request.Method, request.URL.Path)
	}
	reader, writer := io.Pipe()
	answer := &streamingAnswer{header: http.Header{}, body: writer, started: make(chan struct{})}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				answer.start(http.StatusInternalServerError)
				writer.CloseWithError(fmt.Errorf("the in-process handler of %s panicked: %v", request.URL.Path, recovered))
				return
			}
			answer.start(http.StatusOK)
			writer.Close()
		}()
		transport.Handler.ServeHTTP(answer, request)
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

// streamingAnswer is the http.ResponseWriter a handler answers an
// in-process request into. Its status and headers are taken when the handler
// first writes, as net/http takes them.
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
