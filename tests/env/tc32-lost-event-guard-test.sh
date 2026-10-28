#!/usr/bin/env bash
# TC-32 verifies the Airgap Lost Event (ALE) guard: records above maxEventBytes
# are replaced by a guard carrying the same message id, the original payload is
# stored on the upstream machine, and the offset sequence stays continuous so
# the gap detector never asks for a resend.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc32.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC32_WORK="$WORK"
COMPOSE_FILE="$HERE/docker-compose.tc32.yml"
OVERSIZED_RECORDS=3
SMALL_RECORDS=3

compose() {
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color >&2 || true
        echo "TC-32 artifacts retained at $WORK" >&2
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if (( rc == 0 )); then
        rm -rf "$WORK"
    fi
    report_summary "TC-32 Airgap Lost Event guard" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

echo "TC-32: building Linux helper and lost-event upstream stack"
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

echo "TC-32: producing 3 small and 3 oversized records"
compose run --rm producer | tee "$WORK/producer.log"

deadline=$((SECONDS + 90))
lag=""
while (( SECONDS < deadline )); do
    lag="$(docker exec "$(compose ps -q kafka)" kafka-consumer-groups \
        --bootstrap-server kafka:9092 --group tc32-No-delay --describe 2>/dev/null |
        awk 'NR > 1 && $6 ~ /^[0-9]+$/ { total += $6; found=1 } END { if (found) print total }')"
    [[ "$lag" == "0" ]] && break
    sleep 1
done
sleep 2

compose logs --no-color --no-log-prefix upstream >"$WORK/upstream.log" 2>&1
guard_count="$(grep -c "AIRGAP LOST EVENT" "$WORK/sink.capture" 2>/dev/null || true)"
stored_count="$(docker exec "$UPSTREAM_CID" sh -c 'ls -1 /airgap/work/lost-events 2>/dev/null | wc -l' | tr -d ' ')"
stored_bytes="$(docker exec "$UPSTREAM_CID" sh -c 'cat /airgap/work/lost-events/* 2>/dev/null | wc -c' | tr -d ' ')"
restart_count="$(docker inspect --format '{{.RestartCount}}' "$UPSTREAM_CID")"
state="$(docker inspect --format '{{.State.Status}}' "$UPSTREAM_CID")"

rc=0
if [[ "$guard_count" == "$OVERSIZED_RECORDS" ]]; then
    report_check "lost-event guards delivered" "$OVERSIZED_RECORDS guards at the sink" "$guard_count" 0 || rc=1
else
    report_check "lost-event guards delivered" "$OVERSIZED_RECORDS guards at the sink" "${guard_count:-0}" 1 || rc=1
fi
# The guard is an OTLP-shaped JSON LogRecord. The payload must carry the event
# marker and semantic fields, while all values remain valid JSON strings.
if grep -q '"eventName":"message.lost"' "$WORK/sink.capture" &&
    grep -Eq '"stringValue":"AIRGAP LOST EVENT: Event transfer 0:[0-9]+ couldn.t be delivered due to message size 4194304 bytes exceeds maxEventBytes 1048576 bytes"' "$WORK/sink.capture" &&
    grep -q '"key":"service.instance.id","value":{"stringValue":"Upstream_32"}' "$WORK/sink.capture" &&
    grep -q '"key":"airgap.event.lost.file","value":{"stringValue":"/airgap/work/lost-events/' "$WORK/sink.capture" &&
    grep -q '"key":"airgap.message.max_size","value":{"intValue":"6291456"}' "$WORK/sink.capture" &&
    grep -Fq '"key":"airgap.test.escape","value":{"stringValue":"value \"quoted\"\\backslash"}' "$WORK/sink.capture"; then
    report_check "JSON guard template rendered" "OTLP fields substituted and quote/backslash escaped" "matched" 0 || rc=1
else
    report_check "JSON guard template rendered" "OTLP fields substituted and quote/backslash escaped" "no match" 1 || rc=1
fi
# Offsets 1, 3 and 5 are the oversized records; every one must appear exactly
# once so the delivered sequence has no hole for the gap detector to chase.
missing_offsets=""
for offset in 1 3 5; do
    grep -q "AIRGAP LOST EVENT: Event transfer 0:${offset} " "$WORK/sink.capture" || missing_offsets="$missing_offsets $offset"
done
if [[ -z "$missing_offsets" ]]; then
    report_check "offset sequence preserved" "guards for offsets 1, 3 and 5" "all present" 0 || rc=1
else
    report_check "offset sequence preserved" "guards for offsets 1, 3 and 5" "missing:$missing_offsets" 1 || rc=1
fi
if [[ "$stored_count" == "$OVERSIZED_RECORDS" ]]; then
    report_check "original payloads stored" "$OVERSIZED_RECORDS files in lostEventDirectory" "$stored_count" 0 || rc=1
else
    report_check "original payloads stored" "$OVERSIZED_RECORDS files in lostEventDirectory" "${stored_count:-0}" 1 || rc=1
fi
if [[ "$stored_bytes" == "$((OVERSIZED_RECORDS * 4 * 1024 * 1024))" ]]; then
    report_check "stored payloads complete" "$((OVERSIZED_RECORDS * 4 * 1024 * 1024)) bytes" "$stored_bytes bytes" 0 || rc=1
else
    report_check "stored payloads complete" "$((OVERSIZED_RECORDS * 4 * 1024 * 1024)) bytes" "${stored_bytes:-0} bytes" 1 || rc=1
fi
# lostEvent is configured as ERROR:... so the log record must carry the
# configured level and the configured text, not the delivered guard.
lost_logged="$(grep -c "\[ERROR\] Event transfer 0:[0-9]* couldn't be delivered" "$WORK/upstream.log" || true)"
if (( lost_logged >= OVERSIZED_RECORDS )); then
    report_check "lost events logged at the configured level" "at least $OVERSIZED_RECORDS ERROR records" "$lost_logged" 0 || rc=1
else
    report_check "lost events logged at the configured level" "at least $OVERSIZED_RECORDS ERROR records" "$lost_logged" 1 || rc=1
fi
# Anchored on the rendered record, because the startup configuration echo
# prints the templates themselves, placeholders and all.
if grep "\[ERROR\] Event transfer" "$WORK/upstream.log" | grep -q "{"; then
    report_check "lost-event templates expanded" "no unresolved placeholders" "unresolved placeholder found" 1 || rc=1
else
    report_check "lost-event templates expanded" "no unresolved placeholders" "all substituted" 0 || rc=1
fi
small_delivered="$(grep -c "AIRGAP_00000" "$WORK/sink.capture" 2>/dev/null || true)"
if (( small_delivered >= SMALL_RECORDS )); then
    report_check "small records unaffected" "at least $SMALL_RECORDS original payloads delivered" "$small_delivered" 0 || rc=1
else
    report_check "small records unaffected" "at least $SMALL_RECORDS original payloads delivered" "$small_delivered" 1 || rc=1
fi
if [[ "$lag" == "0" ]]; then
    report_check "partition not stalled" "consumer lag 0" "lag 0" 0 || rc=1
else
    report_check "partition not stalled" "consumer lag 0" "lag ${lag:-unknown}" 1 || rc=1
fi
if [[ "$restart_count" == "0" && "$state" == "running" ]]; then
    report_check "upstream process survival" "running, zero restarts" "$state, restarts=$restart_count" 0 || rc=1
else
    report_check "upstream process survival" "running, zero restarts" "$state, restarts=$restart_count" 1 || rc=1
fi

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-32 containers retained. Artifacts: $WORK"
    report_summary "TC-32 Airgap Lost Event guard" "$rc"
    trap - EXIT
    exit "$rc"
fi
exit "$rc"
