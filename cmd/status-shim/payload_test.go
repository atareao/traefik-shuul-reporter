package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractClientIP(t *testing.T) {
	t.Run("first entry of multi-hop XFF", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1, 172.17.0.2")
		if got := extractClientIP(r); got != "203.0.113.9" {
			t.Fatalf("extractClientIP = %q, want 203.0.113.9", got)
		}
	})

	t.Run("XFF entry with surrounding spaces is trimmed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		r.Header.Set("X-Forwarded-For", "  198.51.100.7  , 10.0.0.1")
		if got := extractClientIP(r); got != "198.51.100.7" {
			t.Fatalf("extractClientIP = %q, want 198.51.100.7", got)
		}
	})

	t.Run("no XFF falls back to RemoteAddr without port", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		r.RemoteAddr = "10.0.0.1:5678"
		if got := extractClientIP(r); got != "10.0.0.1" {
			t.Fatalf("extractClientIP = %q, want 10.0.0.1", got)
		}
	})
}

func TestPayloadContract(t *testing.T) {
	wantKeys := []string{
		"ip_address", "status_code", "path", "method", "user_agent",
		"referer", "fqdn", "query", "content_type", "accept_language",
		"x_request_id", "protocol",
	}

	t.Run("omitempty on optional fields", func(t *testing.T) {
		data, err := json.Marshal(reportPayload{IpAddress: "203.0.113.9", StatusCode: 500})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(m) != 2 {
			t.Fatalf("empty optionals must be omitted: got %v", m)
		}
		if m["ip_address"] != "203.0.113.9" || m["status_code"] != float64(500) {
			t.Fatalf("unexpected payload: %v", m)
		}
	})

	t.Run("every contract key present when populated", func(t *testing.T) {
		p := reportPayload{
			IpAddress: "203.0.113.9", StatusCode: 499, Path: "/a", Method: "POST",
			UserAgent: "ua", Referer: "ref", Fqdn: "example.com", Query: "q=1",
			ContentType: "json", AcceptLanguage: "es", XRequestId: "rid", Protocol: "https",
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range wantKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("missing contract key %q", k)
			}
		}
		if len(m) != len(wantKeys) {
			t.Errorf("got %d keys, want %d: %v", len(m), len(wantKeys), m)
		}
	})

	t.Run("buildPayload from request", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPut, "http://valet.example.com/boom?x=1&y=2", nil)
		r.RemoteAddr = "10.0.0.9:1234"
		r.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.9")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("User-Agent", "agent/1")
		r.Header.Set("Referer", "https://ref.example/")
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept-Language", "es-ES,es;q=0.9")
		r.Header.Set("X-Request-Id", "req-42")

		p := buildPayload(r, 503)
		want := reportPayload{
			IpAddress: "198.51.100.7", StatusCode: 503, Path: "/boom", Method: "PUT",
			UserAgent: "agent/1", Referer: "https://ref.example/", Fqdn: "valet.example.com",
			Query: "x=1&y=2", ContentType: "application/x-www-form-urlencoded",
			AcceptLanguage: "es-ES,es;q=0.9", XRequestId: "req-42", Protocol: "https",
		}
		if p != want {
			t.Fatalf("buildPayload = %+v, want %+v", p, want)
		}
	})

	t.Run("protocol falls back to http", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.com/x", nil)
		if p := buildPayload(r, 500); p.Protocol != "http" {
			t.Fatalf("protocol = %q, want http fallback", p.Protocol)
		}
	})
}
