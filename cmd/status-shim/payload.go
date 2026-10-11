package main

import (
	"net/http"
	"strings"
)

// reportPayload is the JSON structure POSTed to shuul's /api/v1/report. It
// mirrors the plugin's ReportPayload (root package main.go) key-for-key: only
// IpAddress and StatusCode are required, everything else is omitempty.
type reportPayload struct {
	IpAddress      string `json:"ip_address"`
	StatusCode     int    `json:"status_code"`
	Path           string `json:"path,omitempty"`
	Method         string `json:"method,omitempty"`
	UserAgent      string `json:"user_agent,omitempty"`
	Referer        string `json:"referer,omitempty"`
	Fqdn           string `json:"fqdn,omitempty"`
	Query          string `json:"query,omitempty"`
	ContentType    string `json:"content_type,omitempty"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	XRequestId     string `json:"x_request_id,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
}

// extractClientIP extracts the original client IP from the request: the first
// (leftmost) entry of X-Forwarded-For, trimmed, falling back to RemoteAddr
// without its port — the same logic as the plugin.
func extractClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if ip := strings.Split(fwd, ",")[0]; ip != "" {
			return strings.TrimSpace(ip)
		}
	}
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

// buildPayload assembles the shuul report payload from the request being
// proxied and the status code captured at WriteHeader time. Protocol is taken
// from the X-Forwarded-Proto header (set by Traefik), defaulting to http.
func buildPayload(r *http.Request, status int) reportPayload {
	protocol := r.Header.Get("X-Forwarded-Proto")
	if protocol == "" {
		protocol = "http"
	}
	return reportPayload{
		IpAddress:      extractClientIP(r),
		StatusCode:     status,
		Path:           r.URL.Path,
		Method:         r.Method,
		UserAgent:      r.Header.Get("User-Agent"),
		Referer:        r.Header.Get("Referer"),
		Fqdn:           r.Host,
		Query:          r.URL.RawQuery,
		ContentType:    r.Header.Get("Content-Type"),
		AcceptLanguage: r.Header.Get("Accept-Language"),
		XRequestId:     r.Header.Get("X-Request-Id"),
		Protocol:       protocol,
	}
}
