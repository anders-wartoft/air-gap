#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
source "$HERE/lib/udp-calibration.sh"
case_id=52
if [[ "${TC52_PACING_COMPARISON:-0}" == 1 ]]; then
    case_id=54
    TC54_DATAGRAMS_PER_SECOND="${TC54_DATAGRAMS_PER_SECOND:-12000}"
    TC54_BYTES_PER_SECOND="${TC54_BYTES_PER_SECOND:-14000000}"
    TC54_BURST_DATAGRAMS="${TC54_BURST_DATAGRAMS:-32}"
    for value in "$TC54_DATAGRAMS_PER_SECOND" "$TC54_BYTES_PER_SECOND" "$TC54_BURST_DATAGRAMS"; do
        [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo "TC-54 requires positive integer UDP rates" >&2; exit 2; }
    done
    [[ "$TC54_BURST_DATAGRAMS" -le 256 ]] || { echo "TC-54 burst must be 1..256 datagrams" >&2; exit 2; }
fi
[[ "${TC_PAUSE:-0}" == 0 ]] || { echo "TC-$case_id does not support -pause" >&2; exit 2; }
export TC52_DURATION="${TC52_DURATION:-300}" TC52_WARMUP="${TC52_WARMUP:-20}"
export TC52_WINDOW="${TC52_WINDOW:-10000}"
TC52_BATCH_SIZE="${TC52_BATCH_SIZE:-512}"
for value in "$TC52_DURATION" "$TC52_WINDOW" "$TC52_BATCH_SIZE"; do
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo "TC-52 requires positive integer settings" >&2; exit 2; }
done
[[ "$TC52_WARMUP" =~ ^[0-9]+$ && "$TC52_BATCH_SIZE" -le 512 ]] || {
    echo "TC-52: invalid warmup or batch size (maximum 512)" >&2; exit 2;
}
if (( case_id == 54 )); then
    modes=(udp-unpaced udp-paced udp-calibrated)
else
    read -r -a modes <<< "${TC52_PROTOCOLS:-udp tcp tls}"
    for mode in "${modes[@]}"; do
        case "$mode" in udp|tcp|tls) ;; *) echo "TC-52: unknown protocol $mode" >&2; exit 2 ;; esac
    done
fi
command -v openssl >/dev/null || { echo "TC-52 requires openssl" >&2; exit 2; }
command -v python3 >/dev/null || { echo "TC-52 requires python3 for the results table" >&2; exit 2; }
airgap_init_test "$case_id"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc$case_id.XXXXXX")"
chmod 0755 "$WORK"
mkdir -m 0755 "$WORK/bin"
LOG_BASE="${AIRGAP_TEST_LOG_DIR:-$HERE/tmp}"
mkdir -p "$LOG_BASE"
LOG_BASE="$(cd "$LOG_BASE" && pwd)"
RESULTS="$(mktemp -d "$LOG_BASE/tc$case_id.XXXXXX")"
export TC52_WORK="$WORK" TC52_RESULTS="$RESULTS" COMPOSE_PROGRESS=plain
STARTED=0
SAMPLER_PID=""
STALL_PID=""
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
print_results() {
    python3 - "$RESULTS" "$case_id" "$TC52_DURATION" "${modes[@]}" <<'PY'
import json
import math
from pathlib import Path
import sys

root = Path(sys.argv[1])
comparison = sys.argv[2] == "54"
duration = int(sys.argv[3])
if comparison:
    print("| UDP pacing | Delivered EPS | p99 latency (ms) | Missing records | Socket drops (datagrams) |")
    print("|---|---:|---:|---:|---:|")
else:
    print("| Transport | Current delivered EPS |")
    print("|---|---:|")
failed = False
notes = []
for mode in sys.argv[4:]:
    path = root / mode / "summary.json"
    label = {"udp-unpaced": "Off", "udp-paced": "On (fixed settings)",
             "udp-calibrated": "On (calibrated)"}.get(mode, mode.upper())
    try:
        with path.open() as source:
            summary = json.load(source)
        eps = summary["delivered_eps"]
        if type(eps) not in (int, float) or not math.isfinite(eps) or eps < 0:
            raise ValueError("delivered_eps must be a finite nonnegative number")
        error = summary.get("errors")
        if comparison:
            missing = summary["outstanding"]
            acked, delivered = summary["source_acknowledged"], summary["total_delivered"]
            driver_exit = int(path.with_name("driver-exit.txt").read_text())
            expected = f"delivery mismatch: source acknowledged={acked} delivered={delivered} outstanding={missing}"
            if summary["measurement_seconds"] != duration or eps <= 0 or missing != acked - delivered or missing < 0:
                raise ValueError("incomplete or inconsistent workload")
            drops = None
            with path.with_name("downstream.log").open() as downstream:
                for line in downstream:
                    if "STATISTICS: " in line:
                        drops = json.loads(line.split("STATISTICS: ", 1)[1]).get("SO_RXQ_OVFL_TOTAL", drops)
            if type(drops) is not int or drops < 0:
                raise ValueError("no valid socket drop counter")
            latency = summary.get("p99_latency_ms")
            latency_display = f"{latency:,}" if latency is not None else "N/A"
            print(f"| {label} | {eps:,.0f} | {latency_display} | {missing:,} | {drops:,} |")
            if mode == "udp-unpaced" and driver_exit == 1 and missing > 0 and error == expected:
                notes.append("Off: baseline delivery loss shown in the table; paced delivery is the zero-loss gate.")
            elif error or driver_exit != 0 or missing or (mode != "udp-unpaced" and drops):
                failed = True
                notes.append(f"{label}: FAILED: {error or f'{missing} missing, {drops} socket drops, driver exit {driver_exit}'}")
        else:
            print(f"| {label} | {eps:,.0f} |")
            if error:
                failed = True
                notes.append(f"{label}: FAILED after {summary['measurement_seconds']:.2f} measured seconds: {error}")
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"| {label} | N/A |" + (" N/A | N/A | N/A |" if comparison else ""))
        print(f"TC-{sys.argv[2]} cannot read results from {path}: {error}", file=sys.stderr)
        failed = True
for note in notes:
    print(note)
sys.exit(1 if failed else 0)
PY
}
cleanup() {
    local rc=$?
    trap - EXIT
    stop_sampler
    if [[ -n "$STALL_PID" ]]; then
        kill "$STALL_PID" 2>/dev/null || true
        wait "$STALL_PID" 2>/dev/null || true
        compose unpause downstream >/dev/null 2>&1 || true
    fi
    if (( STARTED )); then
        collect_logs || rc=1
        compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    fi
    rm -f "$WORK/bin/upstream" "$WORK/bin/downstream" "$WORK/airgap-load" \
        "$WORK/server.key" "$WORK/server.crt" "$WORK/upstream.properties" "$WORK/downstream.properties"
    rmdir "$WORK/bin" "$WORK" || rc=1
    echo "TC-$case_id results retained at $RESULTS"
    print_results || rc=1
    if (( case_id == 54 )); then
        if [[ -s "$RESULTS/pacing.properties" ]]; then
            printf '\nTC-54 calibrated settings for this Docker host (preflight, no injected stall):\n'
            cat "$RESULTS/pacing.properties" || rc=1
        else
            echo "TC-54: no verified calibration settings; see the preflight logs" >&2
            rc=1
        fi
        report_summary "TC-54 UDP pacing comparison" "$rc" || true
    else
        report_summary "TC-52 end-to-end Kafka transport stress" "$rc" || true
    fi
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
if (( case_id == 54 )); then
    if [[ -z "${AIRGAP_TEST_UDP_PACING:-}" ]]; then
        echo "TC-54: running shared UDP calibration (standalone invocation)"
        bash "$HERE/calibrate-test-udp.sh" "$RESULTS/udp-calibration" "$WORK/bin" \
            >"$RESULTS/calibration-preflight.log" 2>&1
        airgap_load_udp_calibration "$RESULTS/udp-calibration/pacing.properties"
    fi
    IFS=, read -r calibrated_datagrams calibrated_bytes calibrated_burst <<<"$AIRGAP_TEST_UDP_PACING"
    printf 'udpPacingDatagramsPerSecond=%s\nudpPacingBytesPerSecond=%s\nudpPacingBurstDatagrams=%s\n' \
        "$calibrated_datagrams" "$calibrated_bytes" "$calibrated_burst" >"$RESULTS/pacing.properties"
fi
go version -m "$WORK/bin/upstream" >"$RESULTS/upstream-build.txt"
go version -m "$WORK/bin/downstream" >"$RESULTS/downstream-build.txt"
go version -m "$WORK/airgap-load" >"$RESULTS/driver-build.txt"
git rev-parse HEAD >"$RESULTS/harness-revision.txt"
docker info --format 'CPUs={{.NCPU}} memory={{.MemTotal}} architecture={{.Architecture}}' >"$RESULTS/docker-host.txt"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=downstream \
    -keyout "$WORK/server.key" -out "$WORK/server.crt" >"$RESULTS/certificate-generation.log" 2>&1
rc=0
for mode in "${modes[@]}"; do
    export TC52_RESULTS="$RESULTS/$mode"
    mkdir -p "$TC52_RESULTS"
    protocol="${mode%%-*}"
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
    if [[ "$mode" == udp-paced || "$mode" == udp-calibrated ]]; then
        pacing_datagrams="$TC54_DATAGRAMS_PER_SECOND"
        pacing_bytes="$TC54_BYTES_PER_SECOND"
        pacing_burst="$TC54_BURST_DATAGRAMS"
        if [[ "$mode" == udp-calibrated ]]; then
            pacing_datagrams="$calibrated_datagrams"
            pacing_bytes="$calibrated_bytes"
            pacing_burst="$calibrated_burst"
        fi
        cat >>"$WORK/upstream.properties" <<EOF
udpPacingDatagramsPerSecond=$pacing_datagrams
udpPacingBytesPerSecond=$pacing_bytes
udpPacingBurstDatagrams=$pacing_burst
EOF
    elif [[ "$mode" == udp-unpaced ]]; then
        printf 'udpPacingDatagramsPerSecond=0\nudpPacingBytesPerSecond=0\n' >>"$WORK/upstream.properties"
    fi
    airgap_write_stress_downstream_config "$transport" "$TC52_BATCH_SIZE" >"$WORK/downstream.properties"
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
    if [[ "$mode" == udp-paced || "$mode" == udp-calibrated ]]; then
        grep -Fq "UDP pacing: $pacing_datagrams datagrams/s, $pacing_bytes payload bytes/s, burst $pacing_burst datagrams" \
            "$TC52_RESULTS/upstream-ready.log" || { echo "TC-54 upstream pacing settings not active" >&2; exit 1; }
    elif [[ "$mode" == udp-unpaced ]]; then
        grep -Fq 'UDP pacing: 0 datagrams/s, 0 payload bytes/s,' \
            "$TC52_RESULTS/upstream-ready.log" || { echo "TC-54 baseline pacing is not disabled" >&2; exit 1; }
    fi
    ids="$(compose ps -q source-kafka target-kafka upstream downstream)"
    (
        while true; do
            docker stats --no-stream --format '{{json .}}' $ids
            sleep 1
        done
    ) >"$TC52_RESULTS/docker-stats.jsonl" 2>"$TC52_RESULTS/docker-stats-errors.log" &
    SAMPLER_PID=$!
    if (( case_id == 54 )); then
        (
            deadline=$((SECONDS + TC52_WARMUP + 20))
            until grep -q '"phase":"measure"' "$TC52_RESULTS/driver.log" 2>/dev/null; do
                (( SECONDS < deadline )) || { echo "No measurement sample before stall deadline"; exit 1; }
                sleep 0.1
            done
            compose pause downstream && sleep 0.02 && compose unpause downstream
        ) >"$TC52_RESULTS/stall.log" 2>&1 &
        STALL_PID=$!
    fi
    if compose run --rm --no-deps driver 2>&1 | tee "$TC52_RESULTS/driver.log"; then
        printf '0\n' >"$TC52_RESULTS/driver-exit.txt"
        report_check "$mode end-to-end stress" "all acknowledged records consumed from downstream Kafka" "verified" 0
    else
        pipeline_status=("${PIPESTATUS[@]}")
        printf '%s\n' "${pipeline_status[0]}" >"$TC52_RESULTS/driver-exit.txt"
        if [[ "$mode" == udp-unpaced && "${pipeline_status[1]}" == 0 ]]; then
            echo "TC-54: unpaced baseline did not deliver cleanly; checking final result"
        else
            report_check "$mode end-to-end stress" "all acknowledged records consumed from downstream Kafka" "FAILED; see $TC52_RESULTS" 1 || true
            rc=1
        fi
    fi
    if [[ -n "$STALL_PID" ]]; then
        if ! wait "$STALL_PID"; then
            echo "TC-54: downstream stall injection failed; see $TC52_RESULTS/stall.log" >&2
            compose unpause downstream >/dev/null 2>&1 || true
            rc=1
        fi
        STALL_PID=""
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
