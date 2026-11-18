#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
export TC52_PACING_COMPARISON=1
export TC52_DURATION="${TC54_DURATION:-20}" TC52_WARMUP="${TC54_WARMUP:-3}"
exec bash "$HERE/tc52-kafka-transport-stress-test.sh"
