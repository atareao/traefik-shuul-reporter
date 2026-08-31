# AGENTS.md — traefik-shuul-reporter

**Go Traefik middleware plugin** that captures backend HTTP status codes and
reports non-2xx responses to **shuul** for fail2ban-style rate limiting.

---

## Language & toolchain

- **Go 1.21+** — this is a Go project, not Rust (despite the parent directory name).
- **Zero external dependencies** — pure stdlib (`net/http`, `encoding/json`, `log`).
- No `go.sum` file (no external deps to pin).
- No tests exist; any test work starts from scratch.

## Build & run

```bash
go build -o plugin .
```

## Plugin contract (Traefik middleware)

The file `main.go` must export three symbols Traefik discovers via reflection:

| Symbol | Signature |
|---|---|
| `New` | `func(context.Context, http.Handler, *Config, string) (http.Handler, error)` |
| `Config` | struct with JSON-tagged fields (`shuulUrl`, `timeoutMs`, `reportClientIP`) |
| `CreateConfig` | `func() *Config` returning defaults |

Do not rename or change signatures — Traefik will fail to load the plugin.

## Manifest

`.traefik.yml` is the plugin manifest for Traefik's plugin registry (`type: middleware`).

## Architecture (single file)

Everything lives in `main.go` (~190 lines):

1. **`captureWriter`** — wraps `http.ResponseWriter`, intercepts `WriteHeader` to grab status code.
2. **`ShuulPlugin.ServeHTTP`** — calls next handler, then async-POSTs non-2xx codes to shuul.
3. **`sendReport`** — fire-and-forget goroutine; logs errors but never blocks the response.
4. **`extractClientIP`** — prefers `X-Forwarded-For` (first IP) or falls back to `RemoteAddr` (strips port).

Only status codes ≥ 300 are reported; 2xx is silently skipped.

## Key config defaults

| Field | Default |
|---|---|
| `shuulUrl` | `http://shuul:3000/api/v1/report` |
| `timeoutMs` | `500` |
| `reportClientIP` | `true` |

## Testing

No tests exist. If adding tests:
- Use stdlib `net/http/httptest` (no external test framework needed).
- Test `extractClientIP` with/without `X-Forwarded-For`.
- Test `captureWriter` status code interception.
- Test `sendReport` with `httptest.NewServer`.
- Test `ServeHTTP` with a stub backend handler.

## Git

- Binary `shuul-reporter` is gitignored.
- No CI/CD workflows exist.