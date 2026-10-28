#!/usr/bin/env bash
# TC-35 verifies that reopening the log after logrotate closes the descriptor
# of the rotated file, so a deleted log actually frees its disk space, and that
# the Kafka client writes to the new log rather than the rotated one.
#
# The negative control at the end runs a binary built with the non-default
# airgap_logrotate_leak tag, which keeps the replaced descriptor open. It must
# be detected as leaking, otherwise the checks above prove nothing.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc35.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC_WORK="$WORK"
export TC35_LEAK_BIN="$WORK/leak-bin"
COMPOSE_BASE="$HERE/docker-compose.kafka-transfer.yml"
COMPOSE_OVERRIDE="$HERE/docker-compose.tc35.yml"
ROTATIONS=3

compose() {
    docker compose -p "$PROJECT" -f "$COMPOSE_BASE" -f "$COMPOSE_OVERRIDE" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose --profile rebalance --profile leak-control logs --no-color >&2 || true
        echo "TC-35 artifacts retained at $WORK" >&2
    fi
    compose --profile rebalance --profile leak-control down -v --remove-orphans >/dev/null 2>&1 || true
    if (( rc == 0 )); then
        rm -rf "$WORK"
    fi
    report_summary "TC-35 log rotation closes replaced file descriptors" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# log_fd_count <container> <path-glob> prints how many descriptors of PID 1
# resolve to a path matching the glob. A rotated file that logrotate removed
# resolves to "<path> (deleted)", which the glob still matches.
log_fd_count() {
    local cid="$1" pattern="$2"
    docker exec "$cid" bash -c '
        shopt -s nullglob
        count=0
        for fd in /proc/1/fd/*; do
            target="$(readlink "$fd" 2>/dev/null || true)"
            case "$target" in
                '"$pattern"') count=$((count + 1)) ;;
            esac
        done
        printf "%s" "$count"
    '
}

wait_for_log_line() {
    local cid="$1" path="$2" pattern="$3" timeout="${4:-60}"
    local deadline=$((SECONDS + timeout))
    while (( SECONDS < deadline )); do
        if docker exec "$cid" grep -qa -- "$pattern" "$path" 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# rotate_and_reload performs exactly what logrotate does: rename the live log,
# signal the process, then compress the rotated file.
rotate_and_reload() {
    local cid="$1" path="$2" rotated="$3"
    docker exec "$cid" mv "$path" "$rotated"
    docker exec "$cid" kill -HUP 1
    wait_for_log_line "$cid" "$path" "SIGHUP handling completed" 60
}

echo "TC-35: building Linux binaries, including the leaking negative control"
mkdir -p ../../target/linux-amd64 "$TC35_LEAK_BIN"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../../target/linux-amd64/upstream ../../src/cmd/upstream
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../../target/linux-amd64/downstream ../../src/cmd/downstream
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags airgap_logrotate_leak \
    -o "$TC35_LEAK_BIN/downstream" ../../src/cmd/downstream
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/airgap-load" ./lib/loadgen
chmod 0755 "$WORK/airgap-load" "$TC35_LEAK_BIN/downstream"
compose build
compose up -d kafka topic-init downstream upstream

UPSTREAM_CID="$(compose ps -q upstream)"
DOWNSTREAM_CID="$(compose ps -q downstream)"
KAFKA_CID="$(compose ps -q kafka)"
[[ -n "$UPSTREAM_CID" && -n "$DOWNSTREAM_CID" && -n "$KAFKA_CID" ]] || {
    echo "TC-35 containers did not start" >&2
    exit 1
}

UPSTREAM_LOG=/var/log/airgap/upstream.log
DOWNSTREAM_LOG=/var/log/airgap/downstream.log

wait_for_log_line "$UPSTREAM_CID" "$UPSTREAM_LOG" "up and running" 90 || {
    echo "upstream did not become ready" >&2
    exit 1
}
wait_for_log_line "$DOWNSTREAM_CID" "$DOWNSTREAM_LOG" "Log to file started up" 90 || {
    echo "downstream did not become ready" >&2
    exit 1
}

echo "TC-35: producing records so both roles have an active Kafka client"
compose run --rm producer | tee "$WORK/producer.log"

deadline=$((SECONDS + 60))
lag=""
while (( SECONDS < deadline )); do
    lag="$(docker exec "$KAFKA_CID" kafka-consumer-groups \
        --bootstrap-server kafka:9092 --group tc35-Rotate --describe 2>/dev/null |
        awk 'NR > 1 && $6 ~ /^[0-9]+$/ { total += $6; found=1 } END { if (found) print total }')"
    [[ "$lag" == "0" ]] && break
    sleep 1
done

rc=0
if [[ "$lag" == "0" ]]; then
    report_check "TC-35 transfer before rotation" "consumer lag 0" "lag 0" 0 || rc=1
else
    report_check "TC-35 transfer before rotation" "consumer lag 0" "lag ${lag:-unknown}" 1 || rc=1
fi

initial_upstream_fds="$(log_fd_count "$UPSTREAM_CID" '/var/log/airgap/upstream.log')"
initial_downstream_fds="$(log_fd_count "$DOWNSTREAM_CID" '/var/log/airgap/downstream.log')"
if [[ "$initial_upstream_fds" == "1" && "$initial_downstream_fds" == "1" ]]; then
    report_check "TC-35 descriptors before rotation" "exactly 1 per process" \
        "upstream=$initial_upstream_fds downstream=$initial_downstream_fds" 0 || rc=1
else
    report_check "TC-35 descriptors before rotation" "exactly 1 per process" \
        "upstream=$initial_upstream_fds downstream=$initial_downstream_fds" 1 || rc=1
fi

echo "TC-35: rotating and reloading ${ROTATIONS} times, as nightly logrotate does"
for ((cycle = 1; cycle <= ROTATIONS; cycle++)); do
    for role in upstream downstream; do
        if [[ "$role" == upstream ]]; then
            cid="$UPSTREAM_CID"; path="$UPSTREAM_LOG"
        else
            cid="$DOWNSTREAM_CID"; path="$DOWNSTREAM_LOG"
        fi
        rotated="${path}.${cycle}"
        if ! rotate_and_reload "$cid" "$path" "$rotated"; then
            report_check "TC-35 ${role} reload cycle ${cycle}" "SIGHUP reopens the log" "no reload record" 1 || rc=1
            continue
        fi
        # logrotate compresses the rotated file after the reload.
        docker exec "$cid" gzip -f "$rotated"
    done
done

upstream_active_fds="$(log_fd_count "$UPSTREAM_CID" '/var/log/airgap/upstream.log')"
downstream_active_fds="$(log_fd_count "$DOWNSTREAM_CID" '/var/log/airgap/downstream.log')"
upstream_rotated_fds="$(log_fd_count "$UPSTREAM_CID" '/var/log/airgap/upstream.log.*')"
downstream_rotated_fds="$(log_fd_count "$DOWNSTREAM_CID" '/var/log/airgap/downstream.log.*')"

if [[ "$upstream_active_fds" == "1" && "$downstream_active_fds" == "1" ]]; then
    report_check "TC-35 descriptors after ${ROTATIONS} rotations" "exactly 1 active log per process" \
        "upstream=$upstream_active_fds downstream=$downstream_active_fds" 0 || rc=1
else
    report_check "TC-35 descriptors after ${ROTATIONS} rotations" "exactly 1 active log per process" \
        "upstream=$upstream_active_fds downstream=$downstream_active_fds" 1 || rc=1
fi
if [[ "$upstream_rotated_fds" == "0" && "$downstream_rotated_fds" == "0" ]]; then
    report_check "TC-35 rotated-log descriptors" "none retained" \
        "upstream=$upstream_rotated_fds downstream=$downstream_rotated_fds" 0 || rc=1
else
    report_check "TC-35 rotated-log descriptors" "none retained" \
        "upstream=$upstream_rotated_fds downstream=$downstream_rotated_fds" 1 || rc=1
fi

if docker exec "$UPSTREAM_CID" grep -qa "SIGHUP handling completed" "$UPSTREAM_LOG" &&
   docker exec "$DOWNSTREAM_CID" grep -qa "SIGHUP handling completed" "$DOWNSTREAM_LOG"; then
    report_check "TC-35 application records after reload" "written to the new log" "present" 0 || rc=1
else
    report_check "TC-35 application records after reload" "written to the new log" "missing" 1 || rc=1
fi

echo "TC-35: forcing a consumer-group rebalance so the Kafka client logs again"
rotated_digest_before="$(docker exec "$UPSTREAM_CID" sha256sum "${UPSTREAM_LOG}.${ROTATIONS}.gz" | awk '{print $1}')"
compose --profile rebalance up -d upstream-b
UPSTREAM_B_CID="$(compose --profile rebalance ps -q upstream-b)"
[[ -n "$UPSTREAM_B_CID" ]] || { echo "upstream-b did not start" >&2; exit 1; }

sarama_after_rotation=1
if wait_for_log_line "$UPSTREAM_CID" "$UPSTREAM_LOG" "\[SARAMA\]" 120; then
    sarama_after_rotation=0
fi
rotated_digest_after="$(docker exec "$UPSTREAM_CID" sha256sum "${UPSTREAM_LOG}.${ROTATIONS}.gz" | awk '{print $1}')"

if (( sarama_after_rotation == 0 )); then
    report_check "TC-35 Kafka client records after rotation" "written to the new log" "present" 0 || rc=1
else
    report_check "TC-35 Kafka client records after rotation" "written to the new log" "missing" 1 || rc=1
fi
if [[ "$rotated_digest_before" == "$rotated_digest_after" ]]; then
    report_check "TC-35 rotated log after reload" "never written to again" "unchanged" 0 || rc=1
else
    report_check "TC-35 rotated log after reload" "never written to again" "modified" 1 || rc=1
fi

upstream_restarts="$(docker inspect --format '{{.RestartCount}}' "$UPSTREAM_CID")"
downstream_restarts="$(docker inspect --format '{{.RestartCount}}' "$DOWNSTREAM_CID")"
upstream_state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID")"
downstream_state="$(docker inspect --format '{{.State.Status}}' "$DOWNSTREAM_CID")"
if [[ "$upstream_restarts" == "0" && "$downstream_restarts" == "0" &&
      "$upstream_state" == "running" && "$downstream_state" == "running" ]]; then
    report_check "TC-35 process survival" "both running with zero restarts" \
        "upstream=$upstream_state/$upstream_restarts downstream=$downstream_state/$downstream_restarts" 0 || rc=1
else
    report_check "TC-35 process survival" "both running with zero restarts" \
        "upstream=$upstream_state/$upstream_restarts downstream=$downstream_state/$downstream_restarts" 1 || rc=1
fi

echo "TC-35 negative control: one rotation of a deliberately leaking build"
compose --profile leak-control up -d downstream-leak
LEAK_CID="$(compose --profile leak-control ps -q downstream-leak)"
[[ -n "$LEAK_CID" ]] || { echo "downstream-leak did not start" >&2; exit 1; }

if wait_for_log_line "$LEAK_CID" "$DOWNSTREAM_LOG" "Log to file started up" 90 &&
   rotate_and_reload "$LEAK_CID" "$DOWNSTREAM_LOG" "${DOWNSTREAM_LOG}.1"; then
    docker exec "$LEAK_CID" gzip -f "${DOWNSTREAM_LOG}.1"
    leak_rotated_fds="$(log_fd_count "$LEAK_CID" '/var/log/airgap/downstream.log.*')"
else
    leak_rotated_fds="unavailable"
fi

if [[ "$leak_rotated_fds" != "0" && "$leak_rotated_fds" != "unavailable" ]]; then
    report_check "TC-35 negative control" "leaking build retains a rotated descriptor" \
        "$leak_rotated_fds retained" 0 || rc=1
else
    report_check "TC-35 negative control" "leaking build retains a rotated descriptor" \
        "$leak_rotated_fds retained" 1 || rc=1
fi

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-35 containers retained. Artifacts: $WORK"
    report_summary "TC-35 log rotation closes replaced file descriptors" "$rc"
    trap - EXIT
    exit "$rc"
fi
exit "$rc"
