#!/bin/bash
# Test DEB package installation on Ubuntu
set -e

echo "=== Testing DEB Packages on Ubuntu ==="
echo ""

apt-get update -qq

# Test upstream package
echo "1. Testing airgap-upstream..."
apt-get install -y /packages/airgap-upstream_*_amd64.deb
ls -l /usr/local/bin/airgap-upstream /usr/local/bin/airgap-resend
id airgap
echo "✓ airgap-upstream installed"
echo ""

# Test downstream package
echo "2. Testing airgap-downstream..."
apt-get install -y /packages/airgap-downstream_*_amd64.deb
ls -l /usr/local/bin/airgap-downstream /usr/local/bin/airgap-create
echo "✓ airgap-downstream installed"
echo ""

# Test dedup package
echo "3. Testing airgap-dedup..."
apt-get install -y /packages/airgap-dedup_*_amd64.deb
ls -l /opt/airgap/dedup/air-gap-deduplication-fat.jar
ls -l /lib/systemd/system/airgap-dedup@.service
echo "✓ airgap-dedup installed"
echo ""

echo "=== All DEB tests PASSED ==="
