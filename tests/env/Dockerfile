# syntax=docker/dockerfile:1.7
#
# Air-gap test container (upstream and downstream share this image).
#
# Design:
#   * The OS is a parameter (BASE_IMAGE). Update it by rebuilding with a new tag.
#   * The air-gap binary is NOT baked into the image. It is bind-mounted at
#     /airgap/bin at runtime so the latest build on the host is always used.
#   * The config file is also bind-mounted and chosen via AIRGAP_CONFIG.
#   * The container only installs light runtime diagnostics (ip/ping/ps),
#     which are useful when debugging UDP/TCP transport inside Docker.

ARG BASE_IMAGE=rockylinux:9
FROM ${BASE_IMAGE}

RUN set -eux; \
    if command -v dnf >/dev/null 2>&1; then \
        dnf -y install iproute iputils procps-ng tcpdump && dnf clean all; \
    elif command -v microdnf >/dev/null 2>&1; then \
        microdnf -y install iproute iputils procps-ng tcpdump && microdnf clean all; \
    elif command -v apt-get >/dev/null 2>&1; then \
        apt-get update && \
        apt-get install -y --no-install-recommends iproute2 iputils-ping procps ca-certificates tcpdump && \
        rm -rf /var/lib/apt/lists/*; \
    elif command -v apk >/dev/null 2>&1; then \
        apk add --no-cache iproute2 iputils procps bash tcpdump; \
    fi

WORKDIR /airgap

COPY entrypoint.sh /usr/local/bin/airgap-entrypoint
RUN chmod +x /usr/local/bin/airgap-entrypoint

ENTRYPOINT ["/usr/local/bin/airgap-entrypoint"]
CMD ["upstream"]
