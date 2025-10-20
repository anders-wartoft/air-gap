#!/bin/bash
# Test RPM package installation on Rocky Linux
set -e

echo "=== Testing RPM Packages on Rocky Linux ==="
echo ""

# Test upstream package
echo "1. Testing airgap-upstream..."
dnf install -y /packages/airgap-upstream-*.rpm
ls -l /usr/local/bin/airgap-upstream /usr/local/bin/airgap-resend
id airgap
echo "✓ airgap-upstream installed"
echo ""

# Test downstream package
echo "2. Testing airgap-downstream..."
dnf install -y /packages/airgap-downstream-*.rpm
ls -l /usr/local/bin/airgap-downstream /usr/local/bin/airgap-create
echo "✓ airgap-downstream installed"
echo ""

# Test dedup package
echo "3. Testing airgap-dedup..."
dnf install -y /packages/airgap-dedup-*.rpm
ls -l /opt/airgap/dedup/air-gap-deduplication-fat.jar
ls -l /lib/systemd/system/airgap-dedup@.service
echo "✓ airgap-dedup installed"
echo ""

echo "=== All RPM tests PASSED ==="
