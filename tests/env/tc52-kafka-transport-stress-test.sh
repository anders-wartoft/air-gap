#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
[[ "${TC_PAUSE:-0}" == 0 ]] || { echo "TC-52 does not support -pause" >&2; exit 2; }
export TC52_DURATION="${TC52_DURATION:-300}" TC52_WARMUP="${TC52_WARMUP:-20}"
export TC52_WINDOW="${TC52_WINDOW:-10000}"
TC52_BATCH_SIZE="${TC52_BATCH_SIZE:-512}"
for value in "$TC52_DURATION" "$TC52_WINDOW" "$TC52_BATCH_SIZE"; do
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo "TC-52 requires positive integer settings" >&2; exit 2; }
done
[[ "$TC52_WARMUP" =~ ^[0-9]+$ && "$TC52_BATCH_SIZE" -le 512 ]] || {
    echo "TC-52: invalid warmup or batch size (maximum 512)" >&2; exit 2;
}
read -r -a protocols <<< "${TC52_PROTOCOLS:-udp tcp tls}"
for protocol in "${protocols[@]}"; do
    case "$protocol" in udp|tcp|tls) ;; *) echo "TC-52: unknown protocol $protocol" >&2; exit 2 ;; esac
done
command -v openssl >/dev/null || { echo "TC-52 requires openssl" >&2; exit 2; }
airgap_init_test 52
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc52.XXXXXX")"
chmod 0755 "$WORK"
mkdir -m 0755 "$WORK/bin"
LOG_BASE="${AIRGAP_TEST_LOG_DIR:-$HERE/tmp}"
mkdir -p "$LOG_BASE"
LOG_BASE="$(cd "$LOG_BASE" && pwd)"
RESULTS="$(mktemp -d "$LOG_BASE/tc52.XXXXXX")"
export TC52_WORK="$WORK" TC52_RESULTS="$RESULTS" COMPOSE_PROGRESS=plain
STARTED=0
SAMPLER_PID=""
compose() { docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc52.yml" "$@"; }
stop_sampler() {
    if [[ -n "$SAMPLER_PID" ]]; then
        kill "$SAMPLER_PID" 2>/dev/null || true
        wait "$SAMPLER_PID" 2>/dev/null || true
        SAMPLER_PID=""
    fi
}
collect_logs() {
    local service
    for service in source-kafka target-kafka upstream downstream; do
        compose logs --no-color --no-log-prefix "$service" >"$TC52_RESULTS/$service.log" 2>&1
    done
}
cleanup() {
    local rc=$?
    trap - EXIT
    stop_sampler
    if (( STARTED )); then
        collect_logs || rc=1
        compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    fi
    rm -f "$WORK/bin/upstream" "$WORK/bin/downstream" "$WORK/airgap-load" \
        "$WORK/server.key" "$WORK/server.crt" "$WORK/upstream.properties" "$WORK/downstream.properties"
    rmdir "$WORK/bin" "$WORK" || rc=1
    echo "TC-52 results retained at $RESULTS"
    report_summary "TC-52 end-to-end Kafka transport stress" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
cd "$ROOT"
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-52: unsupported Docker architecture" >&2; exit 2 ;;
esac
if [[ -n "${TC52_BIN_DIR:-}" ]]; then
    cp "$TC52_BIN_DIR/upstream" "$TC52_BIN_DIR/downstream" "$WORK/bin/"
else
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/" ./src/cmd/upstream ./src/cmd/downstream
fi
if [[ -n "${TC52_LOAD_BIN:-}" ]]; then
    cp "$TC52_LOAD_BIN" "$WORK/airgap-load"
else
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/airgap-load" ./tests/env/lib/loadgen
fi
chmod 0755 "$WORK/bin/upstream" "$WORK/bin/downstream" "$WORK/airgap-load"
go version -m "$WORK/bin/upstream" >"$RESULTS/upstream-build.txt"
go version -m "$WORK/bin/downstream" >"$RESULTS/downstream-build.txt"
go version -m "$WORK/airgap-load" >"$RESULTS/driver-build.txt"
git rev-parse HEAD >"$RESULTS/harness-revision.txt"
docker info --format 'CPUs={{.NCPU}} memory={{.MemTotal}} architecture={{.Architecture}}' >"$RESULTS/docker-host.txt"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=downstream \
    -keyout "$WORK/server.key" -out "$WORK/server.crt" >"$RESULTS/certificate-generation.log" 2>&1
rc=0
for protocol in "${protocols[@]}"; do
    export TC52_RESULTS="$RESULTS/$protocol"
    mkdir -p "$TC52_RESULTS"
    transport="$protocol"
    [[ "$protocol" != tls ]] || transport=tcp
    cat >"$WORK/upstream.properties" <<EOF
id=TC52-US
nic=eth0
targetIP=downstream
targetPort=1234
transport=$transport
source=kafka
bootstrapServers=source-kafka:9092
topic=stress
groupID=tc52
from=1970-01-01T00:00:00+00:00
sendingThreads=[{"now":0}]
encryption=false
payloadSize=1400
compressWhenLengthExceeds=0
eps=-1
logLevel=INFO
logStatistics=1
tcpRetryTimes=0
tcpRetryInterval=100
EOF
    cat >"$WORK/downstream.properties" <<EOF
id=TC52-DS
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=$transport
target=kafka
bootstrapServers=target-kafka:9092
topic=transfer
topicTranslations={"stress":"transfer"}
internalTopic=airgap-internal
clientId=tc52-downstream
mtu=1500
numReceivers=10
channelBufferSize=65536
batchSize=$TC52_BATCH_SIZE
readBufferMultiplier=32
rcvBufSize=4194304
logLevel=INFO
logStatistics=1
EOF
    if [[ "$protocol" == tls ]]; then
        cat >>"$WORK/upstream.properties" <<EOF
tcpTLSEnabled=true
tcpTLSCAFile=/airgap/work/server.crt
tcpTLSServerCNRegex=^downstream$
EOF
        cat >>"$WORK/downstream.properties" <<EOF
tcpTLSCertFile=/airgap/work/server.crt
tcpTLSKeyFile=/airgap/work/server.key
EOF
    fi
    cp "$WORK/upstream.properties" "$WORK/downstream.properties" "$TC52_RESULTS/"
    printf 'protocol=%s partitions=10 sendingThreads=1 payload=400-1600 duration=%ss warmup=%ss window=%s batchSize=%s\n' \
        "$protocol" "$TC52_DURATION" "$TC52_WARMUP" "$TC52_WINDOW" "$TC52_BATCH_SIZE" | tee "$TC52_RESULTS/workload.txt"
    STARTED=1
    compose up -d --wait source-kafka target-kafka
    compose exec -T source-kafka kafka-topics --bootstrap-server localhost:9092 --create --topic stress --partitions 10 \
        --config retention.bytes=104857600 --config segment.bytes=16777216
    for topic in transfer airgap-internal; do
        compose exec -T target-kafka kafka-topics --bootstrap-server localhost:9092 --create --topic "$topic" --partitions 10 \
            --config retention.bytes=104857600 --config segment.bytes=16777216
    done
    compose up -d downstream
    deadline=$((SECONDS + 90))
    while true; do
        compose logs --no-color downstream >"$TC52_RESULTS/downstream-ready.log"
        grep -Eq 'TCP listener started|UDP listener starting' "$TC52_RESULTS/downstream-ready.log" && break
        (( SECONDS < deadline )) || { echo "TC-52 downstream did not become ready" >&2; exit 1; }
        sleep 1
    done
    compose up -d upstream
    deadline=$((SECONDS + 90))
    while true; do
        compose logs --no-color upstream >"$TC52_RESULTS/upstream-ready.log"
        grep -q 'now up and running' "$TC52_RESULTS/upstream-ready.log" && break
        (( SECONDS < deadline )) || { echo "TC-52 upstream did not become ready" >&2; exit 1; }
        sleep 1
    done
    ids="$(compose ps -q source-kafka target-kafka upstream downstream)"
    (
        while true; do
            docker stats --no-stream --format '{{json .}}' $ids
            sleep 1
        done
    ) >"$TC52_RESULTS/docker-stats.jsonl" 2>"$TC52_RESULTS/docker-stats-errors.log" &
    SAMPLER_PID=$!
    if compose run --rm --no-deps driver 2>&1 | tee "$TC52_RESULTS/driver.log"; then
        report_check "$protocol end-to-end stress" "all acknowledged records consumed from downstream Kafka" "verified" 0
    else
        report_check "$protocol end-to-end stress" "all acknowledged records consumed from downstream Kafka" "FAILED; see $TC52_RESULTS" 1 || true
        rc=1
    fi
    stop_sampler
    collect_logs
    for service in upstream downstream; do
        cid="$(compose ps -q "$service")"
        if [[ -z "$cid" ]] || [[ "$(docker inspect -f '{{.State.Running}}/{{.State.OOMKilled}}/{{.RestartCount}}' "$cid")" != true/false/0 ]]; then
            report_check "$protocol $service survival" "running without OOM/restarts" "FAILED" 1 || true
            rc=1
        fi
        if grep -Eq '\[(ERROR|FATAL|PANIC)\]|panic:' "$TC52_RESULTS/$service.log"; then
            report_check "$protocol $service errors" "no ERROR/FATAL/PANIC" "observed; see service log" 1 || true
            rc=1
        fi
    done
    compose down -v --remove-orphans
    STARTED=0
done
exit "$rc"
