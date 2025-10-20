#!/bin/bash
set -e

PACKAGE_NAME="airgap-downstream"

echo "Cleaning up $PACKAGE_NAME..."

# Systemd cleanup
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

# Note: We don't remove user/group as they might be used by other airgap packages
# Note: We don't remove /var/lib/airgap or /var/log/airgap for data preservation

echo "✅ $PACKAGE_NAME removed"
