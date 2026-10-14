#!/usr/bin/env bash
# TC-28 verifier: exact Kafka recovery, dedup gap closure, and REQ-52
# OTLP/JSON validation for all five in-scope applications.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
WORK="../../tmp/tc28"
PARTITION_COUNT="${TC28_PARTITION_COUNT:-15}"
rc=0

count_clean_topic() {
    docker compose exec -T kafka-downstream sh -c \
        'kafka-console-consumer --bootstrap-server kafka-downstream:9092 --topic dedup --from-beginning --timeout-ms 5000 2>/dev/null || true' \
        | grep -c . || true
}

clean_count="$(count_clean_topic)"
if [[ "$clean_count" == 100 ]]; then
    report_check "Clean Kafka topic after resend" "exactly 100 events" "100 events" 0 || rc=1
else
    report_check "Clean Kafka topic after resend" "exactly 100 events" "${clean_count} events" 1 || rc=1
fi

dedup_log="$WORK/dedup.log"
mkdir -p "$WORK"
docker logs "${PROJECT}-upstream-a-1" >"$WORK/upstream.log" 2>&1 || rc=1
docker logs "${PROJECT}-downstream-1" >"$WORK/downstream.log" 2>&1 || rc=1
docker logs "${PROJECT}-dedup-1" >"$dedup_log" 2>&1 || rc=1

dedup_bodies="$(python3 "$HERE/lib/verify_otlp.py" --bodies dedup "$dedup_log" || true)"
report_lines="$(printf '%s\n' "$dedup_bodies" | grep -F '[MISSING-REPORT]' || true)"
negative_delta_count="$(printf '%s\n' "$report_lines" \
    | grep -oE '"delta_missing":-?[0-9]+' \
    | grep -oE -- '-[0-9]+' \
    | grep -c . || true)"
if (( negative_delta_count > 0 )); then
    report_check "Dedup gap-closing reports" "> 0 negative delta_missing" "$negative_delta_count" 0 || rc=1
else
    report_check "Dedup gap-closing reports" "> 0 negative delta_missing" "0" 1 || rc=1
fi

last_cycle="$(printf '%s\n' "$report_lines" | tail -n "$PARTITION_COUNT")"
sum_total_missing="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_missing":\([0-9]*\).*/\1/p' \
    | awk '{ value += $1 } END { print value+0 }')"
if (( sum_total_missing == 0 )); then
    report_check "Dedup final confirmed gaps" "0" "0" 0 || rc=1
else
    report_check "Dedup final confirmed gaps" "0" "$sum_total_missing" 1 || rc=1
fi

for service in upstream downstream dedup create resend; do
    if python3 "$HERE/lib/verify_otlp.py" "$service" "$WORK/$service.log"; then
        report_check "$service REQ-52 OTLP/JSON" "valid envelopes and field types" "verified" 0 || rc=1
    else
        report_check "$service REQ-52 OTLP/JSON" "valid envelopes and field types" "invalid" 1 || rc=1
    fi
done

# REQ-52.09: a consumer must be able to tell what each application did from the
# attributes alone. Every record already has to carry a hierarchical eventName
# for the check above to pass, so what is left to prove is that the names are
# actually meaningful: each application must describe the domains it works in.
declare -a semantic_domains=(
    "upstream:app.lifecycle. kafka. transport."
    "downstream:app.lifecycle. kafka. udp."
    "dedup:app.lifecycle. kafka."
    "create:create."
    "resend:kafka."
)
for entry in "${semantic_domains[@]}"; do
    service="${entry%%:*}"
    prefixes="${entry#*:}"

    args=()
    for prefix in $prefixes; do
        args+=(--require-prefix "$prefix")
    done

    if python3 "$HERE/lib/verify_otlp.py" "${args[@]}" "$service" "$WORK/$service.log"; then
        report_check "$service REQ-52.09 semantic attributes" "eventName for $prefixes" "verified" 0 || rc=1
    else
        report_check "$service REQ-52.09 semantic attributes" "eventName for $prefixes" "incomplete" 1 || rc=1
    fi
done

if (( rc == 0 )); then
    rm -rf "$WORK"
fi

report_summary "TC-28 OTLP logs and resend recovery" "$rc"
exit "$rc"
