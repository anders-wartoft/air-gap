#!/usr/bin/env bash
# TC-14 verifier — prove the create+resend gap-fill actually closed the
# gaps dedup had been reporting (REQ-3b, REQ-3d, REQ-40).
#
# tc14-resend.sh (the POST_DRAIN_SCRIPT) already asserts the bundle file
# was created with gap entries and that `create`/`resend` exited 0. This
# script checks the OUTCOME: did dedup's own counters confirm the gaps
# closed, as the manual procedure observes via "MISSING-REPORT now shows
# negative delta_missing as the gaps close"?
#
# Checks:
#   1. At least one [MISSING-REPORT] line shows a negative delta_missing
#      (direct evidence a gap closed during this run, not just that gaps
#      existed and were never resolved).
#   2. The LAST emit cycle (last N lines, N = partition count) sums to
#      total_missing == 0 across all partitions — the final state has no
#      open gaps left.
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN
# Optional: TC14_PARTITION_COUNT (default 15, matches the dedup app's
# RAW_TOPICS=transfer partition count in this Docker rig).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DEDUP_CTR="${PROJECT}-dedup-1"
PARTITION_COUNT="${TC14_PARTITION_COUNT:-15}"

echo "─── verify-tc14: did create+resend actually close dedup's gaps? ───"

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

report_lines="$(printf '%s\n' "$dedup_log" | grep -F '[MISSING-REPORT]' || true)"
report_count="$(printf '%s\n' "$report_lines" | grep -c . || true)"
report_count="${report_count:-0}"
if (( report_count >= 2 )); then
    report_check "Dedup [MISSING-REPORT] line count" ">= 2" "${report_count}" 0 || rc=1
else
    report_check "Dedup [MISSING-REPORT] line count" ">= 2" "${report_count} — too few to observe a gap-close transition" 1 || rc=1
fi

# ── 1. At least one negative delta_missing (a gap actually closed) ──
# Portable extraction: BSD/macOS sed doesn't support the GNU `\?`
# quantifier (zero-or-one) in basic regex, so a `-\?[0-9]*` sed pattern
# silently fails to match on macOS. `grep -oE` with POSIX ERE `-?[0-9]+`
# works identically on both BSD and GNU grep.
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

# ── 2. Last emit cycle sums to total_missing == 0 across all partitions ──
last_cycle="$(printf '%s\n' "$report_lines" | tail -n "$PARTITION_COUNT")"
sum_total_missing="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_missing":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
sum_total_received="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_received":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
if (( sum_total_missing == 0 )); then
    report_check "Dedup last-cycle total_missing" "0" "0" 0 || rc=1
else
    report_check "Dedup last-cycle total_missing" "0" "${sum_total_missing} — gaps not fully closed after resend" 1 || rc=1
fi
if (( sum_total_received > 0 )); then
    report_check "Dedup last-cycle total_received" "> 0" "${sum_total_received}" 0 || rc=1
else
    report_check "Dedup last-cycle total_received" "> 0" "0 — counter never tracked the stream" 1 || rc=1
fi

report_summary "verify-tc14" "$rc"
exit "$rc"
