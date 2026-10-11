# AGENTS.md — traefik-shuul-reporter

**Go Traefik middleware plugin** that captures backend HTTP status codes and
reports non-2xx responses to **shuul** for fail2ban-style rate limiting.

---

## Language & toolchain

- **Go 1.21+** — this is a Go project, not Rust (despite the parent directory name).
- **Zero external dependencies** — pure stdlib (`net/http`, `encoding/json`, `fmt`).
- No `go.sum` file (no external deps to pin).
- No tests exist; any test work starts from scratch.

## Build & verify

This is a **library** (a Traefik plugin), not a binary — `go build -o plugin .` will
fail with "function main is undeclared" and that is **expected**. Verify with:

```bash
go vet ./...
go test ./...
```

Do NOT add `func main()` back to make `go build` pass — it breaks Yaegi.

## CRITICAL: package name (Yaegi)

```go
package traefik_shuul_reporter   // NOT package main
```

Traefik compiles local plugins with the **Yaegi interpreter**, which registers a
package by its `package` clause. Traefik then evals `<basepkg>.New` where `basepkg`
is derived from the module path with `-` → `_`. If the file says `package main`,
Yaegi registers it as `main` and Traefik fails with:

```
failed to eval New: 1:28: undefined: traefik_shuul_reporter
```

This error is byte-identical across every code variant that keeps `package main` —
it is NOT fixed by changing imports, removing main(), or simplifying code.

`.traefik.yml` must also declare `basepkg: traefik_shuul_reporter`.

## Plugin contract (Traefik middleware)

`main.go` must export three symbols Traefik discovers via reflection:

| Symbol | Signature |
|---|---|
| `New` | `func(context.Context, http.Handler, *Config, string) (http.Handler, error)` |
| `Config` | struct with JSON-tagged fields (`shuulUrl`, `timeoutMs`, `reportClientIP`) |
| `CreateConfig` | `func() *Config` returning defaults |

Do not rename or change signatures — Traefik will fail to load the plugin.

## Manifest

`.traefik.yml` is the plugin manifest. Requires: `type: middleware`, `import`,
`basepkg`, `displayName`, `summary`, `testData`.

## Architecture (single file)

Everything lives in `main.go` (~170 lines):

1. **`captureWriter`** — wraps `http.ResponseWriter`, intercepts `WriteHeader` to grab status code.
2. **`ShuulPlugin.ServeHTTP`** — calls next handler, then async-POSTs non-2xx codes to shuul.
3. **`sendReport`** — fire-and-forget goroutine; errors silently dropped.
4. **`extractClientIP`** — prefers `X-Forwarded-For` (first IP) or falls back to `RemoteAddr` (strips port).

Only status codes ≥ 300 are reported; 2xx is silently skipped.

## Installation on the user's Traefik (operational gotchas)

- **Must use `experimental.localPlugins`, NOT `experimental.plugins`.** The remote
  registry (`plugins` key) only downloads from plugins.traefik.io; this plugin is not
  registered there, so remote gives `404: Unknown plugin` regardless of GitHub tags.
- Traefik reads the mounted copy at
  `/plugins-local/src/github.com/atareao/traefik-shuul-reporter/` — a **bind mount
  does not self-update**. After code changes, the mounted dir must be refreshed
  (git pull / copy) or Traefik keeps loading the stale code with the same error.
- A single failing plugin disables **all** plugins → every plugin-based middleware
  (`my-torblock`, `my-geoblock`, `oidc-auth`, …) errors with "invalid middleware
  type or middleware does not exist" and everything returns 404. Fixing one plugin
  restores all of them.

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

## Component: status-shim

`cmd/status-shim` is a compiled Go reverse proxy (package `main`, Go 1.21,
stdlib only — zero external deps, no new entries in `go.mod`) that sits
between Traefik and a backend and replaces the Yaegi plugin on
streaming/SSE routes. It is the streaming-safe, response-path capture the
user chose over access-log parsing (fragile format, conditional headers,
different IP, latency).

- Proxy: `httputil.NewSingleHostReverseProxy` with a backend URL **without
  path** → the original request path/query are preserved. `FlushInterval`
  left at default.
- Capture: `captureWriter` (same interception logic as the plugin) wraps the
  `ResponseWriter` and, on the FIRST `WriteHeader`, enqueues a report when
  status ≥ `-min-status` (default 300); `Write` without a header implies
  200. **Flusher/Hijacker/Pusher/ReaderFrom delegate to the underlying
  writer — this is compiled Go, so `w.(http.Flusher)` succeeds and the
  reverse proxy flushes per chunk → SSE streams arrive intact, and
  WebSocket upgrades still work.**
- Pipeline: captureWriter → buffered channel (`-queue`, default 1000) →
  `-workers` (default 4) goroutines POSTing with
  `http.Client{Timeout: -timeout-ms}` (default 500ms). A full queue drops
  reports (logged), never blocks the proxy.
- Shutdown: SIGINT/SIGTERM → `http.Server.Shutdown` + drain the queue
  (max 3s), exit 0.
- Logging: stdlib `log` with prefix `status-shim:`.
- Payload: same `reportPayload` JSON contract as the plugin (`ip_address`,
  `status_code`, `path`, `method`, `user_agent`, `referer`, `fqdn`, `query`,
  `content_type`, `accept_language`, `x_request_id`, `protocol`; only
  `ip_address`/`status_code` required, rest `omitempty`). Client IP from
  X-Forwarded-For first entry → fallback RemoteAddr without port; protocol
  from X-Forwarded-Proto → fallback `http`.

Flags fall back to env vars of the same name in upper case (`-listen` ↔
`LISTEN` (default `:8080`), `-backend` ↔ `BACKEND` (required),
`-shuul-url` ↔ `SHUUL_URL`, `-timeout-ms` ↔ `TIMEOUT_MS`,
`-min-status` ↔ `MIN_STATUS`, `-workers` ↔ `WORKERS`, `-queue` ↔ `QUEUE`).

Build and verify (same commands cover the whole repo, plugin included):

```bash
go build ./cmd/status-shim     # the binary; `go build .` fails (library) and that is expected
go vet ./...
go test ./...                  # unit + integration tests, all stdlib
```

## Git

- Binaries `shuul-reporter` and `plugin` are gitignored.
- `v0.2.0` tag tracks the `main` branch head (force-pushable tag); keep it in sync.
- No CI/CD workflows exist.

---

## CRITICAL: Yaegi never exposes http.Flusher to Traefik's compiled transport

When a `*captureWriter` crosses the interpreted→compiled boundary, Yaegi
(v0.16.1, the one Traefik v3.2 ships) adapts it to the static
`http.ResponseWriter` type and **the compiled Traefik transport no longer sees
it as `http.Flusher`** (verified with a local harness replicating
`middlewareyaegi.go`). Consequence: the transport cannot flush → SSE/streaming
responses are delivered in bursts, regardless of any `Flush()` method defined
on the wrapper. An A/B test in production confirmed it: without the plugin the
stream trickles, with the plugin (even with a Flush fix) it bursts.

**Rule: do NOT use the Yaegi plugin on streaming/SSE routes.** For those
routes use `cmd/status-shim` instead (see below): a compiled Go reverse
proxy whose `captureWriter` IS seen as `http.Flusher`, so SSE streaming
stays intact while still capturing the status code on the response path
(the user rejected access-log parsing as an alternative).
