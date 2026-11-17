#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
[[ "${TC_PAUSE:-0}" == 0 ]] || { echo "TC-53 does not support -pause" >&2; exit 2; }

TC53_RATE="${TC53_RATE:-10000}"
TC53_DURATION="${TC53_DURATION:-2}"
TC53_IDLE="${TC53_IDLE:-1}"
TC53_CYCLES="${TC53_CYCLES:-3}"
TC53_PAYLOAD_SIZE="${TC53_PAYLOAD_SIZE:-600}"
TC53_BATCH_SIZE="${TC53_BATCH_SIZE:-512}"
TC53_RECEIVERS="${TC53_RECEIVERS:-10}"
TC53_MAX_SPARSE_LATENCY="${TC53_MAX_SPARSE_LATENCY:-2}"
TC53_DRAIN_TIMEOUT="${TC53_DRAIN_TIMEOUT:-30}"
for value in "$TC53_RATE" "$TC53_DURATION" "$TC53_IDLE" "$TC53_CYCLES" "$TC53_PAYLOAD_SIZE" \
    "$TC53_BATCH_SIZE" "$TC53_RECEIVERS" "$TC53_MAX_SPARSE_LATENCY" "$TC53_DRAIN_TIMEOUT"; do
    [[ "$value" =~ ^[0-9]+$ ]] || { echo "TC-53 requires integer settings" >&2; exit 2; }
done
TC53_RATE=$((10#$TC53_RATE))
TC53_DURATION=$((10#$TC53_DURATION))
TC53_IDLE=$((10#$TC53_IDLE))
TC53_CYCLES=$((10#$TC53_CYCLES))
TC53_PAYLOAD_SIZE=$((10#$TC53_PAYLOAD_SIZE))
TC53_BATCH_SIZE=$((10#$TC53_BATCH_SIZE))
TC53_RECEIVERS=$((10#$TC53_RECEIVERS))
TC53_MAX_SPARSE_LATENCY=$((10#$TC53_MAX_SPARSE_LATENCY))
TC53_DRAIN_TIMEOUT=$((10#$TC53_DRAIN_TIMEOUT))
(( TC53_RATE >= 1 && TC53_RATE <= 200000 )) || { echo "TC-53 rate must be 1..200000" >&2; exit 2; }
(( TC53_DURATION >= 1 && TC53_DURATION <= 600 )) || { echo "TC-53 duration must be 1..600 seconds" >&2; exit 2; }
(( TC53_IDLE <= 3600 && TC53_CYCLES >= 1 && TC53_CYCLES <= 100 )) || {
    echo "TC-53 idle must be 0..3600 seconds and cycles 1..100" >&2; exit 2;
}
(( TC53_PAYLOAD_SIZE >= 24 && TC53_PAYLOAD_SIZE <= 1400 )) || {
    echo "TC-53 payload size must be 24..1400 bytes" >&2; exit 2;
}
(( TC53_BATCH_SIZE >= 1 && TC53_BATCH_SIZE <= 512 )) || {
    echo "TC-53 batch size must be 1..512" >&2; exit 2;
}
(( TC53_RECEIVERS >= 1 && TC53_RECEIVERS <= 65535 )) || {
    echo "TC-53 receivers must be 1..65535" >&2; exit 2;
}
(( TC53_MAX_SPARSE_LATENCY >= 1 && TC53_MAX_SPARSE_LATENCY <= 60 &&
   TC53_DRAIN_TIMEOUT >= 1 && TC53_DRAIN_TIMEOUT <= 300 )) || {
    echo "TC-53 sparse latency must be 1..60 seconds and drain timeout 1..300" >&2; exit 2;
}
TC53_EXPECTED=$((TC53_RATE * TC53_DURATION * TC53_CYCLES + TC53_CYCLES))
(( TC53_EXPECTED <= 50000000 )) || {
    echo "TC-53 workload exceeds 50,000,000 bounded records" >&2; exit 2;
}
export TC53_RATE TC53_DURATION TC53_IDLE TC53_CYCLES TC53_PAYLOAD_SIZE TC53_BATCH_SIZE TC53_RECEIVERS
export TC53_MAX_SPARSE_LATENCY TC53_DRAIN_TIMEOUT TC53_EXPECTED COMPOSE_PROGRESS=plain

airgap_init_test 53
mkdir -p "$HERE/tmp"
WORK="$HERE/tmp/$PROJECT-work"
mkdir -m 0755 "$WORK"
mkdir -m 0755 "$WORK/bin"
LOG_BASE="${AIRGAP_TEST_LOG_DIR:-$HERE/tmp}"
mkdir -p "$LOG_BASE"
LOG_BASE="$(cd "$LOG_BASE" && pwd)"
RESULTS="$LOG_BASE/$PROJECT"
mkdir -m 0755 "$RESULTS"
export TC53_WORK="$WORK" TC53_RESULTS="$RESULTS"
STARTED=0
rc=0
DOWNSTREAM_CID=""
SAMPLER_PID=""
BEFORE_SNMP="$RESULTS/udp-snmp-before.txt"
AFTER_SNMP="$RESULTS/udp-snmp-after.txt"

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc53.yml" "$@"
}

collect_logs() {
    local service
    for service in target-kafka downstream; do
        compose logs --no-color --no-log-prefix "$service" >"$RESULTS/$service.log" 2>&1 || true
    done
}

snapshot_udp_snmp() {
    local path="$1"
    [[ -n "$DOWNSTREAM_CID" ]] || return 1
    docker exec "$DOWNSTREAM_CID" cat /proc/net/snmp >"$path" 2>&1
}

udp_rcvbuf_errors() {
    awk '
        $1 == "Udp:" && !header {
            for (i = 2; i <= NF; i++) column[$i] = i
            header = 1
            next
        }
        $1 == "Udp:" && header && ("RcvbufErrors" in column) {
            print $(column["RcvbufErrors"])
            exit
        }
    ' "$1"
}

metric_max() {
    local key="$1" path="$2"
    { grep -oE "\"${key}\":[0-9]+" "$path" 2>/dev/null || true; } |
        sed -E 's/.*://' | sort -n | tail -1
}

stop_sampler() {
    if [[ -n "$SAMPLER_PID" ]]; then
        kill "$SAMPLER_PID" 2>/dev/null || true
        wait "$SAMPLER_PID" 2>/dev/null || true
        SAMPLER_PID=""
    fi
}

cleanup() {
    local rc=$?
    trap - EXIT
    stop_sampler
    if (( STARTED )); then
        [[ -s "$AFTER_SNMP" ]] || snapshot_udp_snmp "$AFTER_SNMP" || true
        collect_logs
        compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    fi
    rm -rf "$WORK"
    echo "TC-53 results retained at $RESULTS"
    report_summary "TC-53 UDP receiver batching regression" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

command -v docker >/dev/null || { echo "TC-53 requires Docker" >&2; exit 2; }
command -v go >/dev/null || { echo "TC-53 requires Go" >&2; exit 2; }
cd "$ROOT"
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-53: unsupported Docker architecture" >&2; exit 2 ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/downstream" ./src/cmd/downstream
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/airgap-load" ./tests/env/lib/loadgen
chmod 0755 "$WORK/bin/downstream" "$WORK/airgap-load"
go version -m "$WORK/bin/downstream" >"$RESULTS/downstream-build.txt"
go version -m "$WORK/airgap-load" >"$RESULTS/driver-build.txt"
git rev-parse HEAD >"$RESULTS/harness-revision.txt"
docker info --format 'CPUs={{.NCPU}} memory={{.MemTotal}} architecture={{.Architecture}}' >"$RESULTS/docker-host.txt"
cat >"$WORK/downstream.properties" <<EOF
id=TC53-DS
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=udp
target=kafka
bootstrapServers=target-kafka:9092
topic=transfer
topicTranslations={"stress":"transfer"}
internalTopic=airgap-internal
clientId=tc53-downstream
mtu=1500
numReceivers=$TC53_RECEIVERS
channelBufferSize=65536
batchSize=$TC53_BATCH_SIZE
readBufferMultiplier=32
rcvBufSize=4194304
enableRxqOvfl=true
logLevel=INFO
logStatistics=1
EOF
cp "$WORK/downstream.properties" "$RESULTS/"
printf 'rate=%s/s burst=%ss idle=%ss cycles=%s payload=%s bytes batchSize=%s receivers=%s expected=%s\n' \
    "$TC53_RATE" "$TC53_DURATION" "$TC53_IDLE" "$TC53_CYCLES" "$TC53_PAYLOAD_SIZE" \
    "$TC53_BATCH_SIZE" "$TC53_RECEIVERS" "$TC53_EXPECTED" | tee "$RESULTS/workload.txt"

STARTED=1
compose up -d --wait target-kafka
for topic in transfer airgap-internal; do
    compose exec -T target-kafka kafka-topics --bootstrap-server localhost:9092 --create \
        --topic "$topic" --partitions 10 --config retention.bytes=536870912 \
        --config segment.bytes=16777216
done
compose up -d downstream
DOWNSTREAM_CID="$(compose ps -q downstream)"
[[ -n "$DOWNSTREAM_CID" ]] || { echo "TC-53 downstream container did not start" >&2; exit 1; }
deadline=$((SECONDS + 90))
socket_ready_re='UDP socket [0-9]+: SO_RCVBUF requested=[0-9]+ effective=[0-9]+, batch=[1-9][0-9]*, queue=[1-9][0-9]* (batches|datagrams)'
while (( SECONDS < deadline )); do
    compose logs --no-color downstream >"$RESULTS/downstream-ready.log" 2>&1 || true
    ready_count="$(grep -Ec "$socket_ready_re" "$RESULTS/downstream-ready.log" || true)"
    if [[ "$ready_count" == "$TC53_RECEIVERS" ]]; then
        break
    fi
    if [[ "$(docker inspect -f '{{.State.Running}}' "$DOWNSTREAM_CID" 2>/dev/null || true)" != true ]]; then
        echo "TC-53 downstream exited before binding its UDP receiver" >&2
        cat "$RESULTS/downstream-ready.log" >&2
        exit 1
    fi
    sleep 1
done
grep -Eq "$socket_ready_re" "$RESULTS/downstream-ready.log" || {
    echo "TC-53 timed out waiting for the bound UDP socket readiness report" >&2
    exit 1
}
bound_sockets="$(grep -Ec "$socket_ready_re" "$RESULTS/downstream-ready.log" || true)"
if [[ "$bound_sockets" == "$TC53_RECEIVERS" ]]; then
    report_check "Bound downstream UDP sockets" "exactly $TC53_RECEIVERS with receive batching configured" \
        "$bound_sockets sockets reported ready" 0
else
    report_check "Bound downstream UDP sockets" "exactly $TC53_RECEIVERS with receive batching configured" \
        "$bound_sockets sockets reported ready" 1 || rc=1
fi
snapshot_udp_snmp "$BEFORE_SNMP"
before_rcvbuf="$(udp_rcvbuf_errors "$BEFORE_SNMP")"
[[ "$before_rcvbuf" =~ ^[0-9]+$ ]] || { echo "TC-53 could not read initial Udp:RcvbufErrors" >&2; exit 1; }

KAFKA_CID="$(compose ps -q target-kafka)"
[[ -n "$KAFKA_CID" ]] || { echo "TC-53 Kafka container is missing" >&2; exit 1; }
(
    while true; do
        docker stats --no-stream --format '{{json .}}' "$DOWNSTREAM_CID" "$KAFKA_CID"
        sleep 1
    done
) >"$RESULTS/docker-stats.jsonl" 2>"$RESULTS/docker-stats-errors.log" &
SAMPLER_PID=$!

if compose run --rm --no-deps driver 2>&1 | tee "$RESULTS/driver.log"; then
    report_check "UDP workload and exact Kafka verification" "$TC53_EXPECTED unique valid records" \
        "driver verified all records" 0
else
    report_check "UDP workload and exact Kafka verification" "$TC53_EXPECTED unique valid records" \
        "driver failed; see $RESULTS/driver.log" 1 || true
    rc=1
fi

stop_sampler
snapshot_udp_snmp "$AFTER_SNMP" || { echo "TC-53 could not capture final UDP socket counters" >&2; rc=1; }
collect_logs
after_rcvbuf="$(udp_rcvbuf_errors "$AFTER_SNMP")"
if [[ "$after_rcvbuf" =~ ^[0-9]+$ ]]; then
    rcvbuf_delta=$((after_rcvbuf - before_rcvbuf))
    if (( rcvbuf_delta == 0 )); then
        report_check "Receiver namespace Udp:RcvbufErrors" "delta 0" "before=$before_rcvbuf after=$after_rcvbuf" 0
    else
        report_check "Receiver namespace Udp:RcvbufErrors" "delta 0" "before=$before_rcvbuf after=$after_rcvbuf delta=$rcvbuf_delta" 1 || rc=1
    fi
else
    report_check "Receiver namespace Udp:RcvbufErrors" "readable before and after" "final counter missing" 1 || rc=1
fi

stats_log="$RESULTS/downstream.log"
rxq_lines="$(grep -F 'STATISTICS:' "$stats_log" | grep -F '"SO_RXQ_OVFL":' | grep -F '"SO_RXQ_OVFL_TOTAL":' || true)"
if [[ -n "$rxq_lines" ]]; then
    rxq_delta_max="$(printf '%s\n' "$rxq_lines" | metric_max SO_RXQ_OVFL /dev/stdin)"
    rxq_total_max="$(printf '%s\n' "$rxq_lines" | metric_max SO_RXQ_OVFL_TOTAL /dev/stdin)"
    if [[ "$rxq_delta_max" =~ ^[0-9]+$ && "$rxq_total_max" =~ ^[0-9]+$ &&
          "$rxq_delta_max" == 0 && "$rxq_total_max" == 0 ]]; then
        report_check "SO_RXQ_OVFL socket telemetry" "present with zero drops" "SO_RXQ_OVFL and total observed" 0
    else
        report_check "SO_RXQ_OVFL socket telemetry" "present with zero drops" \
            "delta=${rxq_delta_max:-unparsed} cumulative=${rxq_total_max:-unparsed}" 1 || rc=1
    fi
else
    report_check "SO_RXQ_OVFL socket telemetry" "delta and cumulative counters present" "missing from downstream statistics" 1 || rc=1
fi

read_datagrams="$(metric_max udp_read_datagrams_total "$stats_log")"
read_batches="$(metric_max udp_read_batches_total "$stats_log")"
if [[ "$read_datagrams" =~ ^[0-9]+$ && "$read_batches" =~ ^[0-9]+$ ]] &&
    (( read_datagrams >= TC53_EXPECTED && read_batches > 0 )) &&
    grep -qF '"udp_queue_batches_high_water":' "$stats_log"; then
    report_check "UDP receive-batch telemetry" "datagrams >= $TC53_EXPECTED and batch/queue metrics present" \
        "datagrams=$read_datagrams batches=$read_batches" 0
else
    report_check "UDP receive-batch telemetry" "datagrams >= $TC53_EXPECTED and batch/queue metrics present" \
        "datagrams=${read_datagrams:-missing} batches=${read_batches:-missing}" 1 || rc=1
fi

state="$(docker inspect -f '{{.State.Running}}/{{.State.OOMKilled}}/{{.RestartCount}}' "$DOWNSTREAM_CID" 2>/dev/null || true)"
if [[ "$state" == "true/false/0" ]]; then
    report_check "Downstream survival" "running without OOM or restart" "$state" 0
else
    report_check "Downstream survival" "running without OOM or restart" "${state:-container missing}" 1 || rc=1
fi
if grep -Eq '\[(ERROR|FATAL|PANIC)\]|panic:' "$stats_log"; then
    report_check "Downstream error log" "no ERROR/FATAL/PANIC" "errors observed; see $stats_log" 1 || rc=1
else
    report_check "Downstream error log" "no ERROR/FATAL/PANIC" "none observed" 0
fi
exit "$rc"
