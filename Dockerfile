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

RUN apk --no-cache add ca-certificates tzdata bash wget curl jq openssh-client

WORKDIR /app

COPY --from=builder /app/mailflow /app/mailflow
COPY --from=builder /app/scripts /app/scripts

# Config directory
VOLUME /config

ENTRYPOINT ["/app/mailflow", "--config-dir=/config"]
CMD ["webhook"]
