#!/usr/bin/env bash
# TC-21 verifier — prove two separate Kafka clusters sharing a topic name
# ("transfer") can feed one dedup instance with no data loss, after the
# phase-2 full replay closes the gaps phase 1's deliverFilter opened.
#
# Checks:
#   1. Both clusters' content actually reached dedup's CLEAN_TOPIC — grep
#      the sink's captured output for BOTH "Cluster1_" and "Cluster2_"
#      prefixed keys. If only one cluster's data appears, the two-cluster
#      claim isn't actually being exercised, bulk counters notwithstanding.
#   2. Dedup's own [MISSING-REPORT] shows gaps were genuinely open at some
#      point (an early cycle has total_missing > 0) — proof phase 1's
#      deliverFilter really did drop events, not that this test trivially
#      passes because nothing was ever missing.
#   3. The LAST emit cycle sums to total_missing == 0 (or close to it —
#      same inherent tail-boundary blind spot documented for TC-14) across
#      all partitions — proof the phase-2 replay actually closed the gaps
#      in BOTH partition ranges (0-4 and 10-14).
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN
# Optional: TC21_PARTITION_COUNT (default 15, matches RAW_TOPICS=transfer
# partition count in this Docker rig), TC21_MAX_FINAL_MISSING (default 5 —
# same windowed-tail-boundary allowance as TC-14's EXPECTED_MISSING range).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DEDUP_CTR="${PROJECT}-dedup-1"
SINK_CTR="${PROJECT}-lg-sink-1"
PARTITION_COUNT="${TC21_PARTITION_COUNT:-15}"
MAX_FINAL_MISSING="${TC21_MAX_FINAL_MISSING:-5}"

echo "─── verify-tc21: did both clusters reach dedup, and did replay close the gaps? ───"

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

# ── 1. Both clusters' content actually reached dedup's clean topic ──
sink_log=""
if docker inspect "$SINK_CTR" >/dev/null 2>&1; then
    sink_log="$(docker logs "$SINK_CTR" 2>&1 || true)"
fi
cluster1_count="$(printf '%s\n' "$sink_log" | grep -cF 'Cluster1_' || true)"
cluster1_count="${cluster1_count:-0}"
cluster2_count="$(printf '%s\n' "$sink_log" | grep -cF 'Cluster2_' || true)"
cluster2_count="${cluster2_count:-0}"
if (( cluster1_count > 0 )); then
    report_check "Cluster 1 (kafka-upstream) events at sink" "> 0" "${cluster1_count} found" 0 || rc=1
else
    report_check "Cluster 1 (kafka-upstream) events at sink" "> 0" "0 found" 1 || rc=1
fi
if (( cluster2_count > 0 )); then
    report_check "Cluster 2 (kafka-upstream-b) events at sink" "> 0" "${cluster2_count} found" 0 || rc=1
else
    report_check "Cluster 2 (kafka-upstream-b) events at sink" "> 0" "0 found" 1 || rc=1
fi

# ── 2. Gaps were genuinely open at some point (deliverFilter actually bit) ──
report_lines="$(printf '%s\n' "$dedup_log" | grep -F '[MISSING-REPORT]' || true)"
report_count="$(printf '%s\n' "$report_lines" | grep -c . || true)"
report_count="${report_count:-0}"
if (( report_count >= PARTITION_COUNT * 2 )); then
    report_check "Dedup [MISSING-REPORT] line count" ">= $((PARTITION_COUNT * 2))" "${report_count}" 0 || rc=1
else
    report_check "Dedup [MISSING-REPORT] line count" ">= $((PARTITION_COUNT * 2))" "${report_count} — too few cycles to observe open-then-close" 1 || rc=1
fi

# Checking only the FIRST cycle is unreliable: dedup emits on a fixed
# GAP_EMIT_INTERVAL_SEC timer regardless of whether any traffic has
# arrived yet, so with LG_STARTUP_DELAY holding the producers back the
# earliest cycles are legitimately all-zero (nothing has been sent yet),
# not evidence the filter failed. Instead take the MAX total_missing
# summed per complete cycle across the WHOLE run — if gaps were ever open
# at any point, this is > 0 regardless of exactly which cycle caught it.
max_cycle_missing="$(printf '%s\n' "$report_lines" \
    | sed -n 's/.*"total_missing":\([0-9]*\).*/\1/p' \
    | awk -v n="$PARTITION_COUNT" '{
        sum += $1; count++
        if (count == n) { if (sum > max) max = sum; sum = 0; count = 0 }
      } END { print max+0 }')"
if (( max_cycle_missing > 0 )); then
    report_check "Dedup max per-cycle total_missing (gaps really opened)" "> 0" "${max_cycle_missing}" 0 || rc=1
else
    report_check "Dedup max per-cycle total_missing (gaps really opened)" "> 0" "0 — deliverFilter may not have taken effect" 1 || rc=1
fi


# ── 3. Last emit cycle: gaps closed (or within the documented tail-boundary allowance) ──
last_cycle="$(printf '%s\n' "$report_lines" | tail -n "$PARTITION_COUNT")"
sum_total_missing="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_missing":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
sum_total_received="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_received":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
if (( sum_total_missing <= MAX_FINAL_MISSING )); then
    report_check "Dedup last-cycle total_missing" "<= ${MAX_FINAL_MISSING}" "${sum_total_missing}" 0 || rc=1
else
    report_check "Dedup last-cycle total_missing" "<= ${MAX_FINAL_MISSING}" "${sum_total_missing} — replay didn't close the gaps" 1 || rc=1
fi
if (( sum_total_received > 0 )); then
    report_check "Dedup last-cycle total_received" "> 0" "${sum_total_received}" 0 || rc=1
else
    report_check "Dedup last-cycle total_received" "> 0" "0 — counter never tracked the stream" 1 || rc=1
fi

report_summary "verify-tc21" "$rc"
exit "$rc"
