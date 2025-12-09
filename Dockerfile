# syntax=docker/dockerfile:1.7

ARG BASE_IMAGE=rockylinux:9
FROM ${BASE_IMAGE} AS test

ARG PACKAGE_FILE
ARG PKG_TYPE=rpm
ARG PACKAGE_NAME=airgap

COPY ${PACKAGE_FILE} /tmp/package.${PKG_TYPE}

RUN set -euxo pipefail; \
    case "${PKG_TYPE}" in \
      rpm) \
        dnf -y install shadow-utils >/dev/null 2>&1 || true; \
        dnf -y install /tmp/package.rpm; \
        ;; \
      deb) \
        apt-get update; \
        apt-get install -y --no-install-recommends ca-certificates; \
        apt-get install -y /tmp/package.deb || (apt-get install -f -y && apt-get install -y /tmp/package.deb); \
        ;; \
      apk) \
        apk update; \
        apk add --no-cache shadow; \
        apk add --no-cache /tmp/package.apk; \
        ;; \
      *) \
        echo "unsupported package type: ${PKG_TYPE}" >&2; \
        exit 1; \
        ;; \
    esac

RUN set -eux; \
    command -v ${PACKAGE_NAME}-upstream; \
    command -v ${PACKAGE_NAME}-downstream; \
    test -d /etc/${PACKAGE_NAME}; \
    ls -1 /etc/${PACKAGE_NAME}

ENTRYPOINT ["/bin/sh"]
