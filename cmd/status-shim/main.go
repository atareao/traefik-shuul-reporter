// Command status-shim is a compiled Go reverse proxy placed between Traefik
// and a backend. It replaces the Yaegi plugin traefik-shuul-reporter on
// streaming/SSE routes: being compiled (not interpreted by Yaegi), its
// captureWriter IS seen by the proxy transport as http.Flusher, so SSE
// streams arrive chunk-by-chunk, and it still captures the response status
// code and reports status >= -min-status to shuul with the same payload
// contract as the plugin.
//
// Configuration comes from command-line flags, each falling back to an
// environment variable of the same name in upper case:
//
//	-listen       LISTEN      default :8080
//	-backend      BACKEND     required, e.g. http://valet:3000
//	-shuul-url    SHUUL_URL   default http://shuul:3000/api/v1/report
//	-timeout-ms   TIMEOUT_MS  default 500
//	-min-status   MIN_STATUS  default 300
//	-workers      WORKERS     default 4
//	-queue        QUEUE       default 1000
//
// On SIGINT/SIGTERM the http.Server shuts down gracefully and the report
// queue is drained (max 3s) before the process exits 0.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// config holds every runtime option.
type config struct {
	listen    string
	backend   string
	shuulURL  string
	timeout   time.Duration
	minStatus int
	workers   int
	queueSize int
}

func main() {
	log.SetPrefix("status-shim: ")
	log.SetFlags(log.LstdFlags)

	cfg := parseConfig()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// parseConfig reads flags with env-var fallback and validates the result.
func parseConfig() *config {
	cfg := &config{}

	flag.StringVar(&cfg.listen, "listen", envStr("LISTEN", ":8080"),
		"listen address (env LISTEN)")
	flag.StringVar(&cfg.backend, "backend", envStr("BACKEND", ""),
		"backend URL to proxy to, without path, e.g. http://valet:3000 (env BACKEND)")
	flag.StringVar(&cfg.shuulURL, "shuul-url", envStr("SHUUL_URL", "http://shuul:3000/api/v1/report"),
		"shuul report endpoint URL (env SHUUL_URL)")
	timeoutMs := flag.Int("timeout-ms", envInt("TIMEOUT_MS", 500),
		"HTTP client timeout in milliseconds (env TIMEOUT_MS)")
	flag.IntVar(&cfg.minStatus, "min-status", envInt("MIN_STATUS", 300),
		"report only status codes >= this value (env MIN_STATUS)")
	flag.IntVar(&cfg.workers, "workers", envInt("WORKERS", 4),
		"number of POST worker goroutines (env WORKERS)")
	flag.IntVar(&cfg.queueSize, "queue", envInt("QUEUE", 1000),
		"report queue buffer size (env QUEUE)")
	flag.Parse()

	u, err := url.Parse(cfg.backend)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Fatalf("-backend must be an absolute http(s) URL, got %q", cfg.backend)
	}
	if u.Path != "" && u.Path != "/" {
		log.Printf("warning: -backend %q carries a path; the shim proxies to the host only "+
			"(the original request path/query are preserved)", cfg.backend)
	}
	if cfg.workers < 1 {
		cfg.workers = 1
	}
	if cfg.queueSize < 1 {
		cfg.queueSize = 1
	}
	if *timeoutMs <= 0 {
		*timeoutMs = 500
	}
	cfg.timeout = time.Duration(*timeoutMs) * time.Millisecond
	return cfg
}

// envStr returns the env var value, or def when unset/empty.
func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envInt returns the env var parsed as an int, or def when unset/invalid.
func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %d", name, v, def)
		return def
	}
	return n
}

// run wires the pipeline together: http server → captureWriter → report
// queue → workers. It returns when ctx is cancelled (after a graceful
// shutdown and queue drain, max 3s) or on a fatal server error.
func run(ctx context.Context, cfg *config) error {
	backend, err := url.Parse(cfg.backend)
	if err != nil {
		return err
	}

	queue := make(chan reportPayload, cfg.queueSize)
	client := &http.Client{Timeout: cfg.timeout}
	var dropped atomic.Uint64

	// enqueue adds a report to the queue without blocking the proxy: a full
	// queue drops the report (logged) instead of stalling the response path.
	enqueue := func(p reportPayload) {
		select {
		case queue <- p:
		default:
			if d := dropped.Add(1); d%100 == 1 {
				log.Printf("queue full (cap=%d): dropped %d reports so far", cfg.queueSize, d)
			}
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go worker(&wg, queue, client, cfg)
	}

	srv := &http.Server{
		Addr:    cfg.listen,
		Handler: newBackendProxy(backend, cfg.minStatus, enqueue),
	}

	log.Printf("listening on %s | backend=%s shuul=%s timeout=%s min_status=%d workers=%d queue=%d",
		cfg.listen, cfg.backend, cfg.shuulURL, cfg.timeout, cfg.minStatus, cfg.workers, cfg.queueSize)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		// Fall through to graceful shutdown.
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown: %v", err)
	}

	// Shutdown has returned, so no handler is still running; closing the
	// queue is safe and lets the workers drain whatever was enqueued.
	log.Printf("shutting down | dropped=%d", dropped.Load())
	close(queue)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Print("shutdown: all workers drained")
	case <-time.After(3 * time.Second):
		log.Printf("shutdown: timed out after 3s draining, %d jobs left", len(queue))
	}
	return nil
}

// worker POSTs queued payloads to shuul until the queue is closed (drain
// semantics: the context is deliberately not consulted, so every queued
// report is flushed during graceful shutdown).
func worker(wg *sync.WaitGroup, queue <-chan reportPayload, client *http.Client, cfg *config) {
	defer wg.Done()
	for p := range queue {
		data, err := json.Marshal(p)
		if err != nil {
			log.Printf("marshal payload: %v", err)
			continue
		}
		resp, err := client.Post(cfg.shuulURL, "application/json", bytes.NewReader(data))
		if err != nil {
			log.Printf("report POST %s: %v", cfg.shuulURL, err)
			continue
		}
		// Drain the body so the connection can be reused; close always.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
