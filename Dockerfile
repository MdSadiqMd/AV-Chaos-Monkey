# Build stage
FROM golang:1.22-alpine AS builder

# Since CGO_ENABLED=0, we don't need build-base (gcc) - saves time and space
RUN apk add --no-cache \
    git \
    ca-certificates

WORKDIR /app
COPY go.mod go.sum ./

# Allow Go to automatically download newer toolchain if needed
ENV GOTOOLCHAIN=auto

RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /orchestrator ./cmd/main.go

# Production stage
FROM alpine:latest

RUN apk add --no-cache \
    ca-certificates \
    dumb-init \
    curl \
    ffmpeg

# Create non-root user
RUN adduser -D -u 1000 app
USER app

COPY --from=builder /orchestrator /usr/local/bin/orchestrator

COPY --chown=app:app public /app/public

WORKDIR /app

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/bin/dumb-init", "--"]
# Set Go memory limits based on container size
# GOMEMLIMIT should be ~80% of container memory limit to allow for OS overhead
# Default: 5GiB for 6GB container, override with docker-compose environment variable
ENV GOMEMLIMIT=5GiB
ENV GOGC=100
CMD ["orchestrator", "-http", ":8080"]
