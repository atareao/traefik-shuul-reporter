# PLAN — status-shim: captura de status ≥300 en la ruta de respuesta sin romper SSE

## Contexto / por qué

El plugin Yaegi `traefik-shuul-reporter` no puede convivir con SSE: en el límite interpretado→compilado, Yaegi (v0.16.1, el de Traefik v3.2) adapta el `*captureWriter` al tipo estático `http.ResponseWriter` y el transporte compilado de Traefik no lo ve como `http.Flusher` (verificado con harness local replicando `middlewareyaegi.go`). Resultado: el transporte no puede flushear → streaming en ráfagas. Imposible de arreglar dentro de un plugin Yaegi.

El usuario rechaza explícitamente la alternativa de parsear el access log (fragilidad de formato, headers condicionales, IP distinta, latencia). Requisito: capturar el status en la ruta de respuesta, como el plugin, sin parsing de logs y con SSE intacto.

Solución elegida: **shim compilado (Go, no Yaegi)** entre Traefik y el backend. Es un reverse-proxy stdlib (`net/http/httputil.ReverseProxy`) cuyo wrapper de ResponseWriter SÍ es visto como `http.Flusher` (Go compilado) → flushea por chunk → SSE llega perfecto, y captura el status → POST a shuul con el mismo contrato `ReportPayload`. Sustituye al plugin para valet.

## Objetivo

Nuevo binario `cmd/status-shim` en este repo (Go 1.21+, SOLO stdlib, cero dependencias). Recibe el tráfico que Traefik le envía como servicio backend de valet, lo proxya a valet, captura el status de la respuesta, reporta ≥ `-min-status` (default 300) a shuul al momento del `WriteHeader` (el status final se conoce ahí; un error a mitad de un stream SSE viaja como evento con status 200, no como cambio de status). El paquete raíz (plugin Yaegi) NO se toca.

## Arquitectura

```
Cliente → Traefik [sin plugin en el entrypoint] → status-shim (ReverseProxy + captureWriter compilado) → valet
                                                     └─ status ≥ min-status → POST /api/v1/report → shuul
```

- `cmd/status-shim/main.go` — flags/env, http.Server, pipeline de reporte (cola + workers), shutdown graceful + drain.
- `cmd/status-shim/proxy.go` — ReverseProxy (`httputil.NewSingleHostReverseProxy` con URL backend sin path → preserva path y query), director por defecto (Traefik ya puso X-Forwarded-For/Proto/Host; el proxy puede añadir el peer local al final de XFF — inofensivo, se extrae la PRIMERA entrada igual que el plugin).
- `cmd/status-shim/capture.go` — `captureWriter` (misma lógica que el plugin) + **Flusher/Hijacker/Pusher/ReaderFrom delegando** — en Go compilado las aserciones funcionan (este es el punto central). Reporte disparado en el primer `WriteHeader` con status ≥ min-status.
- `cmd/status-shim/payload.go` — struct `reportPayload` con EXACTAMENTE las claves del contrato del plugin (ip_address, status_code, path, method, user_agent, referer, fqdn, query, content_type, accept_language, x_request_id, protocol; omitempty en opcionales) + `extractClientIP` (XFF primera entrada → fallback RemoteAddr sin puerto) + builders de payload desde el request.
- Tests colocalizados (stdlib: httptest).

## Flags (con fallback a env var homónima en mayúsculas)

- `-listen` (LISTEN) default `:8080`
- `-backend` (BACKEND) **requerido**, p.ej. `http://valet:3000` (validar URL con scheme http/https)
- `-shuul-url` (SHUUL_URL) default `http://shuul:3000/api/v1/report`
- `-timeout-ms` (TIMEOUT_MS) default 500
- `-min-status` (MIN_STATUS) default 300
- `-workers` (WORKERS) default 4
- `-queue` (QUEUE) default 1000

## Comportamiento

- ReverseProxy estándar: `FlushInterval` sin configurar → al detectar `Flusher` en el writer destino flushea tras cada write → stream del backend (SSE chunked) se reenvía al instante. WebSockets: el Hijacker delegado permite el upgrade.
- Reporte: `captureWriter.WriteHeader` (primer aviso) → si status ≥ min-status, encolar payload (IP de XFF/RemoteAddr, método, path, query, host→fqdn, protocol de `X-Forwarded-Proto`, headers User-Agent/Referer/Content-Type/Accept-Language/X-Request-Id).
- Cola con buffer `-queue`; `-workers` goroutines con `http.Client{Timeout}`; cola llena → descartar + log (no bloquear el proxy).
- SIGINT/SIGTERM: `http.Server.Shutdown` + drenar cola (máx 3s).
- Logging: stdlib `log`, prefijo `status-shim:`.

## Tests

1. `TestCaptureWriterFlusher` — **la aserción `captureWriter.(http.Flusher)` debe ser TRUE** (Go compilado; el test que el plugin Yaegi no podía pasar) y `Flush()` delega al writer subyacente (contador).
2. `TestCaptureStatus` — primer WriteHeader captura y solo él; Write sin WriteHeader → 200 implícito; status < min-status no encola; WebSocket upgrade (Hijack delega).
3. `TestExtractClientIP` — XFF multi-hop → primera entrada; sin XFF → RemoteAddr sin puerto.
4. `TestPayloadContract` — claves JSON exactas del contrato shuul; omitempty en vacíos.
5. `TestEndToEndSSE` — httptest backend que emite 3 chunks SSE con sleeps y status 200; httptest shuul; pasar por el handler del shim con un recorder con Flusher → verificar: (a) los 3 chunks llegan y cada `Flush()` del recorder fue llamado (streaming intacto); (b) ninguna llamada a shuul (200 no se reporta).
6. `TestEndToEndReport` — backend con status 500 → llega 1 POST con payload correcto; shutdown graceful drena cola (segunda petición 500 con handler de shuul lento → se drena tras cancelar).

## Entregables

- `cmd/status-shim/{main,proxy,capture,payload}.go` + tests + `Dockerfile` (multi-stage golang:1.21-alpine CGO_ENABLED=0 → scratch).
- `README.md`: nueva sección "status-shim" con el wiring de compose: quitar `shuul-reporter` del entrypoint; el servicio de valet en Traefik apunta a `http://status-shim:8080`; el shim usa `-backend=http://valet:3000`; contenedor `status-shim` en la red del compose. Mantener el resto del README.
- `AGENTS.md`: documentar el shim como el sustituto streaming-safe del plugin; mantener la nota de la limitación Yaegi; marcar `cmd/accesslog-reporter` como superseded (pendiente de decisión del usuario sobre su borrado).

## Criterios de aceptación

- `gofmt -l .` vacío, `go vet ./...` limpio, `go test ./...` verde (incluido `-race`).
- El paquete raíz (plugin) sigue compilando como librería.
- Cero dependencias externas; go.mod sin requires nuevos.
- Smoke test manual documentado en README: `curl -N` con timestamps a través del shim contra un backend SSE → llegada por chunk (~cadencia del backend), no en ráfagas.