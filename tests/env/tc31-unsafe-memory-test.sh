#!/usr/bin/env bash
# TC-31 is a negative control for TC-30. It runs the same stack with
# deliberately permissive consumer memory settings and passes only when
# upstream is actually killed for exceeding its cgroup. If this test starts
# failing, the bounds in TC-30 are no longer what keeps upstream alive.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc31.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC31_WORK="$WORK"
COMPOSE_FILE="$HERE/docker-compose.tc31.yml"
CAP_BYTES=$((192 * 1024 * 1024))

compose() {
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color >&2 || true
        echo "TC-31 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if (( rc == 0 )); then
        rm -rf "$WORK"
    fi
    report_summary "TC-31 unsafe memory configuration control" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "TC-31: building Linux helper and deliberately unbounded upstream stack"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/airgap-load" ./lib/loadgen
chmod 0755 "$WORK/airgap-load"
compose build
compose up -d kafka topic-init stall upstream

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

# Half the pressure TC-30 survives. The point of the control is that the
# unsafe configuration dies under conditions the bounded configuration handles
# with room to spare.
echo "TC-31: applying 32 MiB of cgroup pressure, half of what TC-30 survives"
docker exec "$UPSTREAM_CID" /airgap/work/airgap-load pressure \
    --bytes=$((32 * 1024 * 1024)) --duration=180s >"$WORK/pressure.log" 2>&1 &
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

# Sampling starts before the producer: the kill arrives within a second or two
# of the first fetch, so a sampler started afterwards would never see a value.
: >"$WORK/memory.log"
(
    while :; do
        docker exec "$UPSTREAM_CID" cat /sys/fs/cgroup/memory.current 2>/dev/null >>"$WORK/memory.log" || break
        sleep 0.1
    done
) &
SAMPLER_PID=$!

echo "TC-31: producing 60 records of 4 MiB with a stalled receiver"
compose run --rm producer | tee "$WORK/producer.log"

deadline=$((SECONDS + 180))
state="running"
while (( SECONDS < deadline )); do
    state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID" 2>/dev/null || echo gone)"
    [[ "$state" == "running" ]] || break
    sleep 0.5
done
kill "$SAMPLER_PID" 2>/dev/null || true
wait "$SAMPLER_PID" 2>/dev/null || true
kill "$PRESSURE_PID" 2>/dev/null || true
wait "$PRESSURE_PID" 2>/dev/null || true

max_memory="$(sort -n "$WORK/memory.log" | tail -1)"
[[ "$max_memory" =~ ^[0-9]+$ ]] || max_memory=0

compose logs --no-color --no-log-prefix upstream >"$WORK/upstream.log" 2>&1
oom_killed="$(docker inspect --format '{{.State.OOMKilled}}' "$UPSTREAM_CID" 2>/dev/null || echo unknown)"
exit_code="$(docker inspect --format '{{.State.ExitCode}}' "$UPSTREAM_CID" 2>/dev/null || echo unknown)"
state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID" 2>/dev/null || echo gone)"

rc=0
if [[ "$oom_killed" == "true" || "$exit_code" == "137" ]]; then
    report_check "unbounded consumer is killed" "OOMKilled=true or exit code 137" \
        "OOMKilled=$oom_killed, exit=$exit_code" 0 || rc=1
else
    report_check "unbounded consumer is killed" "OOMKilled=true or exit code 137" \
        "OOMKilled=$oom_killed, exit=$exit_code, state=$state" 1 || rc=1
fi
if [[ "$state" == "exited" || "$state" == "dead" ]]; then
    report_check "upstream did not survive" "container exited" "$state" 0 || rc=1
else
    report_check "upstream did not survive" "container exited" "$state" 1 || rc=1
fi
if [[ "$max_memory" =~ ^[0-9]+$ ]] && (( max_memory > CAP_BYTES / 2 )); then
    report_check "memory grew towards the cgroup cap" "peak above $((CAP_BYTES / 2)) bytes" \
        "$max_memory bytes" 0 || rc=1
else
    report_check "memory grew towards the cgroup cap" "peak above $((CAP_BYTES / 2)) bytes" \
        "$max_memory bytes" 1 || rc=1
fi
if grep -q "entering high memory watermark" "$WORK/upstream.log"; then
    report_check "watermark cannot save an oversized buffer" "throttle did not prevent the kill" \
        "throttled and still killed" 0 || rc=1
else
    report_check "watermark cannot save an oversized buffer" "throttle did not prevent the kill" \
        "killed before the watermark was reached" 0 || rc=1
fi

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-31 containers retained. Artifacts: $WORK"
    report_summary "TC-31 unsafe memory configuration control" "$rc"
    trap - EXIT
    exit "$rc"
fi
exit "$rc"
