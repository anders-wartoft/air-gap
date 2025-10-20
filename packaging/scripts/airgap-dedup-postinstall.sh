#!/bin/bash
set -e

AIRGAP_USER="airgap"
AIRGAP_GROUP="airgap"
PACKAGE_NAME="airgap-dedup"

echo "Configuring $PACKAGE_NAME..."

# Create runtime directories
mkdir -p /opt/airgap/dedup
mkdir -p /var/lib/airgap/dedup
mkdir -p /var/log/airgap
mkdir -p /etc/airgap
mkdir -p /etc/default

# Set ownership
chown -R "$AIRGAP_USER:$AIRGAP_GROUP" /opt/airgap/dedup
chown -R "$AIRGAP_USER:$AIRGAP_GROUP" /var/lib/airgap/dedup
chown -R "$AIRGAP_USER:$AIRGAP_GROUP" /var/log/airgap
chmod 755 /opt/airgap/dedup /var/lib/airgap/dedup /var/log/airgap

# Config directory (root-owned)
chown root:root /etc/airgap
chmod 755 /etc/airgap

# Check Java
if ! command -v java >/dev/null 2>&1; then
    echo ""
    echo "⚠️  WARNING: Java runtime not found!"
    echo ""
    echo "Install Java 11+ with:"
    echo "  RHEL/Rocky: dnf install java-11-openjdk-headless"
    echo "  Debian/Ubuntu: apt install openjdk-11-jre-headless"
    echo "  Alpine: apk add openjdk11-jre-headless"
    echo ""
fi

# Systemd integration
if command -v systemctl >/dev/null 2>&1 && systemctl is-system-running >/dev/null 2>&1; then
    systemctl daemon-reload
    
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "✅ $PACKAGE_NAME installed successfully"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "📋 Setup Instructions:"
    echo ""
    echo "1. Create instance configuration(s):"
    echo "   cp /etc/airgap/dedup-template.env /etc/default/airgap-dedup-1"
    echo "   vi /etc/default/airgap-dedup-1"
    echo ""
    echo "   # Update STATE_DIR_CONFIG, APPLICATION_ID, and other settings"
    echo ""
    echo "2. Create state directory:"
    echo "   mkdir -p /var/lib/airgap/dedup/state-1"
    echo "   chown airgap:airgap /var/lib/airgap/dedup/state-1"
    echo ""
    echo "3. Enable and start instance:"
    echo "   systemctl enable airgap-dedup@1"
    echo "   systemctl start airgap-dedup@1"
    echo ""
    echo "4. Check status:"
    echo "   systemctl status airgap-dedup@1"
    echo "   journalctl -u airgap-dedup@1 -f"
    echo ""
    echo "💡 For multiple instances (e.g., per partition group):"
    echo "   Repeat steps 1-3 with different instance numbers"
    echo ""
    echo "📚 Documentation: /usr/share/doc/airgap-dedup/"
    echo "🎯 JAR Location: /opt/airgap/dedup/air-gap-deduplication-fat.jar"
    echo ""
fi
