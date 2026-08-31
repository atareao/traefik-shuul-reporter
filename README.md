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

## Licencia

MIT
