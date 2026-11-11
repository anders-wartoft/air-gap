#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc47.XXXXXX")"
airgap_init_test 47
export TC30_WORK="$WORK"
compose() { docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc47.yml" "$@"; }
cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color --tail 60 >&2 || true
        echo "TC-47 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    echo "─── testcase 47 verdict ───"
    echo "  exit code : $rc"
    if (( rc != 0 )); then echo "  evidence : $WORK"; fi
    report_summary "TC-47 memory watermark self-recovery" "$rc" || true
    if (( rc == 0 )); then rm -rf "$WORK"; fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ "${TC_PAUSE:-0}" != 0 ]]; then
    echo "TC-47 does not support -pause" >&2
    exit 2
fi
docker info >/dev/null
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-47: unsupported Docker architecture" >&2; exit 2 ;;
esac
mkdir -p "$WORK/bin" "$WORK/lost-events"
printf '0\n' > "$WORK/sink.bytes"
chmod 0600 "$WORK/sink.bytes"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$WORK/upstream.test" ./src/upstream
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/upstream" ./src/cmd/upstream
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/airgap-load" ./tests/env/lib/loadgen
compose build sink upstream
echo "TC-47: reclaim the paused process's own heap in a real 1 GiB cgroup, with 60 concurrent claims"
compose run --rm --no-deps runtime-test | tee "$WORK/runtime-test.log"
compose up -d kafka topic-init sink
echo "TC-47: seed one million records across 30 partitions before upstream starts"
compose run --rm producer | tee "$WORK/producer.log"
cat > "$WORK/upstream.properties" <<'EOF'
id=Upstream_47
nic=eth0
targetIP=sink
targetPort=1234
transport=tcp
bootstrapServers=kafka:9092
topic=transfer
groupID=tc47
source=kafka
logLevel=INFO
logStatistics=2
payloadSize=65535
sendingThreads=[{"sec0":0},{"sec-120":-120}]
lostEventDirectory=/airgap/work/lost-events
memoryLowWatermarkLog=INFO:Upstream {upstreamId} leaving high memory watermark at {memoryCurrentBytes}.
EOF
compose up -d upstream
cid="$(compose ps -q upstream)"
deadline=$((SECONDS + 3600))
drained=0
while (( SECONDS < deadline )); do
    compose logs --no-color --no-log-prefix upstream > "$WORK/upstream.log"
    if grep -q 'leaving high memory watermark at [0-9]' "$WORK/upstream.log"; then
        echo "TC-47: production upstream resumed after a high-watermark pause"
    fi
    drained=0
    for group in tc47-sec0 tc47-sec-120; do
        docker exec "$(compose ps -q kafka)" kafka-consumer-groups \
            --bootstrap-server kafka:9092 --group "$group" --describe > "$WORK/$group.lag" 2>/dev/null
        if awk '$2 == "transfer" && $6 ~ /^[0-9]+$/ { n++; lag += $6 } END { exit !(n == 30 && lag == 0) }' "$WORK/$group.lag"; then
            drained=$((drained + 1))
        fi
    done
    (( drained == 2 )) && break
    sleep 2
done
[[ "$drained" == 2 ]] || { echo "TC-47: both 30-partition groups must reach lag zero" >&2; exit 1; }
deadline=$((SECONDS + 30))
while (( SECONDS < deadline )); do
    bytes="$(cat "$WORK/sink.bytes")"
    (( bytes >= 2 * 1000020 * 1024 )) && break
    sleep 1
done
(( bytes >= 2 * 1000020 * 1024 ))
[[ "$(docker inspect --format '{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}}' "$cid")" == "true false 0" ]]
compose logs --no-color --no-log-prefix upstream > "$WORK/upstream.log"
if grep -q 'abandoned subscription' "$WORK/upstream.log"; then
    echo "TC-47: partition subscription was abandoned" >&2
    exit 1
fi
report_check "million-record backlog" "both groups lag zero, payloads delivered, no restart/OOM/abandoned subscription" \
    "30 partitions per group, sink bytes=$bytes" 0
