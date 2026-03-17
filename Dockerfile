# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install git and ca-certificates for HTTPS
RUN apk add --no-cache git ca-certificates

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o http-proxy .

# Final stage
FROM alpine:3.19

WORKDIR /app

# Install ca-certificates for HTTPS
RUN apk --no-cache add ca-certificates

# Copy binary from builder
COPY --from=builder /app/http-proxy /app/http-proxy

# Copy default config
COPY config.json /app/config.json

# Create non-root user
RUN adduser -D -u 1000 proxyuser
USER proxyuser

# Expose proxy port
EXPOSE 8080

# Run the proxy
ENTRYPOINT ["/app/http-proxy"]
CMD ["-config", "/app/config.json"]
