#!/usr/bin/env bash
# TC-28 post-drain hook. This deliberately uses separate downstream and
# upstream paths and an explicit host-side copy for the resend bundle.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

WORK_HOST="../../tmp/tc28"
DOWNSTREAM_HOST="$WORK_HOST/downstream"
UPSTREAM_HOST="$WORK_HOST/upstream"
DOWNSTREAM_CONTAINER="/airgap/tmp/tc28/downstream"
UPSTREAM_CONTAINER="/airgap/tmp/tc28/upstream"
BUNDLE_NAME="resend-28-all.json"
CREATE_LOG="$WORK_HOST/create.log"
RESEND_LOG="$WORK_HOST/resend.log"
PROPAGATION_WAIT="${TC28_PROPAGATION_WAIT:-20}"
RESEND_EPS="${TC28_RESEND_EPS:-50}"

rm -rf "$WORK_HOST"
mkdir -p "$DOWNSTREAM_HOST" "$UPSTREAM_HOST"

count_clean_topic() {
    docker compose exec -T kafka-downstream sh -c \
        'kafka-console-consumer --bootstrap-server kafka-downstream:9092 --topic dedup --from-beginning --timeout-ms 5000 2>/dev/null || true' \
        | grep -c . || true
}

before_count="$(count_clean_topic)"
if [[ "$before_count" =~ ^[0-9]+$ ]] && (( before_count > 0 && before_count < 100 )); then
    report_check "Clean Kafka topic before resend" "1..99 events" "${before_count} events (intentionally incomplete)" 0
else
    report_check "Clean Kafka topic before resend" "1..99 events" "${before_count} events" 1 || true
    exit 1
fi

echo "tc28-resend: exporting detected gaps on the downstream side"
if docker compose --profile create run --rm create create \
        --resendFileName="$DOWNSTREAM_CONTAINER/$BUNDLE_NAME" --limit=all \
        2>&1 | tee "$CREATE_LOG"; then
    report_check "create exports all detected gaps" "exit 0" "exit 0" 0
else
    report_check "create exports all detected gaps" "exit 0" "non-zero exit" 1 || true
    exit 1
fi

downstream_bundle="$DOWNSTREAM_HOST/$BUNDLE_NAME"
upstream_bundle="$UPSTREAM_HOST/$BUNDLE_NAME"
if [[ ! -s "$downstream_bundle" ]]; then
    report_check "Downstream resend bundle" "non-empty" "missing or empty" 1 || true
    exit 1
fi

gap_entries="$(grep -c '"partition"' "$downstream_bundle" || true)"
if (( gap_entries > 0 )); then
    report_check "Resend bundle partition entries" "> 0" "$gap_entries" 0
else
    report_check "Resend bundle partition entries" "> 0" "0" 1 || true
    exit 1
fi

echo "tc28-resend: copying resend bundle from downstream side to upstream side"
cp "$downstream_bundle" "$upstream_bundle"
if cmp -s "$downstream_bundle" "$upstream_bundle"; then
    report_check "Explicit downstream-to-upstream bundle copy" "byte-identical" "byte-identical" 0
else
    report_check "Explicit downstream-to-upstream bundle copy" "byte-identical" "differs" 1 || true
    exit 1
fi

echo "tc28-resend: replaying copied bundle on the upstream side"
if docker compose --profile resend run --rm resend resend \
        --resendFileName="$UPSTREAM_CONTAINER/$BUNDLE_NAME" --eps="$RESEND_EPS" \
        2>&1 | tee "$RESEND_LOG"; then
    report_check "resend replays copied bundle" "exit 0" "exit 0" 0
else
    report_check "resend replays copied bundle" "exit 0" "non-zero exit" 1 || true
    exit 1
fi

echo "tc28-resend: waiting ${PROPAGATION_WAIT}s for resend -> downstream -> dedup -> clean Kafka topic"
sleep "$PROPAGATION_WAIT"

