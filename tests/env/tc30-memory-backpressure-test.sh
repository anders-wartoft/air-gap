#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc30.XXXXXX")"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC30_WORK="$WORK"
COMPOSE_FILE="$HERE/docker-compose.tc30.yml"
CAP_BYTES=$((192 * 1024 * 1024))
PAYLOAD_BYTES=$((12 * 4 * 1024 * 1024))

compose() {
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color >&2 || true
        echo "TC-30 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if (( rc == 0 )); then
        rm -rf "$WORK"
    fi
    report_summary "TC-30 large-message memory backpressure" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "TC-30: building Linux helper and memory-bounded upstream stack"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/airgap-load" ./lib/loadgen
chmod 0755 "$WORK/airgap-load"
compose build
compose up -d kafka topic-init sink upstream

UPSTREAM_CID="$(compose ps -q upstream)"
[[ -n "$UPSTREAM_CID" ]] || { echo "upstream container did not start" >&2; exit 1; }

deadline=$((SECONDS + 60))
while (( SECONDS < deadline )); do
    if compose logs --no-color upstream 2>&1 | grep -q "up and running"; then
        break
    fi
    sleep 1
done
compose logs --no-color upstream >"$WORK/upstream-before.log" 2>&1
grep -q "up and running" "$WORK/upstream-before.log" || {
    echo "upstream did not become ready" >&2
    exit 1
}

max_memory=0
sample_memory() {
    while docker inspect "$UPSTREAM_CID" >/dev/null 2>&1; do
        current="$(docker exec "$UPSTREAM_CID" cat /sys/fs/cgroup/memory.current 2>/dev/null || echo 0)"
        if [[ "$current" =~ ^[0-9]+$ ]] && (( current > max_memory )); then
            max_memory="$current"
            printf '%s\n' "$max_memory" >"$WORK/max-memory"
        fi
        sleep 0.1
    done
}
sample_memory &
SAMPLER_PID=$!

docker exec "$UPSTREAM_CID" /airgap/work/airgap-load pressure \
    --bytes=$((64 * 1024 * 1024)) --duration=8s >"$WORK/pressure.log" 2>&1 &
PRESSURE_PID=$!

deadline=$((SECONDS + 15))
while (( SECONDS < deadline )); do
    grep -q "pressure-ready" "$WORK/pressure.log" 2>/dev/null && break
    sleep 0.1
done
grep -q "pressure-ready" "$WORK/pressure.log" || {
    echo "pressure helper did not allocate memory" >&2
    exit 1
}

compose run --rm producer | tee "$WORK/producer.log"
wait "$PRESSURE_PID"

deadline=$((SECONDS + 90))
lag=""
while (( SECONDS < deadline )); do
    lag="$(docker exec "$(compose ps -q kafka)" kafka-consumer-groups \
        --bootstrap-server kafka:9092 --group tc30-No-delay --describe 2>/dev/null |
        awk 'NR > 1 && $6 ~ /^[0-9]+$/ { total += $6; found=1 } END { if (found) print total }')"
    [[ "$lag" == "0" ]] && break
    sleep 1
done

compose logs --no-color --no-log-prefix upstream >"$WORK/upstream.log" 2>&1
restart_count="$(docker inspect --format '{{.RestartCount}}' "$UPSTREAM_CID")"
oom_killed="$(docker inspect --format '{{.State.OOMKilled}}' "$UPSTREAM_CID")"
state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID")"
sink_bytes="$(cat "$WORK/sink.bytes" 2>/dev/null || echo 0)"
max_memory="$(cat "$WORK/max-memory" 2>/dev/null || echo 0)"

kill "$SAMPLER_PID" 2>/dev/null || true
wait "$SAMPLER_PID" 2>/dev/null || true

rc=0
# The watermark events are configurable. These are the messages configured in
# upstream-airgap-30-docker.properties, rendered from their templates.
if grep -q "\[WARN\] Upstream Upstream_30 entering high memory watermark" "$WORK/upstream.log"; then
    report_check "upstream memory throttle" "high-watermark transition at the configured level" "observed" 0 || rc=1
else
    report_check "upstream memory throttle" "high-watermark transition at the configured level" "missing" 1 || rc=1
fi
if grep -q "\[INFO\] Upstream Upstream_30 leaving high memory watermark" "$WORK/upstream.log"; then
    report_check "upstream memory resume" "low-watermark transition at the configured level" "observed" 0 || rc=1
else
    report_check "upstream memory resume" "low-watermark transition at the configured level" "missing" 1 || rc=1
fi
# A placeholder that is not substituted is left as written, so any leftover
# brace in a rendered watermark message means a template variable was not
# resolved. The match is anchored on the rendered message, because the startup
# configuration echo prints the templates themselves.
if grep -E "\[(WARN|INFO)\] Upstream Upstream_30 .*memory watermark" "$WORK/upstream.log" | grep -q "{"; then
    report_check "watermark templates expanded" "no unresolved placeholders" "unresolved placeholder found" 1 || rc=1
else
    report_check "watermark templates expanded" "no unresolved placeholders" "all substituted" 0 || rc=1
fi
if [[ "$lag" == "0" ]]; then
    report_check "large Kafka records consumed" "consumer lag 0" "lag 0" 0 || rc=1
else
    report_check "large Kafka records consumed" "consumer lag 0" "lag ${lag:-unknown}" 1 || rc=1
fi
if [[ "$sink_bytes" =~ ^[0-9]+$ ]] && (( sink_bytes >= PAYLOAD_BYTES )); then
    report_check "large payload transferred" "at least $PAYLOAD_BYTES TCP bytes" "$sink_bytes bytes" 0 || rc=1
else
    report_check "large payload transferred" "at least $PAYLOAD_BYTES TCP bytes" "$sink_bytes bytes" 1 || rc=1
fi
if [[ "$restart_count" == "0" && "$oom_killed" == "false" && "$state" == "running" ]]; then
    report_check "upstream process survival" "running, zero restarts, OOMKilled=false" \
        "$state, restarts=$restart_count, OOMKilled=$oom_killed" 0 || rc=1
else
    report_check "upstream process survival" "running, zero restarts, OOMKilled=false" \
        "$state, restarts=$restart_count, OOMKilled=$oom_killed" 1 || rc=1
fi
if [[ "$max_memory" =~ ^[0-9]+$ ]] && (( max_memory > 0 && max_memory <= CAP_BYTES )); then
    report_check "cgroup memory ceiling" "peak at most $CAP_BYTES bytes" "$max_memory bytes" 0 || rc=1
else
    report_check "cgroup memory ceiling" "peak at most $CAP_BYTES bytes" "$max_memory bytes" 1 || rc=1
fi

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-30 containers retained. Artifacts: $WORK"
    report_summary "TC-30 large-message memory backpressure" "$rc"
    trap - EXIT
    exit "$rc"
fi
exit "$rc"
