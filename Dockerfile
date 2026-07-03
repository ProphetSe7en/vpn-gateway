# --- Stage 1: Build web UI ---
FROM golang:1.25.9-alpine AS builder
WORKDIR /build
COPY ui/ .
RUN go mod download
ARG APP_VERSION=1.4.4
RUN CGO_ENABLED=0 go build -o vpn-gateway-ui -ldflags="-s -w -X main.Version=${APP_VERSION}" .

# --- Stage 2: Runtime ---
# hotio/base:alpinevpn digest pinned for reproducible builds.
# Version mapping (see README § Base Image History):
#   v1.2.x - v1.3.x → 2026-02-23 (a3f171bc…) — Alpine 3.21
#   v1.4.0 - v1.4.2 → 2026-04-17 (f34cfccf…) — Alpine 3.21→3.22 refresh + service-pia / service-healthcheck 4-space indent fix
#   v1.4.3          → 2026-05-17 (38fffcf0…) — Alpine 3.23, s6-overlay 3.2.3.0
#   v1.4.4+         → 2026-06-30 (496a867c…) — Alpine 3.23.5 point release; OpenSSL 3.5.7, expat 2.8.2, ca-certificates security patches. nftables / wireguard-tools / iproute2 / s6-overlay unchanged.
# To check current upstream: docker pull ghcr.io/hotio/base:alpinevpn && docker inspect --format '{{.RepoDigests}}' ghcr.io/hotio/base:alpinevpn
FROM ghcr.io/hotio/base@sha256:496a867ce1c643896313654132f9a87dfea97317843fdfd9bd07de63e37b25ec

LABEL maintainer="ProphetSe7en" \
      description="VPN gateway with nftables bandwidth limiting, scheduling, and web UI"

COPY root/ /
COPY traffic.conf.sample /traffic.conf.sample
COPY --from=builder /build/vpn-gateway-ui /vpn-gateway-ui

RUN chmod +x /usr/local/bin/nft-apply \
             /usr/local/bin/healthcheck \
             /etc/s6-overlay/s6-rc.d/svc-traffic/run \
             /etc/s6-overlay/s6-rc.d/svc-traffic/finish \
             /etc/s6-overlay/s6-rc.d/svc-webui/run \
             /vpn-gateway-ui

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD /usr/local/bin/healthcheck
