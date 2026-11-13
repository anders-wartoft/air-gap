#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-51 does not support -pause" >&2
    exit 2
fi
airgap_init_test 51
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc51.XXXXXX")"
chmod 0755 "$WORK"

cleanup() {
    local rc=$?
    trap - EXIT
    rm -f "$WORK/transport.test"
    rmdir "$WORK" || rc=1
    report_summary "TC-51 TCP and TLS transport throughput" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

cd "$HERE/../.."
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-51: unsupported Docker architecture" >&2; exit 2 ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$WORK/transport.test" ./src/downstream
chmod 0755 "$WORK/transport.test"
echo "TC-51: 20,000 ordered 256-byte records per transport; minimum 5,000 delivered EPS"
airgap_docker_run --rm -e AIRGAP_TRANSPORT_PERF=1 \
    -v "$WORK:/test:ro" "$AIRGAP_BASE_IMAGE" \
    /test/transport.test -test.run '^TestTransportThroughput$' -test.v -test.timeout=120s
