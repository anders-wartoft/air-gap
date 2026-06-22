#!/bin/bash
# vmware-drop-detector.sh - Detect packet drops in VMware/hypervisor layer
#
# This script compares upstream messages sent vs downstream messages received
# to detect packet loss that occurs in the hypervisor networking layer.
# Such drops are invisible to Linux guest OS metrics (SO_RXQ_OVFL, ethtool, netstat).
#
# Usage:
#   ./vmware-drop-detector.sh [upstream_log] [downstream_log]
#
# Default log locations:
#   - Upstream: /var/log/airgap/upstream.log
#   - Downstream: /var/log/airgap/downstream.log

set -e

# Parse command line arguments
UPSTREAM_LOG="${1:-/var/log/airgap/upstream.log}"
DOWNSTREAM_LOG="${2:-/var/log/airgap/downstream.log}"

# Verify log files exist
if [ ! -f "$UPSTREAM_LOG" ]; then
    echo "ERROR: Upstream log not found: $UPSTREAM_LOG"
    echo "Usage: $0 [upstream_log] [downstream_log]"
    exit 1
fi

if [ ! -f "$DOWNSTREAM_LOG" ]; then
    echo "ERROR: Downstream log not found: $DOWNSTREAM_LOG"
    echo "Usage: $0 [upstream_log] [downstream_log]"
    exit 1
fi

# Check for jq
if ! command -v jq &> /dev/null; then
    echo "ERROR: jq is required but not installed"
    echo "Install with: sudo apt-get install jq  # or  sudo yum install jq"
    exit 1
fi

echo "=== Air-Gap Packet Loss Detector ==="
echo ""
echo "Analyzing logs:"
echo "  Upstream:   $UPSTREAM_LOG"
echo "  Downstream: $DOWNSTREAM_LOG"
echo ""

# Extract latest statistics
UP_SENT=$(grep "STATISTICS" "$UPSTREAM_LOG" | tail -1 | jq -r '.total_messages_sent // .total_sent // empty')
DOWN_RECV=$(grep "STATISTICS" "$DOWNSTREAM_LOG" | tail -1 | jq -r '.total_received // empty')
SO_RXQ=$(grep "STATISTICS" "$DOWNSTREAM_LOG" | tail -1 | jq -r '.SO_RXQ_OVFL_TOTAL // 0')

# Verify we got data
if [ -z "$UP_SENT" ] || [ -z "$DOWN_RECV" ]; then
    echo "ERROR: Could not parse statistics from logs"
    echo "  Upstream total_messages_sent: ${UP_SENT:-NOT FOUND}"
    echo "  Downstream total_received: ${DOWN_RECV:-NOT FOUND}"
    echo ""
    echo "Make sure logs contain STATISTICS lines with JSON format"
    exit 1
fi

# Calculate loss
LOSS=$((UP_SENT - DOWN_RECV))
LOSS_PERCENT=$(awk "BEGIN {printf \"%.2f\", ($LOSS / $UP_SENT) * 100}")

echo "Message Counts:"
echo "  Upstream sent:     $UP_SENT"
echo "  Downstream recv:   $DOWN_RECV"
echo "  Loss:              $LOSS ($LOSS_PERCENT%)"
echo "  SO_RXQ_OVFL_TOTAL: $SO_RXQ"
echo ""

# Determine drop location
if [ "$LOSS" -gt 1000 ]; then
    if [ "$SO_RXQ" -eq 0 ]; then
        echo "⚠️  HYPERVISOR/VMWARE DROPS DETECTED!"
        echo ""
        echo "Analysis:"
        echo "  - $LOSS messages lost ($LOSS_PERCENT% loss rate)"
        echo "  - SO_RXQ_OVFL shows 0 drops (socket buffer not the problem)"
        echo "  - Drops are occurring OUTSIDE the Linux guest OS"
        echo "  - Most likely: VMware virtual switch or host NIC drops"
        echo ""
        echo "This is normal in VM environments and invisible to Linux metrics."
        echo ""
        echo "Verify with these commands on downstream:"
        echo "  ethtool -S eth0 | grep drop           # Should show 0"
        echo "  netstat -s | grep 'receive buffer'    # Should show 0"
        echo "  cat /proc/net/udp | awk '{print \$13}' # Should show 0s"
        echo ""
        echo "If all show 0, confirms hypervisor drops."
        echo ""
        echo "Solutions:"
        echo "  1. Test on physical hardware (bare metal)"
        echo "  2. Reduce upstream send rate (eps parameter)"
        echo "  3. Use VMXNET3 virtual NIC driver"
        echo "  4. Add CPU/memory resources to VMs"
        echo "  5. Pin VMs to dedicated CPU cores"
        echo "  6. Accept 5-10% loss as expected for VM networking"
        echo ""
        echo "The deduplicator is designed to detect and recover from"
        echo "these gaps - this is working as intended!"
    else
        echo "⚠️  APPLICATION BOTTLENECK DETECTED!"
        echo ""
        echo "Analysis:"
        echo "  - $LOSS messages lost ($LOSS_PERCENT% loss rate)"
        echo "  - SO_RXQ_OVFL shows $SO_RXQ drops"
        echo "  - Downstream application can't read from socket fast enough"
        echo ""
        echo "Solutions:"
        echo "  1. Increase numReceivers in downstream config"
        echo "  2. Increase SO_RCVBUF socket buffer size"
        echo "  3. Reduce upstream send rate (eps parameter)"
        echo "  4. Add CPU cores to downstream machine"
    fi
elif [ "$LOSS" -gt 0 ]; then
    echo "ℹ️  Minor packet loss detected ($LOSS messages, $LOSS_PERCENT%)"
    echo ""
    echo "This is within normal UDP tolerance for testing."
    echo "Monitor trend over time - alert if loss rate increases."
else
    echo "✓ No packet loss detected!"
    echo ""
    echo "Upstream and downstream message counts match."
    echo "System is operating normally."
fi

echo ""
echo "=== End of Report ==="
