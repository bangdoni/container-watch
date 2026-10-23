# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.27-alpine AS build

ARG VERSION=1.0.0
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Dependencies first so the module cache survives source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

# CGO is disabled so the resulting binary is fully static and runs on a
# scratch-like base image. The SQLite driver (modernc.org/sqlite) is pure Go.
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/bangdoni/container-watch/internal/version.Version=${VERSION} \
        -X github.com/bangdoni/container-watch/internal/version.Commit=${COMMIT} \
        -X github.com/bangdoni/container-watch/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/containerwatch ./cmd/containerwatch

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM alpine:3.22

# ca-certificates: TLS verification for remote Docker/Podman nodes.
# tzdata: correct timestamps in logs and the UI.
# su-exec: drop privileges from the entrypoint when the socket needs it.
RUN apk add --no-cache ca-certificates tzdata su-exec \
 && addgroup -g 1000 containerwatch \
 && adduser -u 1000 -G containerwatch -H -D -s /sbin/nologin containerwatch \
 && mkdir -p /data \
 && chown -R containerwatch:containerwatch /data

COPY --from=build /out/containerwatch /usr/local/bin/containerwatch

# The data directory holds the SQLite database and the generated session
# secret; mount a volume to keep them across upgrades.
VOLUME ["/data"]
ENV CW_ADDR=":8080" \
    CW_DATA_DIR="/data" \
    CW_LOG_LEVEL="info"

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/containerwatch", "-healthcheck"]

# The binary itself is static and unprivileged; running as a known UID lets the
# operator grant socket access with `--group-add <docker-gid>` (the container
# then keeps the containerwatch user's supplementary groups).
USER containerwatch

ENTRYPOINT ["/usr/local/bin/containerwatch"]
