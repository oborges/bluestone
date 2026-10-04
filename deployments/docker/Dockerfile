# Multi-stage build for Bluestone

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.22

# Build stage. It runs on the build host's own architecture and cross-compiles
# for the image's, so building for another platform needs no emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

# Dependencies first, so they are cached across source changes. go.mod
# replaces two modules with the copies under third_party, which therefore
# have to be present before anything is resolved.
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags="-w -s -X main.Version=${VERSION}" \
    -o bluestone \
    ./cmd/bluestone

# Runtime stage
FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    && addgroup -g 1000 bluestone \
    && adduser -D -u 1000 -G bluestone bluestone

WORKDIR /app

COPY --from=builder /build/bluestone .

# Non-secret example configuration as the runtime default.
COPY --from=builder /build/configs/config.example.yaml /etc/bluestone/config.yaml

# The gateway writes its read cache and its staging area; staging holds
# accepted writes until they reach COS, so mount a volume there.
RUN mkdir -p /var/cache/bluestone /var/staging/bluestone && \
    chown -R bluestone:bluestone /var/cache/bluestone /var/staging/bluestone /app

USER bluestone

# NFS, SMB (when smb.enabled), metrics and health.
EXPOSE 2049 445 8080 8081

# Needs server.health_enabled: true. The health server listens on 127.0.0.1
# only, and "localhost" resolves to ::1 first here, so name the address.
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8081/health/live || exit 1

ENTRYPOINT ["./bluestone"]

CMD ["--config", "/etc/bluestone/config.yaml"]
