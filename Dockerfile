# ---- Build stage ----
FROM golang:1.24-alpine AS build
WORKDIR /src

# Speed up `go mod download`
RUN apk add --no-cache git

# Copy and download deps
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Static build (no CGO), small binary
RUN GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/pingo

# ---- Runtime stage ----
FROM alpine:3.20
# Root user by default (needed for raw ICMP inside container)
WORKDIR /app

# Certificates for HTTPS to signal REST
RUN apk add --no-cache ca-certificates tzdata curl jq

# Optional: set timezone to match your logs
ENV TZ=Europe/Berlin

# Copy binary
COPY --from=build /out/pingo /usr/local/bin/pingo

# nsswitch to make Alpine DNS behave like glibc (better host lookups)
RUN printf 'hosts: files dns\n' > /etc/nsswitch.conf

# Default config path (can be overridden with CONFIG env)
ENV CONFIG=/config/config.yaml

# Entry
ENTRYPOINT ["/usr/local/bin/pingo"]
