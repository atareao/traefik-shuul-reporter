// Package traefik_shuul_reporter implements a Traefik middleware plugin that captures
// backend HTTP response status codes and reports them to shuul
// for fail2ban-style rate limiting based on response codes.
//
// Architecture:
//
//	Cliente → Traefik [ForwardAuth → Backend → Plugin] → Cliente
//	                                          ↓
//	                              POST /api/v1/report → shuul
//
// The plugin wraps the http.ResponseWriter to intercept the status code
// sent by the backend, then asynchronously POSTs the IP, status code,
// path, and method to shuul's report endpoint for evaluation.
package traefik_shuul_reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Config holds the plugin configuration from Traefik's dynamic config.
type Config struct {
	// ShuulUrl is the full URL to shuul's report endpoint
	// (e.g. "http://shuul:3000/api/v1/report").
	ShuulUrl string `json:"shuulUrl,omitempty"`

	// TimeoutMs is the HTTP client timeout in milliseconds for the
	// report POST request. Default: 500.
	TimeoutMs int `json:"timeoutMs,omitempty"`

	// ReportClientIP controls whether to report the original client IP
	// from X-Forwarded-For or the direct connection IP via RemoteAddr.
	// Default: true.
	ReportClientIP bool `json:"reportClientIP,omitempty"`
}

// ReportPayload is the JSON structure POSTed to shuul's /api/v1/report.
type ReportPayload struct {
	IpAddress  string `json:"ip_address"`
	StatusCode int    `json:"status_code"`
	Path       string `json:"path,omitempty"`
	Method     string `json:"method,omitempty"`
}

// captureWriter wraps http.ResponseWriter to intercept the status code.
type captureWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

// WriteHeader captures the status code before delegating to the real writer.
func (w *captureWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write ensures WriteHeader is called if the backend writes without one.
func (w *captureWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// ShuulPlugin is the Traefik middleware plugin instance.
type ShuulPlugin struct {
	next   http.Handler
	name   string
	config *Config
	client *http.Client
}

// New creates a new plugin instance.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.ShuulUrl == "" {
		config.ShuulUrl = "http://shuul:3000/api/v1/report"
	}
	if config.TimeoutMs <= 0 {
		config.TimeoutMs = 500
	}

	fmt.Printf("traefik-shuul-reporter: plugin loaded | shuul_url=%s timeout_ms=%d report_client_ip=%v\n",
		config.ShuulUrl, config.TimeoutMs, config.ReportClientIP)

	client := &http.Client{
		Timeout: time.Duration(config.TimeoutMs) * time.Millisecond,
	}

	return &ShuulPlugin{
		next:   next,
		name:   name,
		config: config,
		client: client,
	}, nil
}

// ServeHTTP implements the http.Handler interface.
func (p *ShuulPlugin) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	cw := &captureWriter{ResponseWriter: rw}
	p.next.ServeHTTP(cw, req)

	// Only report non-2xx status codes
	if cw.statusCode == 0 || (cw.statusCode >= 200 && cw.statusCode < 300) {
		return
	}

	ip := extractClientIP(req, p.config.ReportClientIP)

	payload := ReportPayload{
		IpAddress:  ip,
		StatusCode: cw.statusCode,
		Path:       req.URL.Path,
		Method:     req.Method,
	}

	go p.sendReport(payload)
}

// extractClientIP extracts the original client IP from the request.
func extractClientIP(r *http.Request, useForwardedFor bool) string {
	if useForwardedFor {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if ip := strings.Split(fwd, ",")[0]; ip != "" {
				return strings.TrimSpace(ip)
			}
		}
	}
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

// sendReport POSTs the report payload to shuul in the background.
func (p *ShuulPlugin) sendReport(payload ReportPayload) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	resp, err := p.client.Post(
		p.config.ShuulUrl,
		"application/json",
		bytes.NewReader(data),
	)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		ShuulUrl:       "http://shuul:3000/api/v1/report",
		TimeoutMs:      500,
		ReportClientIP: true,
	}
}