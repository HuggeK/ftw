# Publishes ftw.local through the host's avahi-daemon over D-Bus. The image
# runs no responder of its own: balenaOS already answers mDNS on the LAN.
FROM debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends avahi-utils iproute2 && \
    rm -rf /var/lib/apt/lists/*

COPY publish.sh /usr/local/bin/publish-ftw-local
RUN chmod 0755 /usr/local/bin/publish-ftw-local

ENTRYPOINT ["/usr/local/bin/publish-ftw-local"]
