package server

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// gzipWriterPool recycles gzip.Writers across requests so a busy endpoint
// doesn't allocate a fresh compressor (and its ~256KB window) per response.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipResponseWriter compresses the response body when the client accepts
// gzip and the response is a compressible content type. The gzip/passthrough
// decision is deferred to the first WriteHeader/Write so it can read the
// Content-Type the handler set. Two response shapes must pass through
// uncompressed and both do: text/event-stream (SSE) — gzip would buffer
// frames the client needs immediately — and hijacked connections (the
// gorilla WebSocket upgrade for pty attach), which take over the raw socket
// before any body is written, leaving compress false.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compress    bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wroteHeader = true
	h := w.ResponseWriter.Header()
	// Skip if the handler already encoded the body (Content-Encoding set) or
	// the type doesn't benefit. An empty Content-Type means the handler is
	// relying on net/http's content sniffing — treat that as non-compressible
	// rather than guess.
	if h.Get("Content-Encoding") == "" && isCompressibleContentType(h.Get("Content-Type")) {
		w.compress = true
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		// The declared length is for the uncompressed body; it no longer
		// matches what goes on the wire.
		h.Del("Content-Length")
		w.gz = gzipWriterPool.Get().(*gzip.Writer)
		w.gz.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps SSE working: for a compressed body it flushes the gzip stream
// first so buffered bytes reach the client, then the underlying writer. For a
// pass-through body it just forwards to the underlying Flusher.
func (w *gzipResponseWriter) Flush() {
	if w.compress && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so the gorilla WebSocket upgrader
// (pty attach) can take over the connection. Without this the wrapper would
// mask the ResponseWriter's Hijacker and every attach upgrade would fail.
func (w *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// close finishes the gzip stream (writing the trailer) and returns the writer
// to the pool. Safe to call when compression never engaged.
func (w *gzipResponseWriter) close() {
	if w.compress && w.gz != nil {
		_ = w.gz.Close()
		gzipWriterPool.Put(w.gz)
		w.gz = nil
	}
}

// isCompressibleContentType reports whether gzip is worth applying to a body
// of the given Content-Type. Text-shaped payloads (JSON, HTML, JS, XML)
// compress well; already-compressed binary (images, event-stream) does not.
func isCompressibleContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	// SSE must stream frame-by-frame; buffering it in a compressor stalls the
	// client, so never compress it even though it's text.
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	return strings.Contains(ct, "json") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "xml")
}
