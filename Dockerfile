# --- Stage 1: Build web UI ---
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY ui/ .
RUN go mod download
ARG APP_VERSION=dev
RUN CGO_ENABLED=0 go build -o vpn-gateway-ui -ldflags="-s -w -X main.version=${APP_VERSION}" .

# --- Stage 2: Runtime ---
# Pinned digest from 2026-02-23. To update: docker pull ghcr.io/hotio/base:alpinevpn && docker inspect --format '{{.RepoDigests}}' ghcr.io/hotio/base:alpinevpn
FROM ghcr.io/hotio/base@sha256:a3f171bc00b03218907c6bd6530e4231446def5ed2e8ebed53927ab2693919c7

LABEL maintainer="ProphetSe7en" \
      description="VPN gateway with nftables bandwidth limiting, scheduling, and web UI"

COPY root/ /
COPY traffic.conf.sample /traffic.conf.sample
COPY --from=builder /build/vpn-gateway-ui /vpn-gateway-ui

RUN chmod +x /usr/local/bin/nft-apply \
             /etc/s6-overlay/s6-rc.d/svc-traffic/run \
             /etc/s6-overlay/s6-rc.d/svc-traffic/finish \
             /etc/s6-overlay/s6-rc.d/svc-webui/run \
             /vpn-gateway-ui
