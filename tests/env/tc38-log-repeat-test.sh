#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
airgap_init_test 38
exec python3 "$HERE/tc38-log-repeat-test.py"
