#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc48.XXXXXX")"
airgap_init_test 48
export TC48_WORK="$WORK"
# Compose interpolates all services in the TC-33 file used for broker setup.
export TC33_WORK="$WORK"
compose() { docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc48.yml" "$@"; }
cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color --tail 60 >&2 || true
        echo "TC-48 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    echo "─── testcase 48 verdict ───"
    echo "  exit code : $rc"
    report_summary "TC-48 periodic memory-stall STATUS" "$rc" || true
    if (( rc == 0 )); then rm -rf "$WORK"; fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ "${TC_PAUSE:-0}" != 0 ]]; then
    echo "TC-48 does not support -pause" >&2
    exit 2
fi
docker info >/dev/null
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-48: unsupported Docker architecture" >&2; exit 2 ;;
esac
mkdir -p "$WORK/bin"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/" ./src/cmd/upstream ./src/cmd/downstream
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/airgap-load" ./tests/env/lib/loadgen
compose build downstream
compose up -d kafka topic-init downstream
kafka="$(compose ps -q kafka)"

for interval in 2 0; do
    cat > "$WORK/upstream.properties" <<EOF
id=Upstream_48_$interval
nic=eth0
targetIP=downstream
targetPort=1234
transport=tcp
bootstrapServers=kafka:9092
topic=source
topicTranslations={"source":"transfer"}
groupID=tc48-$interval
source=kafka
logLevel=INFO
logStatistics=0
payloadSize=1400
sendingThreads=[{"No-delay":0}]
tcpRetryInterval=100
memoryHighWatermarkPercent=60
memoryLowWatermarkPercent=50
goMemoryLimitPercent=40
memoryCheckIntervalMs=50
memoryStallStatusIntervalSeconds=$interval
memoryLowWatermarkLog=INFO:Upstream {upstreamId} resumed Kafka admission.
EOF
    compose up -d upstream
    upstream="$(compose ps -q upstream)"
    deadline=$((SECONDS + 60))
    while true; do
        compose logs --no-color upstream > "$WORK/upstream-$interval-ready.log"
        grep -q 'up and running' "$WORK/upstream-$interval-ready.log" && break
        (( SECONDS < deadline )) || { echo "TC-48 upstream readiness timed out" >&2; exit 1; }
        sleep 1
    done
    # This helper holds unreclaimable pressure in the upstream's cgroup.
    # No record loss or memory-recovery behavior is inferred from its release.
    docker exec -d "$upstream" /airgap/work/airgap-load pressure --bytes=125829120 --duration=16s
    compose run --rm producer
    deadline=$((SECONDS + 15))
    while true; do
        compose logs --no-color upstream > "$WORK/upstream-$interval-pause.log"
        grep -q '\[WARN\] Upstream .* entering high memory watermark' "$WORK/upstream-$interval-pause.log" && break
        (( SECONDS < deadline )) || { echo "TC-48 upstream did not pause" >&2; exit 1; }
        sleep 1
    done
    compose run --rm producer
    sleep 7
    compose logs --no-color --no-log-prefix upstream > "$WORK/upstream-$interval-paused.log"
    count="$(grep -c 'Kafka admission stalled for' "$WORK/upstream-$interval-paused.log" || true)"
    if (( interval > 0 )); then
        (( count >= 2 )) || { echo "TC-48 expected repeated local warnings, got $count" >&2; exit 1; }
    else
        (( count == 0 )) || { echo "TC-48 disabled interval still warned" >&2; exit 1; }
    fi
    deadline=$((SECONDS + 45))
    while true; do
        compose logs --no-color upstream > "$WORK/upstream-$interval-recovery.log"
        grep -q '\[INFO\] Upstream .* resumed Kafka admission' "$WORK/upstream-$interval-recovery.log" && break
        (( SECONDS < deadline )) || { echo "TC-48 upstream did not recover" >&2; exit 1; }
        sleep 1
    done
    compose logs --no-color --no-log-prefix upstream > "$WORK/upstream-$interval-resumed.log"
    before="$(grep -c 'Kafka admission stalled for' "$WORK/upstream-$interval-resumed.log" || true)"
    sleep 4
    compose logs --no-color --no-log-prefix upstream > "$WORK/upstream-$interval.log"
    after="$(grep -c 'Kafka admission stalled for' "$WORK/upstream-$interval.log" || true)"
    [[ "$before" == "$after" ]] || { echo "TC-48 stall alerts continued after recovery" >&2; exit 1; }
    [[ "$(docker inspect --format '{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}}' "$upstream")" == "true false 0" ]]
    docker exec "$kafka" kafka-consumer-groups --bootstrap-server kafka:9092 \
        --group "tc48-$interval-No-delay" --describe > "$WORK/lag-$interval.log"
    awk '$2 == "source" && $6 ~ /^[0-9]+$/ { n++; lag += $6 } END { exit !(n == 1 && lag == 0) }' "$WORK/lag-$interval.log"
    compose stop upstream
    compose rm -f upstream
done

# Snapshot every partition to a known end offset, so a console timeout cannot
# masquerade as an empty/passing routing assertion.
for topic in airgap-internal transfer; do
    docker exec "$kafka" kafka-get-offsets --bootstrap-server kafka:9092 --topic "$topic" > "$WORK/$topic.offsets"
    count="$(awk -F: '{ n += $3 } END { print n+0 }' "$WORK/$topic.offsets")"
    (( count > 0 )) || { echo "TC-48 no records in $topic" >&2; exit 1; }
    docker exec "$kafka" timeout 30s kafka-console-consumer --bootstrap-server kafka:9092 \
        --topic "$topic" --from-beginning --max-messages "$count" > "$WORK/$topic.log"
done
count="$(grep -c 'Upstream_48_2 Kafka admission stalled for' "$WORK/airgap-internal.log" || true)"
(( count >= 2 )) || { echo "TC-48 expected repeated downstream STATUS records, got $count" >&2; exit 1; }
if grep -q 'Upstream_48_0 Kafka admission stalled for' "$WORK/airgap-internal.log" ||
   grep -q 'Kafka admission stalled for' "$WORK/transfer.log"; then
    echo "TC-48 disabled STATUS or stall alert in normal event topic" >&2
    exit 1
fi
grep -q 'current=.*limit=.*resume_at=.*source=cgroup' "$WORK/airgap-internal.log"
report_check "stall STATUS" "repeated WARN and internal-topic STATUS, disabled at 0, reset on recovery" \
    "$count downstream alerts; both runs resumed and drained with no restarts/OOM" 0
