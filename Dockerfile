
FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . . 

RUN go build -o rlnode ./cmd/rlnode

FROM alpine:3.22

WORKDIR /app

COPY --from=builder /app/rlnode /rlnode


CMD ["/rlnode"]