#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc34.XXXXXX")"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC34_WORK="$WORK"
COMPOSE_FILE="$HERE/docker-compose.tc34.yml"

compose() {
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color >&2 || true
        echo "TC-34 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if (( rc == 0 )); then
        rm -rf "$WORK"
    fi
    report_summary "TC-34 slow UDP Kafka subscription stability" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "TC-34: building Linux binaries and starting UDP transfer stack"
mkdir -p ../../target/linux-amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../../target/linux-amd64/upstream ../../src/cmd/upstream
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../../target/linux-amd64/downstream ../../src/cmd/downstream
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/airgap-load" ./lib/loadgen
chmod 0755 "$WORK/airgap-load"
compose build
compose up -d kafka topic-init downstream upstream

UPSTREAM_CID="$(compose ps -q upstream)"
DOWNSTREAM_CID="$(compose ps -q downstream)"
KAFKA_CID="$(compose ps -q kafka)"
[[ -n "$UPSTREAM_CID" && -n "$DOWNSTREAM_CID" && -n "$KAFKA_CID" ]] || {
    echo "TC-34 containers did not start" >&2
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

echo "TC-34: producing eight records for a one-event-per-second UDP consumer"
compose run --rm producer | tee "$WORK/producer.log"

deadline=$((SECONDS + 45))
lag=""
while (( SECONDS < deadline )); do
    lag="$(docker exec "$KAFKA_CID" kafka-consumer-groups \
        --bootstrap-server kafka:9092 --group tc34-Slow-udp --describe 2>/dev/null |
        awk 'NR > 1 && $6 ~ /^[0-9]+$/ { total += $6; found=1 } END { if (found) print total }')"
    [[ "$lag" == "0" ]] && break
    sleep 1
done
sleep 3

compose logs --no-color --no-log-prefix upstream >"$WORK/upstream.log" 2>&1
upstream_restarts="$(docker inspect --format '{{.RestartCount}}' "$UPSTREAM_CID")"
downstream_restarts="$(docker inspect --format '{{.RestartCount}}' "$DOWNSTREAM_CID")"
upstream_state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID")"
downstream_state="$(docker inspect --format '{{.State.Status}}' "$DOWNSTREAM_CID")"
transfer_count="$(docker exec "$KAFKA_CID" kafka-get-offsets \
    --bootstrap-server kafka:9092 --topic transfer 2>/dev/null |
    awk -F: '{ total += $3 } END { print total + 0 }')"

rc=0
if grep -q "kafkaMaxProcessingTimeMs: 2000" "$WORK/upstream.log"; then
    report_check "processing timeout applied" "kafkaMaxProcessingTimeMs 2000" "configured" 0 || rc=1
else
    report_check "processing timeout applied" "kafkaMaxProcessingTimeMs 2000" "missing" 1 || rc=1
fi
if grep -q "abandoned subscription.*consuming was taking too long" "$WORK/upstream.log"; then
    report_check "Kafka subscription stability" "no abandoned subscription" "abandonment observed" 1 || rc=1
else
    report_check "Kafka subscription stability" "no abandoned subscription" "stable" 0 || rc=1
fi
if [[ "$lag" == "0" ]]; then
    report_check "slow consumer progress" "consumer lag 0" "lag 0" 0 || rc=1
else
    report_check "slow consumer progress" "consumer lag 0" "lag ${lag:-unknown}" 1 || rc=1
fi
if [[ "$transfer_count" == "8" ]]; then
    report_check "UDP delivery" "8 downstream records" "$transfer_count records" 0 || rc=1
else
    report_check "UDP delivery" "8 downstream records" "$transfer_count records" 1 || rc=1
fi
if [[ "$upstream_restarts" == "0" && "$downstream_restarts" == "0" &&
      "$upstream_state" == "running" && "$downstream_state" == "running" ]]; then
    report_check "process survival" "both running with zero restarts" \
        "upstream=$upstream_state/$upstream_restarts downstream=$downstream_state/$downstream_restarts" 0 || rc=1
else
    report_check "process survival" "both running with zero restarts" \
        "upstream=$upstream_state/$upstream_restarts downstream=$downstream_state/$downstream_restarts" 1 || rc=1
fi

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-34 containers retained. Artifacts: $WORK"
    report_summary "TC-34 slow UDP Kafka subscription stability" "$rc"
    trap - EXIT
    exit "$rc"
fi
exit "$rc"
