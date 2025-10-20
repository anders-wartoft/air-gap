#!/usr/bin/env bash
set -euo pipefail
FORMAT="${1:-apk}"
PACKAGE="${2:-/packages/airgap-dedup-*.${FORMAT}}"
if [[ "$PACKAGE" == *"*"* ]]; then
    PACKAGE=$(ls $PACKAGE 2>/dev/null | head -n1)
fi
exec "$(dirname "$0")/verify-package.sh" dedup "$FORMAT" "$PACKAGE"
