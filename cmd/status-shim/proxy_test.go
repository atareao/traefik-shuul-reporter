package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flushRecorder counts Flush() calls on top of httptest.ResponseRecorder so
// the SSE test can assert each chunk was flushed through the proxy.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int32
}

func (r *flushRecorder) Flush() {
	atomic.AddInt32(&r.flushes, 1)
	r.ResponseRecorder.Flush()
}

// shuulRecorder is an httptest shuul endpoint that records every payload.
type shuulRecorder struct {
	mu    sync.Mutex
	posts []map[string]any
}

func (s *shuulRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.posts = append(s.posts, m)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// TestEndToEndSSE proves the core claim of the component: through the
// compiled shim, an SSE stream from the backend arrives chunk-by-chunk (the
// reverse proxy flushes because captureWriter is a real http.Flusher) and a
// 200 SSE response is never reported to shuul.
func TestEndToEndSSE(t *testing.T) {
	chunks := []string{"data: one\n\n", "data: two\n\n", "data: three\n\n"}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest backend does not support Flush")
			return
		}
		for i, c := range chunks {
			if _, err := w.Write([]byte(c)); err != nil {
				return
			}
			flusher.Flush()
			if i < len(chunks)-1 {
				time.Sleep(15 * time.Millisecond)
			}
		}
	}))
	defer backend.Close()

	shuul := &shuulRecorder{}
	shuulSrv := httptest.NewServer(shuul)
	defer shuulSrv.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	var reported atomic.Bool
	proxy := newBackendProxy(backendURL, 300, func(reportPayload) { reported.Store(true) })

	req := httptest.NewRequest(http.MethodGet, "http://example.com/events?stream=1", nil)
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	proxy.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, c := range chunks {
		for _, line := range strings.Split(strings.TrimSpace(c), "\n") {
			if !strings.Contains(body, line) {
				t.Fatalf("chunk %q missing from proxied body: %q", c, body)
			}
		}
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("proxied status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("proxied Content-Type = %q, want text/event-stream", ct)
	}
	// Each backend chunk must have been flushed to the client; more flushes
	// than chunks are fine (the reverse proxy may flush the initial header).
	if got := atomic.LoadInt32(&rec.flushes); got < int32(len(chunks)) {
		t.Fatalf("proxy flushed %d times, want >= %d (streaming intact)", got, len(chunks))
	}
	if reported.Load() {
		t.Fatal("a 200 SSE stream was enqueued for reporting")
	}

	shuul.mu.Lock()
	defer shuul.mu.Unlock()
	if len(shuul.posts) != 0 {
		t.Fatalf("shuul received %d reports for a 200 SSE stream, want 0: %v", len(shuul.posts), shuul.posts)
	}
}

// TestEndToEndReport wires the whole pipeline against a real shim server: a
// backend that 500s, a slow shuul (100ms per POST) and a single worker. The
// first 500 arrives with the exact shuul contract payload; the second 500 is
// still queued when the context is cancelled, exercising the graceful
// shutdown drain (both reports must reach shuul).
func TestEndToEndReport(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	shuul := &shuulRecorder{}
	shuulSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shuul.ServeHTTP(w, r)
		time.Sleep(100 * time.Millisecond) // slow shuul → queue backlog
	}))
	defer shuulSrv.Close()

	// Pick a free local port for the shim's http.Server.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	cfg := &config{
		listen:    fmt.Sprintf("127.0.0.1:%d", port),
		backend:   backend.URL,
		shuulURL:  shuulSrv.URL,
		timeout:   500 * time.Millisecond,
		minStatus: 300,
		workers:   1,
		queueSize: 16,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()

	shimURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}
	waitShimUp(t, client, shimURL+"/health")

	// First 500: triggers exactly one report with the correct payload.
	first := newReportRequest(shimURL+"/boom?x=1", "198.51.100.7")
	resp, err := client.Do(first)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first request status = %d, want 500", resp.StatusCode)
	}
	waitReportCount(t, shuul, 1, 5*time.Second)

	// Second 500 while shuul is still slow: must be drained on shutdown.
	second := newReportRequest(shimURL+"/boom?x=1", "203.0.113.9")
	resp, err = client.Do(second)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("second request status = %d, want 500", resp.StatusCode)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("run did not return after cancellation")
	}

	shuul.mu.Lock()
	defer shuul.mu.Unlock()
	if len(shuul.posts) != 2 {
		t.Fatalf("shuul received %d reports, want exactly 2 (drained): %v", len(shuul.posts), shuul.posts)
	}
	for _, p := range shuul.posts {
		if p["status_code"] == float64(200) {
			t.Fatalf("a 200 health poll was reported: %v", p)
		}
	}
	checkReport(t, shuul.posts, "198.51.100.7")
	checkReport(t, shuul.posts, "203.0.113.9")
}

// newReportRequest builds a request that the shim will see the way Traefik
// forwards it: Host set, XFF/X-Forwarded-Proto already populated by Traefik.
func newReportRequest(target, ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("a=b"))
	req.RequestURI = "" // cleared: RequestURI can't be set in client requests
	req.Host = "valet.example.com"
	req.Header.Set("X-Forwarded-For", ip+", 10.0.0.1")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("User-Agent", "curl/8")
	req.Header.Set("Referer", "https://ref.example/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "es-ES,es;q=0.9")
	req.Header.Set("X-Request-Id", "rid-"+ip)
	return req
}

// checkReport asserts that the report for ip carries the exact shuul contract
// fields the plugin would send.
func checkReport(t *testing.T, posts []map[string]any, ip string) {
	t.Helper()
	for _, p := range posts {
		if p["ip_address"] == ip {
			if p["status_code"] != float64(500) {
				t.Fatalf("report for %s: status_code = %v, want 500", ip, p["status_code"])
			}
			for k, want := range map[string]any{
				"path": "/boom", "method": "POST", "query": "x=1",
				"fqdn": "valet.example.com", "protocol": "https",
				"user_agent": "curl/8", "referer": "https://ref.example/",
				"content_type":    "application/x-www-form-urlencoded",
				"accept_language": "es-ES,es;q=0.9", "x_request_id": "rid-" + ip,
			} {
				if p[k] != want {
					t.Errorf("report for %s: %s = %v, want %v", ip, k, p[k], want)
				}
			}
			return
		}
	}
	t.Fatalf("no report for ip %q in %v", ip, posts)
}

func waitShimUp(t *testing.T, client *http.Client, healthURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("status-shim server did not come up in time")
}

func waitReportCount(t *testing.T, s *shuulRecorder, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		c := len(s.posts)
		s.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("timed out waiting for %d reports, got %d: %v", n, len(s.posts), s.posts)
}
