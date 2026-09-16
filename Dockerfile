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

# Download modules once (cached across builds unless manifests change)
RUN go mod download

# Copy the rest of the source
COPY . .

# Build for the target architecture with version injection
RUN GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o provider_bin ./cmd/provider/

# --- Final Stage ---
FROM alpine:latest

ARG TARGETARCH
ARG VERSION=v.unknown
WORKDIR /app

# Install runtime dependencies
RUN apk update && apk add --no-cache \
    tzdata iputils dos2unix \
    jq tar curl htop wget procps \
    bind-tools ca-certificates \
    ca-certificates-bundle bash \
  && rm -rf /var/cache/apk/*

# Setup directory structure
RUN mkdir -p /root/.urnetwork

# Copy the compiled binary
COPY --from=builder /app/provider_bin /app/provider

# Set permissions
RUN chmod +x /app/provider

# Expose the provider binary on PATH
RUN ln -sf /app/provider /usr/local/bin/provider

# Setup volumes
VOLUME ["/root/.urnetwork"]

ENTRYPOINT ["/app/provider"]
CMD ["provide"]
