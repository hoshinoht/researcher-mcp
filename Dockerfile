FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/researcher-mcp ./cmd/google-scholar-mcp

FROM alpine:3.21
RUN adduser -D appuser
USER appuser
WORKDIR /app
COPY --from=builder /out/researcher-mcp /usr/local/bin/researcher-mcp
ENTRYPOINT ["/usr/local/bin/researcher-mcp"]
