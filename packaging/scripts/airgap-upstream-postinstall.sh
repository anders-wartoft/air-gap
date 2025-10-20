#!/bin/bash
set -e

AIRGAP_USER="airgap"
AIRGAP_GROUP="airgap"
PACKAGE_NAME="airgap-upstream"

echo "Configuring $PACKAGE_NAME..."

# Create runtime directories
mkdir -p /var/lib/airgap/upstream
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
    echo "Reloading systemd daemon..."
    systemctl daemon-reload
    
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "✅ $PACKAGE_NAME installed successfully"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "📋 Next Steps:"
    echo ""
    echo "1. Create configuration:"
    echo "   cp /etc/airgap/upstream.properties.example /etc/airgap/upstream.properties"
    echo "   vi /etc/airgap/upstream.properties"
    echo ""
    echo "2. (Optional) Create environment overrides:"
    echo "   vi /etc/default/airgap-upstream"
    echo ""
    echo "3. Enable and start service:"
    echo "   systemctl enable airgap-upstream"
    echo "   systemctl start airgap-upstream"
    echo ""
    echo "4. Check status:"
    echo "   systemctl status airgap-upstream"
    echo "   journalctl -u airgap-upstream -f"
    echo ""
    echo "📚 Documentation: /usr/share/doc/airgap-upstream/"
    echo ""
fi
