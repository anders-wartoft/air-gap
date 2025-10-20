#!/bin/bash
set -e

# Create airgap system user and group (idempotent)
AIRGAP_USER="airgap"
AIRGAP_GROUP="airgap"

# Detect package type
if [ -f /etc/redhat-release ] || [ -f /etc/rocky-release ]; then
    PKG_TYPE="rpm"
elif [ -f /etc/debian_version ]; then
    PKG_TYPE="deb"
elif [ -f /etc/alpine-release ]; then
    PKG_TYPE="apk"
else
    PKG_TYPE="unknown"
fi

# Create group if doesn't exist
if ! getent group "$AIRGAP_GROUP" >/dev/null 2>&1; then
    echo "Creating airgap group..."
    case "$PKG_TYPE" in
        rpm)
            groupadd -r "$AIRGAP_GROUP" 2>/dev/null || true
            ;;
        deb)
            addgroup --system "$AIRGAP_GROUP" 2>/dev/null || true
            ;;
        apk)
            addgroup -S "$AIRGAP_GROUP" 2>/dev/null || true
            ;;
    esac
fi

# Create user if doesn't exist
if ! id "$AIRGAP_USER" >/dev/null 2>&1; then
    echo "Creating airgap user..."
    case "$PKG_TYPE" in
        rpm)
            useradd -r -g "$AIRGAP_GROUP" -s /sbin/nologin \
                -d /var/lib/airgap -c "Air Gap Service User" "$AIRGAP_USER" 2>/dev/null || true
            ;;
        deb)
            adduser --system --group --no-create-home --home /var/lib/airgap \
                --gecos "Air Gap Service User" "$AIRGAP_USER" 2>/dev/null || true
            ;;
        apk)
            adduser -S -G "$AIRGAP_GROUP" -H -h /var/lib/airgap \
                -s /sbin/nologin "$AIRGAP_USER" 2>/dev/null || true
            ;;
    esac
fi

echo "✅ Air gap user and group configured"
