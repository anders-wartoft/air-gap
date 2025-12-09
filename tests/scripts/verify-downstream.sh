#!/usr/bin/env bash
set -euo pipefail
FORMAT="${1:-deb}"
PACKAGE="${2:-/packages/airgap-downstream-*.${FORMAT}}"
if [[ "$PACKAGE" == *"*"* ]]; then
    PACKAGE=$(ls $PACKAGE 2>/dev/null | head -n1)
fi
exec "$(dirname "$0")/verify-package.sh" downstream "$FORMAT" "$PACKAGE"
