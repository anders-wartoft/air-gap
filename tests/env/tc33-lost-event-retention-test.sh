#!/usr/bin/env bash
# TC-33 verifies positive and negative lost-event retention behavior and proves
# that every Go removal produces a JSON TYPE_STATUS record in the downstream
# internal Kafka topic.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc33.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC33_WORK="$WORK"
COMPOSE_FILE="$HERE/docker-compose.tc33.yml"

compose() {
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color >&2 || true
        echo "TC-33 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if (( rc == 0 )); then
        rm -rf "$WORK"
    fi
    report_summary "TC-33 lost-event retention and status" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "TC-33: building helper and starting upstream, downstream and Kafka"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/airgap-load" ./lib/loadgen
chmod 0755 "$WORK/airgap-load"
compose build
compose up -d kafka topic-init seed downstream upstream

UPSTREAM_CID="$(compose ps -q upstream)"
DOWNSTREAM_CID="$(compose ps -q downstream)"
KAFKA_CID="$(compose ps -q kafka)"
[[ -n "$UPSTREAM_CID" && -n "$DOWNSTREAM_CID" && -n "$KAFKA_CID" ]] || {
    echo "TC-33 containers did not start" >&2
    exit 1
}

deadline=$((SECONDS + 90))
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

echo "TC-33: producing one oversized record to trigger retention"
compose run --rm producer | tee "$WORK/producer.log"

deadline=$((SECONDS + 90))
lag=""
while (( SECONDS < deadline )); do
    lag="$(docker exec "$KAFKA_CID" kafka-consumer-groups \
        --bootstrap-server kafka:9092 --group tc33-No-delay --describe 2>/dev/null |
        awk 'NR > 1 && $6 ~ /^[0-9]+$/ { total += $6; found=1 } END { if (found) print total }')"
    [[ "$lag" == "0" ]] && break
    sleep 1
done
sleep 3

docker exec "$KAFKA_CID" timeout 15s kafka-console-consumer \
    --bootstrap-server kafka:9092 --topic airgap-internal --from-beginning \
    --timeout-ms 10000 >"$WORK/internal.log" 2>"$WORK/internal.err" || true
docker exec "$KAFKA_CID" timeout 10s kafka-console-consumer \
    --bootstrap-server kafka:9092 --topic transfer --from-beginning \
    --timeout-ms 5000 >"$WORK/transfer.log" 2>"$WORK/transfer.err" || true

retention_result="failed"
if compose run --rm seed verify-retention --dir=/airgap/work/lost-events >"$WORK/retention.log" 2>&1; then
    retention_result="passed"
fi
status_result="failed"
if compose run --rm seed verify-status --file=/airgap/work/internal.log --expected=3 >"$WORK/status.log" 2>&1; then
    status_result="passed"
fi

compose logs --no-color --no-log-prefix upstream >"$WORK/upstream.log" 2>&1
upstream_restarts="$(docker inspect --format '{{.RestartCount}}' "$UPSTREAM_CID")"
downstream_restarts="$(docker inspect --format '{{.RestartCount}}' "$DOWNSTREAM_CID")"
upstream_state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID")"
downstream_state="$(docker inspect --format '{{.State.Status}}' "$DOWNSTREAM_CID")"

rc=0
if [[ "$retention_result" == "passed" ]]; then
    report_check "retention policy" "70-day files deleted, 10-day file compressed with age preserved, 1-day file retained" "$(<"$WORK/retention.log")" 0 || rc=1
else
    report_check "retention policy" "70-day files deleted, 10-day file compressed with age preserved, 1-day file retained" "$(<"$WORK/retention.log")" 1 || rc=1
fi
if [[ "$status_result" == "passed" ]]; then
    report_check "truncation status routing" "3 valid JSON status records in airgap-internal" "$(<"$WORK/status.log")" 0 || rc=1
else
    report_check "truncation status routing" "3 valid JSON status records in airgap-internal" "$(<"$WORK/status.log")" 1 || rc=1
fi
if grep -q "AIRGAP LOST EVENT" "$WORK/transfer.log"; then
    report_check "guard delivery" "lost-event guard in transfer topic" "observed" 0 || rc=1
else
    report_check "guard delivery" "lost-event guard in transfer topic" "missing" 1 || rc=1
fi
if ! grep -q "AIRGAP LOST MESSAGE TRUNCATED" "$WORK/transfer.log"; then
    report_check "status isolation" "no truncation status in transfer topic" "isolated to internal topic" 0 || rc=1
else
    report_check "status isolation" "no truncation status in transfer topic" "status leaked into transfer topic" 1 || rc=1
fi
if [[ "$lag" == "0" ]]; then
    report_check "partition progress" "consumer lag 0" "lag 0" 0 || rc=1
else
    report_check "partition progress" "consumer lag 0" "lag ${lag:-unknown}" 1 || rc=1
fi
if [[ "$upstream_state" == "running" && "$downstream_state" == "running" &&
      "$upstream_restarts" == "0" && "$downstream_restarts" == "0" ]]; then
    report_check "process survival" "both running with zero restarts" "upstream=$upstream_state/$upstream_restarts downstream=$downstream_state/$downstream_restarts" 0 || rc=1
else
    report_check "process survival" "both running with zero restarts" "upstream=$upstream_state/$upstream_restarts downstream=$downstream_state/$downstream_restarts" 1 || rc=1
fi

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-33 containers retained. Artifacts: $WORK"
    report_summary "TC-33 lost-event retention and status" "$rc"
    trap - EXIT
    exit "$rc"
fi
exit "$rc"
