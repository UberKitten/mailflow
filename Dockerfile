# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o mailflow ./cmd/mailflow

# Runtime stage
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata bash wget

WORKDIR /app

COPY --from=builder /app/mailflow /app/mailflow

# Config directory
VOLUME /config

ENTRYPOINT ["/app/mailflow", "--config-dir=/config"]
CMD ["process", "--watch"]
