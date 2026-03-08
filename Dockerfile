# syntax=docker/dockerfile:1

# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

# TARGETARCH is set automatically by Docker Buildx for multi-arch builds
# (e.g. docker buildx build --platform linux/amd64,linux/arm64).
ARG TARGETARCH

WORKDIR /app

# Cache dependency download layer separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a static binary for the target architecture with debug info stripped
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /app/server ./cmd/server

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
# distroless/static has no shell, no libc, no package manager — minimal attack surface.
# nonroot variant runs as uid 65532 by default.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /app/server /server

EXPOSE 8080

# OTel and Pyroscope are configured via environment variables.
# See README.md for the full reference.
ENTRYPOINT ["/server"]
