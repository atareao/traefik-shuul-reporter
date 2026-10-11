package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// newBackendProxy returns a handler that reverse-proxies to backend and
// captures the response status through a captureWriter. The backend URL must
// carry no path: httputil.NewSingleHostReverseProxy then preserves the
// original request path and query. The proxy's FlushInterval is left at its
// default, so chunked/SSE responses are flushed to the client as soon as the
// backend writes them — the compiled captureWriter satisfies http.Flusher,
// which is the whole point of this component (a Yaegi-interpreted wrapper
// never does).
func newBackendProxy(backend *url.URL, minStatus int, enqueue func(reportPayload)) http.Handler {
	rp := httputil.NewSingleHostReverseProxy(backend)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &captureWriter{
			ResponseWriter: w,
			minStatus:      minStatus,
			req:            r,
			enqueue:        enqueue,
		}
		rp.ServeHTTP(cw, r)
	})
}
