# traefik-shuul-reporter

**Plugin de middleware para Traefik** que captura los códigos de estado HTTP
que devuelve el backend real y los reporta a **shuul** para un rate limiting
inteligente basado en respuesta, al estilo **fail2ban**.

---

## Problema que resuelve

shuul funciona como **ForwardAuth** de Traefik: decide si una request está
autorizada (200 OK) o bloqueada (403) **antes** de que llegue al backend.
Pero shuul **nunca ve la respuesta real del backend**.

Si un atacante pasa el filtro de shuul pero el backend responde con un
`401 Unauthorized` o `404 Not Found`, shuul no lo sabe y no puede
contabilizarlo para el rate limiting.

Este plugin es el puente que le falta a shuul: **intercepta la respuesta
del backend y la reporta**.

---

## Arquitectura

```
Cliente
   │
   ▼
Traefik
   │
   ├─ [ForwardAuth] ──→ shuul (200 OK / 403)
   │
   ├─ Backend real ──→ 401 / 403 / 404 / 429 / 500...
   │      │
   │      ▼
   ├─ [shuul-reporter]  ◄── captura status code + IP
   │      │
   │      ▼  POST async (fire-and-forget)
   │   shuul /api/v1/report
   │      │
   │      ▼  evalua: ¿status_code ∈ fail_codes del perfil?
   │      │
   │      ├─ Sí → rl.record(ip) → ban si excede threshold
   │      └─ No → ignora
   │
   └─ Cliente recibe la respuesta normalmente
```

### Flujo detallado

1. **Traefik** recibe la request del cliente
2. **ForwardAuth** (shuul) decide si deja pasar → 200 OK
3. **Backend real** procesa la request y responde (ej: 401)
4. **shuul-reporter** intercepta la respuesta, captura el status code
5. **Asíncronamente** envía un POST a `shuul:3000/api/v1/report` con
   `{ip_address, status_code, path, method}`
6. **shuul** recibe el reporte, matchea contra sus reglas activas,
   y si el status code está en `fail_codes` del perfil asociado,
   lo contabiliza en el rate limiter y banea si corresponde
7. **El cliente recibe la respuesta sin demora** — el reporte es
   fire-and-forget, no bloquea la respuesta

---

## Cómo funciona

El plugin wrappea el `http.ResponseWriter` de Go para interceptar el
código de estado que escribe el backend:

```go
type captureWriter struct {
    http.ResponseWriter
    statusCode  int
    wroteHeader bool
}

func (w *captureWriter) WriteHeader(code int) {
    if !w.wroteHeader {
        w.statusCode = code
        w.wroteHeader = true
    }
    w.ResponseWriter.WriteHeader(code)
}
```

Después de que el backend escriba la respuesta, el plugin extrae la IP
del cliente (de `X-Forwarded-For` o `RemoteAddr`) y lanza una gorutina
que hace un POST al endpoint de shuul.

Solo se reportan códigos **no exitosos** (status ≥ 300). Los 2xx se
ignoran porque no son relevantes para rate limiting.

---

## Configuración

### Opciones del plugin

| Parámetro | Tipo | Default | Descripción |
|---|---|---|---|
| `shuulUrl` | string | `http://shuul:3000/api/v1/report` | URL del endpoint de reporte de shuul |
| `timeoutMs` | int | `500` | Timeout del HTTP client para el reporte (ms) |
| `reportClientIP` | bool | `true` | Si es `true`, usa `X-Forwarded-For` para la IP del cliente; si es `false`, usa `RemoteAddr` |

### Instalación en Traefik

**1. Publicar el plugin** en GitHub (ej: `github.com/atareao/traefik-shuul-reporter`)

**2. Configurar Traefik** (`traefik.yml` o variables de entorno):

```yaml
experimental:
  plugins:
    shuul-reporter:
      moduleName: github.com/atareao/traefik-shuul-reporter
      version: v0.1.0
```

**3. Definir el middleware** (`dynamic.yml` o labels en Docker):

```yaml
http:
  middlewares:
    shuul-reporter:
      plugin:
        shuul-reporter:
          shuulUrl: "http://shuul:3000/api/v1/report"
          timeoutMs: 500
          reportClientIP: true
```

**4. Encadenar con ForwardAuth** — el orden es importante:

```yaml
http:
  routers:
    my-router:
      middlewares:
        - shuul-auth        # 1º ForwardAuth (shuul)
        - shuul-reporter    # 2º Plugin reporter (captura respuesta del backend)
```

### Con Docker Compose

```yaml
services:
  traefik:
    image: traefik:v3.1
    command:
      - "--experimental.plugins.shuul-reporter.modulename=github.com/atareao/traefik-shuul-reporter"
      - "--experimental.plugins.shuul-reporter.version=v0.1.0"
    labels:
      - "traefik.http.middlewares.shuul-reporter.plugin.shuul-reporter.shuulUrl=http://shuul:3000/api/v1/report"
      - "traefik.http.middlewares.shuul-reporter.plugin.shuul-reporter.timeoutMs=500"
      - "traefik.http.middlewares.shuul-reporter.plugin.shuul-reporter.reportClientIP=true"

  shuul:
    image: shuul:latest

  backend:
    image: tu-backend:latest
```

---

## Integración con shuul

### Configuración del perfil de rate limiting

En shuul, cada `RateLimitProfile` tiene un campo `fail_codes` que define
qué códigos de estado cuentan para el rate limiting:

| Perfil | fail_codes | Comportamiento |
|---|---|---|
| **Strict** | `[401, 403, 404, 429]` | 3 errores en 10 min → ban 24h |
| **Moderate** | `[401, 403, 404]` | 5 errores en 10 min → ban 1h |
| **Relaxed** | `[401, 403]` | 30 errores en 5 min → ban 30 min |
| **Scraping** | `[401, 403, 404, 429, 500]` | 60 errores en 1 min → ban 10 min |

### Reglas asociadas

Las reglas en shuul que matchean paths de login o paths sensibles deben
tener `allow = true` (dejar pasar al backend) pero con un
`rate_limit_profile_id` asignado. El plugin detectará los 401/403 del
backend real y shuul los contabilizará.

Ejemplo: proteger el wp-login.php

```json
{
  "name": "WordPress - wp-login",
  "path": "^/wp-login\\.php",
  "allow": true,
  "mode": "enforce",
  "rate_limit_profile_id": 2
}
```

Las requests a `/wp-login.php` pasan al backend, pero si el backend
responde 401 (login incorrecto), el plugin lo reporta y shuul cuenta
el intento fallido.

---

## Desarrollo

### Requisitos

- Go 1.21+

### Compilar

```bash
go build -o plugin .
```

### Probar localmente

El plugin sigue el estándar de plugins de Traefik. Puedes probarlo
en un entorno de desarrollo con Traefik en modo dev:

```bash
traefik --experimental.localPlugins.shuul-reporter.modulename=./traefik-shuul-reporter
```

---


## status-shim

> **Proxy inverso Go compilado** — captura los status ≥ 300 en la ruta de la
> respuesta y los reporta a shuul **sin romper el streaming SSE**. Es el
> sustituto del plugin `traefik-shuul-reporter` en rutas streaming: al ser Go
> compilado (no un plugin Yaegi interpretado), su `captureWriter` SÍ es visto
> como `http.Flusher` por el transporte → el SSE llega por chunk.

### Por qué existe

El plugin Yaegi `traefik-shuul-reporter` nunca expone `http.Flusher` al
transporte compilado de Traefik (ver AGENTS.md): el streaming SSE se entrega
en ráfagas. El usuario descartó explícitamente el parsing del access log
exige capturar el status **en la ruta de la
respuesta**, como hace el plugin, sin parsing de logs y con SSE intacto.

`status-shim` lo consigue siendo un **reverse proxy stdlib**
(`net/http/httputil.ReverseProxy`) entre Traefik y el backend: su wrapper de
`ResponseWriter` se compila con el binario, así que el transporte SÍ lo ve
como `http.Flusher` → flushea por chunk → SSE perfecto, y a la vez captura el
status → POST a shuul con el mismo contrato `ReportPayload` del plugin.

### Qué hace

- Proxy inverso al backend (`-backend=http://valet:3000`, URL **sin path** →
  se preservan path y query del request); `FlushInterval` sin configurar.
- `captureWriter` envuelve la ResponseWriter; en el **primer `WriteHeader`**,
  si el status ≥ `-min-status` (300 por defecto), encola el reporte
  inmediatamente (el status final se conoce ahí; un error a mitad de un stream
  SSE viaja como evento con status 200, no como cambio de status). `Write`
  sin header → 200 implícito.
- **Flusher/Hijacker/Pusher/ReaderFrom delegan al writer subyacente** (Go
  compilado, no Yaegi): SSE intacto y WebSockets (upgrade por `Hijack`) siguen
  funcionando.
- Cola con buffer `-queue` (1000) y `-workers` (4) goroutines que hacen POST
  con `http.Client{Timeout: -timeout-ms}`; cola llena → descarta + log (nunca
  bloquea el proxy).
- Shutdown graceful: SIGINT/SIGTERM → `http.Server.Shutdown` + drenar la cola
  de reportes (máx 3s), exit 0.
- Logging: stdlib `log`, prefijo `status-shim:`.
- Payload: mismo contrato JSON del plugin (`ip_address`, `status_code`, `path`,
  `method`, `user_agent`, `referer`, `fqdn`, `query`, `content_type`,
  `accept_language`, `x_request_id`, `protocol`; solo `ip_address` y
  `status_code` obligatorios, el resto `omitempty`). IP cliente de la primera
  entrada de `X-Forwarded-For` → fallback `RemoteAddr` sin puerto; protocol de
  `X-Forwarded-Proto` → fallback `http`.

### Flags (con fallback a env var homónima en mayúsculas)

| Flag | Env | Default | Descripción |
|---|---|---|---|
| `-listen` | `LISTEN` | `:8080` | Dirección de escucha |
| `-backend` | `BACKEND` | — (requerido) | URL del backend, p.ej. `http://valet:3000` |
| `-shuul-url` | `SHUUL_URL` | `http://shuul:3000/api/v1/report` | Endpoint de reporte de shuul |
| `-timeout-ms` | `TIMEOUT_MS` | `500` | Timeout del HTTP client de reporte (ms) |
| `-min-status` | `MIN_STATUS` | `300` | Reporta solo status ≥ este valor |
| `-workers` | `WORKERS` | `4` | Goroutines de POST |
| `-queue` | `QUEUE` | `1000` | Buffer de la cola de reportes |

### Wiring en Docker Compose (valet)

1. **Quita el middleware del plugin del entrypoint** de Traefik: fuera
   `--experimental.plugins.shuul-reporter.*` (y el label
   `traefik.http.middlewares.shuul-reporter.*`). Con el plugin presente, el
   SSE se entregaría en ráfagas.
2. El router de valet ya no apunta directamente al backend: apunta al
   servicio que expone el shim.
3. El shim proxya a valet con `-backend=http://valet:3000`.

```yaml
services:
  traefik:
    image: traefik:v3.2
    # command: sin --experimental.plugins.shuul-reporter.*
    labels:
      # El router de valet apunta al shim (que expone :8080) en lugar de al
      # puerto del backend:
      - "traefik.http.routers.valet.rule=Host(`valet.example.com`)"
      - "traefik.http.routers.valet.service=status-shim"
      - "traefik.http.services.status-shim.loadbalancer.server.port=8080"
      # sin middlewares de plugin shuul-reporter en este router
    networks: [app]

  status-shim:
    build:
      context: ./
      dockerfile: cmd/status-shim/Dockerfile
    restart: unless-stopped
    command:
      - "-listen=:8080"
      - "-backend=http://valet:3000"
      # opcionales (defaults sensatos):
      # - "-shuul-url=http://shuul:3000/api/v1/report"
      # - "-min-status=300"
      # - "-timeout-ms=500"
      # - "-workers=4"
      # - "-queue=1000"
    # environment:   # cada flag admite fallback a env var homónima en mayúsculas
    #   - LISTEN=:8080
    #   - BACKEND=http://valet:3000
    #   - SHUUL_URL=http://shuul:3000/api/v1/report
    #   - WORKERS=4
    #   - QUEUE=1000
    depends_on:
      - shuul
    networks: [app]

  valet:
    # el backend real; el tráfico le llega AHORA a través del shim
    networks: [app]

  shuul:
    image: shuul:latest
    networks: [app]

networks:
  app:
```

> Ajusta `context:`/`dockerfile:` al layout de tu compose; el punto clave es
> la cadena `Traefik → status-shim:8080 → valet:3000` y que el middleware del
> plugin ya **no** esté en el router de valet.

### Smoke test (streaming intacto)

Con un endpoint SSE en el backend (`/events`), pásalo por el shim y mira los
timestamps: cada chunk debe llegar **por separado** (~la cadencia del
backend), no en ráfagas.

```bash
# 1. Levanta el stack del compose de arriba.

# 2. Consume el stream a través de Traefik → status-shim, con timestamp por línea:
curl -N -s http://valet.example.com/events | while IFS= read -r line; do
  printf '%s  %s\n' "$(date +%H:%M:%S.%3N)" "$line"
done

# Esperado: un timestamp distinto por chunk, espaciado según el backend.
# Si en su lugar todo llega de golpe, el plugin Yaegi sigue activo en ese router.
```

> `curl -N` (sin buffering) + timestamps por línea: si la separación entre
> timestamps se parece a la cadencia del backend, el stream llega flusheado
> chunk a chunk. Si quieres apuntar el curl directamente al shim (sin
> Traefik), expón su puerto en el compose.

---

## Licencia

MIT
