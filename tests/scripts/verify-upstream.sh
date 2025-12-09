#!/usr/bin/env bash
set -euo pipefail
FORMAT="${1:-rpm}"
PACKAGE="${2:-/packages/airgap-upstream-*.${FORMAT}}"
if [[ "$PACKAGE" == *"*"* ]]; then
    PACKAGE=$(ls $PACKAGE 2>/dev/null | head -n1)
fi
exec "$(dirname "$0")/verify-package.sh" upstream "$FORMAT" "$PACKAGE"
