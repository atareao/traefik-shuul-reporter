package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
)

// captureWriter wraps http.ResponseWriter to intercept the first status code
// written by the backend. If that status is >= minStatus it enqueues a report
// payload immediately: the final status is known at the first WriteHeader,
// and a mid-stream SSE error travels as a 200 event, not as a status change.
//
// Unlike the Yaegi-interpreted plugin, this is compiled Go, so the wrapper
// keeps its dynamic type: Flusher/Hijacker/Pusher/ReaderFrom are delegated to
// the underlying writer and http.Flusher assertions on this type succeed,
// which is what keeps SSE streaming intact behind a reverse proxy.
type captureWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	minStatus   int
	req         *http.Request
	enqueue     func(reportPayload)
}

// WriteHeader captures the first status code and forwards the header write to
// the real writer. Subsequent calls are forwarded unchanged; only the first
// status is captured (and possibly reported).
func (w *captureWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
		if w.enqueue != nil && code >= w.minStatus {
			w.enqueue(buildPayload(w.req, code))
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write ensures WriteHeader is invoked if the backend writes without one
// (implicit 200), then delegates.
func (w *captureWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher by delegating to the wrapped writer. Required
// so SSE streams (text/event-stream) are not buffered into bursts by the
// reverse proxy: w.(http.Flusher) must succeed on this type (compiled Go).
func (w *captureWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack implements http.Hijacker by delegating, so WebSocket upgrades keep
// working through the shim.
func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// Push implements http.Pusher (HTTP/2 server push) by delegating.
func (w *captureWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// ReadFrom implements io.ReaderFrom for zero-copy body copies.
func (w *captureWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w}, r)
}
