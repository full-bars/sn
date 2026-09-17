# --- Build Stage ---
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETARCH
ARG VERSION=v.unknown
WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

# Copy module manifests first so dependency download is a separate
# cacheable layer (only invalidated when go.mod/go.sum change)
COPY go.mod go.sum ./

# Local replace targets (third_party/npipe) must exist before go mod download
COPY third_party/ ./third_party/

# Download modules once (cached across builds unless manifests change)
RUN go mod download

# Copy the rest of the source
COPY . .

# Build all three binaries for the target architecture with version injection
RUN GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.VersionStamp=URNET_VERSION_STAMP=${VERSION}" \
    -o provider_bin ./cmd/provider/ \
    && go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.VersionStamp=URNET_VERSION_STAMP=${VERSION}" \
    -o urnet_tools_bin ./cmd/urnet-tools/ \
    && go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.VersionStamp=URNET_VERSION_STAMP=${VERSION}" \
    -o urnet_docker_bin ./cmd/urnet-docker/

# --- Final Stage ---
FROM alpine:latest

ARG TARGETARCH
ARG VERSION=v.unknown
WORKDIR /app

# Install runtime dependencies (including vnStat, networking and diagnostic tools)
RUN apk update && apk add --no-cache \
    tzdata iputils vnstat dos2unix \
    jq tar curl htop wget procps \
    iptables net-tools bind-tools \
    busybox-extras ca-certificates \
    ca-certificates-bundle bash \
    gosu \
  && rm -rf /var/cache/apk/*

# Setup directory structure
RUN mkdir -p /app/cgi-bin /root/.urnetwork

# Copy scripts from docker/scripts folder
COPY docker/scripts/*.sh /app/
COPY docker/scripts/stats /app/cgi-bin/

# Copy compiled binaries
COPY --from=builder /app/provider_bin /app/urnetwork_${TARGETARCH}_stable
COPY --from=builder /app/urnet_tools_bin /usr/local/bin/urnet-tools
COPY --from=builder /app/urnet_docker_bin /usr/local/bin/urnet-docker

# Set permissions
RUN dos2unix /app/*.sh /app/cgi-bin/stats && chmod +x /app/*.sh /app/cgi-bin/stats

# Expose helper tools on PATH
RUN ln -sf /app/proxy-health.sh /usr/local/bin/proxy-health
RUN ln -sf /app/proxy-traffic.sh /usr/local/bin/proxy-traffic
RUN ln -sf /app/logs.sh /usr/local/bin/logs
# Shell wrapper kept as fallback for Docker-specific update logic
RUN ln -sf /app/urnet-tools.sh /usr/local/bin/urnet-tools-sh

# update_verify.sh is sourced by urnet-tools for digest verification
RUN ln -sf /app/update_verify.sh /usr/local/bin/update_verify.sh

# Expose the provider binary on PATH as `provider` for `docker exec <c> provider <cmd>`
RUN ln -sf /app/urnetwork_${TARGETARCH}_stable /usr/local/bin/provider

# Configure vnStat
RUN sed -i \
  -e 's/^;*TimeSyncWait.*/TimeSyncWait 1/' \
  -e 's/^;*TrafficlessEntries.*/TrafficlessEntries 1/' \
  -e 's/^;*UpdateInterval.*/UpdateInterval 15/' \
  -e 's/^;*PollInterval.*/PollInterval 15/' \
  -e 's/^;*SaveInterval.*/SaveInterval 1/' \
  -e 's/^;*UnitMode.*/UnitMode 1/' \
  -e 's/^;*RateUnit.*/RateUnit 0/' \
  -e 's/^;*RateUnitMode.*/RateUnitMode 0/' \
  /etc/vnstat.conf

# Setup volumes
VOLUME ["/root/.urnetwork"]

ENTRYPOINT ["/app/entrypoint.sh"]
