# Pinned digest from 2026-02-23. To update: docker pull ghcr.io/hotio/base:alpinevpn && docker inspect --format '{{.RepoDigests}}' ghcr.io/hotio/base:alpinevpn
FROM ghcr.io/hotio/base@sha256:a3f171bc00b03218907c6bd6530e4231446def5ed2e8ebed53927ab2693919c7

LABEL maintainer="ProphetSe7en" \
      description="VPN gateway with nftables bandwidth limiting and scheduling"

COPY root/ /
COPY traffic.conf.sample /traffic.conf.sample

RUN chmod +x /usr/local/bin/nft-apply \
             /etc/s6-overlay/s6-rc.d/svc-traffic/run \
             /etc/s6-overlay/s6-rc.d/svc-traffic/finish
