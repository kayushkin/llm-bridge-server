// Package httproutes is how a package that answers some of this server's
// routes registers them. Each such package exports a Mount function taking a
// Mux; the server passes its own, which records every pattern it is given so
// a test can hold each one to the server's access rules wherever it was
// registered.
package httproutes

import (
	"net/http"
	"sync"
)

// Mux is the part of *http.ServeMux a package registers its routes on.
type Mux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// RecordingMux is an *http.ServeMux that remembers every pattern registered
// on it, in the order they came.
type RecordingMux struct {
	*http.ServeMux
	mutex    sync.Mutex
	patterns []string
}

// NewRecordingMux is an empty RecordingMux.
func NewRecordingMux() *RecordingMux {
	return &RecordingMux{ServeMux: http.NewServeMux()}
}

// Handle registers handler for pattern and records the pattern.
func (mux *RecordingMux) Handle(pattern string, handler http.Handler) {
	mux.record(pattern)
	mux.ServeMux.Handle(pattern, handler)
}

// HandleFunc registers handler for pattern and records the pattern.
func (mux *RecordingMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	mux.record(pattern)
	mux.ServeMux.HandleFunc(pattern, handler)
}

// Patterns is every pattern registered so far, in the order they came.
func (mux *RecordingMux) Patterns() []string {
	mux.mutex.Lock()
	defer mux.mutex.Unlock()
	return append([]string(nil), mux.patterns...)
}

func (mux *RecordingMux) record(pattern string) {
	mux.mutex.Lock()
	defer mux.mutex.Unlock()
	mux.patterns = append(mux.patterns, pattern)
}
