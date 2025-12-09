#!/usr/bin/env bash
set -euo pipefail

COMPONENT="${1:-}"
FORMAT="${2:-}"
PACKAGE="${3:-}"

if [ -z "$COMPONENT" ] || [ -z "$FORMAT" ] || [ -z "$PACKAGE" ]; then
    echo "Usage: $0 <component> <format> <package-path>" >&2
    exit 1
fi

if [ ! -f "$PACKAGE" ]; then
    echo "Package not found: $PACKAGE" >&2
    exit 1
fi

echo "========================================="
echo "Testing $COMPONENT ($FORMAT)"
echo "Package: $PACKAGE"
echo "========================================="

# Install based on format
case "$FORMAT" in
    rpm)
        echo "Installing RPM..."
        if command -v dnf &> /dev/null; then
            dnf install -y "$PACKAGE"
        else
            yum install -y "$PACKAGE" || rpm -ivh "$PACKAGE"
        fi
        ;;
    deb)
        echo "Installing DEB..."
        apt-get update -qq
        apt-get install -y "$PACKAGE" || {
            apt-get install -f -y
            apt-get install -y "$PACKAGE"
        }
        ;;
    apk)
        echo "Installing APK..."
        apk add --allow-untrusted "$PACKAGE"
        ;;
    *)
        echo "Unsupported format: $FORMAT" >&2
        exit 1
        ;;
esac

echo ""
echo "Verifying installation..."

# Verify based on component
case "$COMPONENT" in
    upstream)
        test -x /usr/local/bin/airgap-upstream || exit 1
        test -x /usr/local/bin/airgap-resend || exit 1
        test -f /etc/airgap/upstream.properties.example || exit 1
        echo "✅ Upstream package verification passed"
        ;;
    downstream)
        test -x /usr/local/bin/airgap-downstream || exit 1
        test -x /usr/local/bin/airgap-create || exit 1
        test -f /etc/airgap/downstream.properties.example || exit 1
        echo "✅ Downstream package verification passed"
        ;;
    dedup)
        ls /opt/airgap/java-streams/air-gap-deduplication-fat-*.jar || exit 1
        test -f /etc/airgap/dedup-template.env || exit 1
        echo "✅ Dedup package verification passed"
        ;;
    *)
        echo "Unknown component: $COMPONENT" >&2
        exit 1
        ;;
esac

echo "========================================="
echo "✅ All verifications passed"
echo "========================================="
