# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w -X main.version=container" \
    -o /out/llmtap \
    ./cmd/llmtap

FROM alpine:3.21

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN addgroup -S -g 10001 llmtap \
    && adduser -S -D -H -u 10001 -G llmtap llmtap \
    && mkdir -p /data \
    && chown llmtap:llmtap /data

COPY --from=build /out/llmtap /usr/local/bin/llmtap

VOLUME ["/data"]
EXPOSE 17878 17879
USER llmtap:llmtap

ENTRYPOINT ["/usr/local/bin/llmtap"]
CMD ["--data-dir", "/data", "start", "--forward-proxy", "0.0.0.0:17878", "--forward-admin", "0.0.0.0:17879"]
