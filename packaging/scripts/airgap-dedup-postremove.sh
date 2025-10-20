#!/bin/bash
set -e

PACKAGE_NAME="airgap-dedup"

echo "Cleaning up $PACKAGE_NAME..."

# Systemd cleanup
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

# Note: We don't remove user/group as they might be used by other airgap packages
# Note: We don't remove /var/lib/airgap or /var/log/airgap for data preservation
# Note: We don't remove /opt/airgap/dedup to preserve any custom files

echo "✅ $PACKAGE_NAME removed"
echo "💡 State files preserved in /var/lib/airgap/dedup/"
