#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/udp-calibration.sh"

mkdir -p "$1"
results="$(cd "$1" && pwd)"
project="airgap-calibration-$(LC_ALL=C od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
work="$(mktemp -d "${TMPDIR:-/tmp}/airgap-calibration.XXXXXX")"
export TC52_WORK="$work" TC52_RESULTS="$results" TC52_DURATION=20 TC52_WARMUP=3 TC52_WINDOW=10000
export AIRGAP_TEST_UDP_PACING=""
compose() { docker compose -p "$project" -f "$HERE/docker-compose.tc52.yml" "$@"; }
started=0
cleanup() {
    local rc=$?
    if (( started )); then
        compose logs --no-color downstream target-kafka >"$results/services.log" 2>&1 || rc=1
        compose down -v --remove-orphans >&2 || rc=1
    fi
    rm -f "$work/bin/upstream" "$work/bin/downstream" "$work/downstream.properties"
    rmdir "$work/bin" "$work" || rc=1
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
chmod 0755 "$work"
mkdir -m 0755 "$work/bin"
rm -f "$results/pacing.properties"
cd "$ROOT"
if [[ -n "${2:-}" ]]; then
    cp "$2/upstream" "$2/downstream" "$work/bin/"
else
    case "$(docker info --format '{{.Architecture}}')" in
        aarch64|arm64) arch=arm64 ;;
        x86_64|amd64) arch=amd64 ;;
        *) echo "UDP calibration: unsupported Docker architecture" >&2; exit 2 ;;
    esac
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$work/bin/" ./src/cmd/upstream ./src/cmd/downstream
fi
airgap_write_stress_downstream_config udp "${TC52_BATCH_SIZE:-512}" >"$work/downstream.properties"
cp "$work/downstream.properties" "$results/"
started=1
compose up -d --wait target-kafka
for topic in transfer airgap-internal airgap-calibration-tests; do
    compose exec -T target-kafka kafka-topics --bootstrap-server localhost:9092 --create \
        --topic "$topic" --partitions 10 --config retention.bytes=104857600 --config segment.bytes=16777216
done
compose up -d downstream
deadline=$((SECONDS + 90))
while true; do
    compose logs --no-color downstream >"$results/downstream-ready.log"
    [[ "$(grep -c 'UDP socket [0-9][0-9]*: SO_RCVBUF requested=' "$results/downstream-ready.log")" == 10 ]] && break
    (( SECONDS < deadline )) || { echo "UDP calibration: downstream did not become ready" >&2; exit 1; }
    sleep 1
done
if ! compose run --rm --no-deps -T --entrypoint /airgap/bin/upstream upstream --calibrate \
    --address=downstream:1234 --nic=eth0 --brokers=target-kafka:9092 \
    --topic=airgap-calibration-tests --burst="${TC54_BURST_DATAGRAMS:-32}" \
    >"$results/calibration.out" 2>"$results/calibration.log"; then
    cat "$results/calibration.log" >&2
    exit 1
fi
airgap_load_udp_calibration "$results/calibration.out"
cp "$results/calibration.out" "$results/pacing.properties"
