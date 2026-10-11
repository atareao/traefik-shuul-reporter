# syntax=docker/dockerfile:1
#
# Multi-stage build for the status-shim binary.
#
# Build context: repository root (docker build -f cmd/status-shim/Dockerfile .).
# Only the pieces this binary needs are copied: go.mod + cmd/. The root
# package (the Yaegi plugin) is deliberately NOT copied — it is a library
# package, not required here, and importing the whole tree would just slow
# the build.
#
# Runtime: scratch. No CA certificates on purpose: both the backend and shuul
# are only reachable over internal http (no TLS), so the cert pool would be
# dead weight.

# ---- build stage ----
FROM golang:1.21-alpine AS build
WORKDIR /src

COPY go.mod ./
COPY cmd/ ./cmd/

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/status-shim \
    ./cmd/status-shim

# ---- runtime stage ----
FROM scratch
COPY --from=build /out/status-shim /status-shim
ENTRYPOINT ["/status-shim"]