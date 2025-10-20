#!/bin/sh
# Test APK package installation on Alpine
set -e

echo "=== Testing APK Packages on Alpine ==="
echo ""

# Test upstream package
echo "1. Testing airgap-upstream..."
apk add --allow-untrusted /packages/airgap-upstream_*.apk
ls -l /usr/local/bin/airgap-upstream /usr/local/bin/airgap-resend
id airgap
echo "✓ airgap-upstream installed"
echo ""

# Test downstream package
echo "2. Testing airgap-downstream..."
apk add --allow-untrusted /packages/airgap-downstream_*.apk
ls -l /usr/local/bin/airgap-downstream /usr/local/bin/airgap-create
echo "✓ airgap-downstream installed"
echo ""

# Test dedup package (requires Java)
echo "3. Testing airgap-dedup..."
apk add openjdk11-jre-headless
apk add --allow-untrusted /packages/airgap-dedup_*.apk
ls -l /opt/airgap/dedup/air-gap-deduplication-fat.jar
java -version
echo "✓ airgap-dedup installed"
echo ""

echo "=== All APK tests PASSED ==="
