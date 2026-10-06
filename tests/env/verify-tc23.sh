#!/usr/bin/env bash
# TC-23 verifier — prove (a) input-filtered events don't create gaps
# (REQ-38, same create/resend gap-closing proof as TC-14) and (b) resend
# applies the SAME input filter, so previously-filtered content never
# reappears in full even via the gap-fill path (REQ-39 — TC-23's actual
# new claim over TC-14).
#
# Checks:
#   1-2. Same as verify-tc14.sh: at least one negative delta_missing (a
#        gap genuinely closed) and the last MISSING-REPORT cycle sums to
#        a near-zero total_missing across all partitions.
#   3. Direct kafka-console-consumer scrape of kafka-downstream[dedup]
#      (the clean topic) — NO message's VALUE may contain the substring
#      "10", whether it arrived there via upstream's own inputFilterRules
#      (first delivery) or via resend's gap-fill (second delivery). This
#      is the check that actually distinguishes TC-23 from TC-14: a dumb
#      "gaps closed" count alone can't tell you whether resend resurrected
#      filtered content in full.
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN
# Optional: TC23_PARTITION_COUNT (default 15).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DEDUP_CTR="${PROJECT}-dedup-1"
KAFKA_DOWNSTREAM_CTR="${PROJECT}-kafka-downstream-1"
PARTITION_COUNT="${TC23_PARTITION_COUNT:-15}"

echo "─── verify-tc23: did filtering avoid gaps, and did resend respect the same filter? ───"

if ! docker inspect "$DEDUP_CTR" >/dev/null 2>&1; then
    report_check "Container ${DEDUP_CTR}" "running" "NOT running" 1 || true
    exit 2
fi

dedup_log="$(docker logs "$DEDUP_CTR" 2>&1 || true)"
if [[ -z "$dedup_log" ]]; then
    report_check "${DEDUP_CTR} log output" "non-empty" "empty" 1 || true
    exit 2
fi

rc=0

# ── 1. At least one negative delta_missing (a gap actually closed) ──
report_lines="$(printf '%s\n' "$dedup_log" | grep -F '[MISSING-REPORT]' || true)"
negative_delta_count="$(printf '%s\n' "$report_lines" \
    | grep -oE '"delta_missing":-?[0-9]+' \
    | grep -oE -- '-[0-9]+' \
    | grep -c . || true)"
negative_delta_count="${negative_delta_count:-0}"
if (( negative_delta_count > 0 )); then
    report_check "Reports with negative delta_missing (gap closing)" "> 0" "${negative_delta_count}" 0 || rc=1
else
    report_check "Reports with negative delta_missing (gap closing)" "> 0" "0 — resend's gap-fill may not have reached dedup" 1 || rc=1
fi

# ── 2. Last emit cycle: gaps closed (small allowance for the same
#    windowed tail-boundary blind spot documented for TC-14) ──
last_cycle="$(printf '%s\n' "$report_lines" | tail -n "$PARTITION_COUNT")"
sum_total_missing="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_missing":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
sum_total_received="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_received":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
if (( sum_total_missing <= 5 )); then
    report_check "Dedup last-cycle total_missing" "<= 5" "${sum_total_missing}" 0 || rc=1
else
    report_check "Dedup last-cycle total_missing" "<= 5" "${sum_total_missing} — gaps not closed after resend" 1 || rc=1
fi
if (( sum_total_received > 0 )); then
    report_check "Dedup last-cycle total_received" "> 0" "${sum_total_received}" 0 || rc=1
else
    report_check "Dedup last-cycle total_received" "> 0" "0 — counter never tracked the stream" 1 || rc=1
fi

# ── 3. The clean topic itself: no "10"-containing VALUE, from either
#    upstream's own filtering or resend's gap-fill ──
if docker inspect "$KAFKA_DOWNSTREAM_CTR" >/dev/null 2>&1; then
    clean_dump="$(docker exec "$KAFKA_DOWNSTREAM_CTR" kafka-console-consumer \
        --bootstrap-server kafka-downstream:9092 --topic dedup --from-beginning \
        --timeout-ms 8000 --property print.key=true 2>/dev/null || true)"
    total_lines="$(printf '%s\n' "$clean_dump" | grep -c . || true)"
    total_lines="${total_lines:-0}"
    if (( total_lines > 0 )); then
        report_check "Clean topic (dedup) messages scraped" "> 0" "${total_lines}" 0 || rc=1
    else
        report_check "Clean topic (dedup) messages scraped" "> 0" "0 — consumer found nothing, check is inconclusive" 1 || rc=1
    fi
    # Each line is "KEY\tVALUE" (print.key=true, default separator is tab).
    # A leaked filter would show up as a non-empty VALUE containing "10".
    leaked_count="$(printf '%s\n' "$clean_dump" \
        | awk -F'\t' '{ v = $2; for (i = 3; i <= NF; i++) v = v "\t" $i; if (v ~ /10/) print }' \
        | grep -c . || true)"
    leaked_count="${leaked_count:-0}"
    if (( leaked_count == 0 )); then
        report_check "Clean topic VALUEs containing '10' (filter leak)" "0" "0" 0 || rc=1
    else
        report_check "Clean topic VALUEs containing '10' (filter leak)" "0" "${leaked_count} — filter not applied on resend (or on original delivery)" 1 || rc=1
        printf '%s\n' "$clean_dump" | awk -F'\t' '{ v=$2; for (i=3;i<=NF;i++) v=v"\t"$i; if (v ~ /10/) print }' | head -5 >&2
    fi
else
    report_check "Container ${KAFKA_DOWNSTREAM_CTR}" "running" "NOT running" 1 || rc=1
fi

report_summary "verify-tc23" "$rc"
exit "$rc"
