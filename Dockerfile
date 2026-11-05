FROM node:22-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /vpn-monitor ./cmd/vpn-monitor

# The official core includes QUIC, uTLS and Naive support, including its runtime libraries.
FROM ghcr.io/sagernet/sing-box:v1.14.4
WORKDIR /app
COPY --from=go-build /vpn-monitor /usr/local/bin/vpn-monitor
COPY --from=web-build /src/web/dist ./web/dist
RUN mkdir -p /app/data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/vpn-monitor"]
CMD ["--config", "/app/config.yaml", "--listen", "0.0.0.0:8080"]
