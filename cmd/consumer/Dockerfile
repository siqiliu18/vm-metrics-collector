# Stage 1 — build
FROM golang:1.25-alpine AS builder
WORKDIR /app

# download dependencies first (cached layer unless go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# copy source and build
COPY . .
RUN go build -o consumer ./cmd/consumer/

# Stage 2 — run
FROM alpine:3.21
WORKDIR /app

COPY --from=builder /app/consumer .

ENTRYPOINT ["./consumer"]
