#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
if [[ "${TC_PAUSE:-0}" != 0 ]]; then
    echo "TC-46 does not support -pause; child JVM logs and Maven reports are retained under java-streams/target" >&2
    exit 2
fi
airgap_init_test 46
compose() { docker compose -p "$PROJECT" -f "$HERE/docker-compose.dedup-state.yml" "$@"; }
cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then compose logs --no-color --tail 60 >&2; fi
    compose down -v --remove-orphans --timeout 10 || rc=1
    report_summary "TC-46 bounded dedup state and crash recovery" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker info >/dev/null
compose up -d --wait --wait-timeout 180
export DEDUP_TEST_BOOTSTRAP="localhost:${DEDUP_TEST_KAFKA_PORT:-19092}"
mvn -f "$HERE/../../java-streams/pom.xml" -Pdocker-tests verify "$@"
