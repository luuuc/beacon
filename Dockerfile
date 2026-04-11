# syntax=docker/dockerfile:1.7
#
# Beacon — minimal production image.
#
# Two stages:
#   1. builder   — golang:1.25-alpine, compiles a fully static binary
#                  (CGO_ENABLED=0, modernc.org/sqlite is pure Go)
#   2. runtime   — alpine:3.20, non-root user, HTTP healthcheck via wget
#
# Build:
#   docker build -t beacon:dev .
#   docker build -t beacon:dev --build-arg VERSION=v0.1.0 .
#
# Run (SQLite, zero-config):
#   docker run --rm \
#     -p 127.0.0.1:4680:4680 \
#     -e BEACON_AUTH_TOKEN=$(openssl rand -hex 32) \
#     -v beacon_data:/var/lib/beacon \
#     ghcr.io/luuuc/beacon:latest
#
# Run (Postgres):
#   docker run --rm \
#     -p 127.0.0.1:4680:4680 \
#     -e BEACON_AUTH_TOKEN=$(openssl rand -hex 32) \
#     -e BEACON_DATABASE_URL=postgres://user:pass@host:5432/beacon \
#     -e BEACON_DATABASE_SCHEMA=beacon \
#     ghcr.io/luuuc/beacon:latest

# ----------------------------------------------------------------------------
# Stage 1 — builder
# ----------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

# VERSION is injected at build time and stamped into internal/version.Version
# via -ldflags. Defaults to "dev" for local builds.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Pre-copy go.mod + go.sum so dependency download is cacheable independently
# of source changes — 99% of edits don't touch go.mod and this layer stays hot.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# Static, stripped, reproducible-ish build. -trimpath drops absolute paths
# from the binary; -s -w drop the symbol and DWARF tables (~30% smaller).
# CGO_ENABLED=0 forces pure-Go linkage so the image has no libc dependency
# beyond whatever alpine ships.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build \
      -trimpath \
      -ldflags="-s -w -X github.com/luuuc/beacon/internal/version.Version=${VERSION}" \
      -o /out/beacon \
      ./cmd/beacon

# ----------------------------------------------------------------------------
# Stage 2 — runtime
# ----------------------------------------------------------------------------
FROM alpine:3.20

# wget is in busybox but we add ca-certificates so Beacon can reach a
# managed Postgres over TLS, and tzdata so rollup.timezone can be anything
# other than UTC without surprises.
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 10001 -S beacon && \
    adduser  -u 10001 -S -G beacon -h /var/lib/beacon beacon && \
    mkdir -p /var/lib/beacon /etc/beacon && \
    chown -R beacon:beacon /var/lib/beacon /etc/beacon

COPY --from=builder /out/beacon /usr/local/bin/beacon

# Default config: binds 0.0.0.0 so `docker run -p` works, SQLite on the
# mounted volume, sensible retention. The user MUST still set
# BEACON_AUTH_TOKEN — Validate() refuses non-loopback bind without it,
# which is the intended fail-loud-at-boot behavior.
COPY docker/beacon.default.yml /etc/beacon/beacon.yml

VOLUME ["/var/lib/beacon"]

EXPOSE 4680 4681

USER beacon

# HTTP healthcheck against /healthz. Busybox wget ships in alpine so this
# is zero extra weight. --spider does not download the body.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q --spider http://127.0.0.1:4680/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/beacon"]
CMD ["serve", "--config", "/etc/beacon/beacon.yml"]

# ----------------------------------------------------------------------------
# OCI labels — source of truth is the build args / workflow.
# ----------------------------------------------------------------------------
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="beacon" \
      org.opencontainers.image.description="The small observability accessory for self-hosted apps." \
      org.opencontainers.image.url="https://github.com/luuuc/beacon" \
      org.opencontainers.image.source="https://github.com/luuuc/beacon" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="OSaasy" \
      org.opencontainers.image.authors="Luc B. Perussault-Diallo"
