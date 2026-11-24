// Package inprocesshttp answers an HTTP client's requests with a handler in
// this process, with no socket between them.
//
// A store that moved into this server keeps the HTTP client its callers were
// written against: the client's requests, refusals and answers stay exactly
// what they were, because the store's own handler still makes them. Only the
// network goes. kanban-store reaches noteboard this way, and this server
// reaches kanban-store.
package inprocesshttp

import (
	"bytes"
	"io"
	"net/http"
)

// Transport is an http.RoundTripper that serves every request with Handler.
// The request's context travels with it unchanged, so a value the caller put
// there (who is calling, a deadline) reaches the handler.
type Transport struct {
	Handler http.Handler
}

// RoundTrip serves request with the handler and returns its whole answer. A
// request with no body is given an empty one, as a server would.
func (transport Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	served := request.Clone(request.Context())
	if served.Body == nil {
		served.Body = http.NoBody
	}
	served.RequestURI = request.URL.RequestURI()
	served.Host = request.URL.Host
	recorder := &answerRecorder{header: http.Header{}}
	transport.Handler.ServeHTTP(recorder, served)
	if request.Body != nil {
		request.Body.Close()
	}
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        recorder.header,
		Body:          io.NopCloser(bytes.NewReader(recorder.body.Bytes())),
		ContentLength: int64(recorder.body.Len()),
		Request:       request,
	}, nil
}

// answerRecorder keeps what a handler wrote. The header is frozen at the first
// write, as a server sends it then.
type answerRecorder struct {
	header      http.Header
	wroteHeader bool
	status      int
	body        bytes.Buffer
}

func (recorder *answerRecorder) Header() http.Header { return recorder.header }

func (recorder *answerRecorder) WriteHeader(status int) {
	if recorder.wroteHeader {
		return
	}
	recorder.wroteHeader = true
	recorder.status = status
	recorder.header = recorder.header.Clone()
}

func (recorder *answerRecorder) Write(data []byte) (int, error) {
	if !recorder.wroteHeader {
		recorder.WriteHeader(http.StatusOK)
	}
	return recorder.body.Write(data)
}
