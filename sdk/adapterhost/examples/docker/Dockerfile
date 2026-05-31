# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /criteria-adapter ./cmd/adapter

# Runtime stage
FROM alpine:3.21
RUN apk add --no-cache ca-certificates

COPY --from=builder /criteria-adapter /usr/local/bin/criteria-adapter

ENV CRITERIA_REMOTE_HOST="wss://criteria.example.com:7778"
ENV CRITERIA_REMOTE_TOKEN=""

ENTRYPOINT ["/usr/local/bin/criteria-adapter"]
