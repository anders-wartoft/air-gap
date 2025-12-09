#!/bin/bash
set -e

AIRGAP_USER="airgap"
AIRGAP_GROUP="airgap"
PACKAGE_NAME="airgap-downstream"

echo "Configuring $PACKAGE_NAME..."

# Create runtime directories
mkdir -p /var/lib/airgap/downstream
mkdir -p /var/log/airgap
mkdir -p /etc/airgap
mkdir -p /etc/default

# Set ownership
chown -R "$AIRGAP_USER:$AIRGAP_GROUP" /var/lib/airgap
chown -R "$AIRGAP_USER:$AIRGAP_GROUP" /var/log/airgap
chmod 755 /var/lib/airgap /var/log/airgap

# Config directory (root-owned)
chown root:root /etc/airgap
chmod 755 /etc/airgap

# Systemd integration
if command -v systemctl >/dev/null 2>&1 && systemctl is-system-running >/dev/null 2>&1; then
    systemctl daemon-reload
    
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "✅ $PACKAGE_NAME installed successfully"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "📋 Next Steps:"
    echo ""
    echo "1. Create configuration:"
    echo "   cp /etc/airgap/downstream.properties.example /etc/airgap/downstream.properties"
    echo "   vi /etc/airgap/downstream.properties"
    echo ""
    echo "2. (Optional) Create environment overrides:"
    echo "   vi /etc/default/airgap-downstream"
    echo ""
    echo "3. Enable and start service:"
    echo "   systemctl enable airgap-downstream"
    echo "   systemctl start airgap-downstream"
    echo ""
    echo "4. Check status:"
    echo "   systemctl status airgap-downstream"
    echo "   journalctl -u airgap-downstream -f"
    echo ""
    echo "💡 Note: Consider installing airgap-dedup package for deduplication"
    echo "📚 Documentation: /usr/share/doc/airgap-downstream/"
    echo ""
fi
