# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled: modernc.org/sqlite is pure Go, so we get a static binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway cmd/gateway/main.go

# ponytail: alpine not scratch - need ca-certificates for HTTPS to openrouter.ai
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 app && \
    mkdir -p /data && chown app:app /data
USER app
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
ENV DB_PATH=/data/gateway.db \
    LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
