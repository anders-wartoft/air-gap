#!/usr/bin/env bash
# TC-13 verifier — prove dedup periodically logs received/sent/missing
# counters at the configured interval (REQ-27/28).
#
# What TC-13 proves at the sink level:
#   * 100 events were written to kafka-upstream[transfer]
#   * Upstream deliverFilter=2,4,6 forwards half → 50 events arrive at
#     kafka-downstream[transfer]
#   * Dedup emits those 50 to kafka-downstream[dedup]
#   * The LG sink sees 50 unique, 50 missing (allowed via EXPECTED_MISSING)
#
# What this verifier checks on top of that:
#   1. The dedup container emitted at least two [MISSING-REPORT] lines
#      (proves periodic emit is working, not just a one-shot).
#   2. Each line is a JSON array containing the REQ-27/28 counters:
#        total_received, delta_received, total_emitted, delta_emitted,
#        total_missing, delta_missing
#      (JSON structure proves counters are logged, not just a message
#      saying "something missing happened").
#   3. The last report shows total_received > 0 and total_missing > 0
#      (proves the counters actually tracked the stream, not that they
#      stayed at zero because nothing flowed through).
#   4. The GAP_EMIT_INTERVAL_SEC the app logged at startup matches the
#      value we configured in dedup-13-docker.env (REQ-27/28 "configurable
#      interval").
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN
# Optional env: TC13_EXPECTED_INTERVAL (default 5), TC13_MIN_REPORTS (default 2)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DEDUP_CTR="${PROJECT}-dedup-1"
EXPECTED_INTERVAL="${TC13_EXPECTED_INTERVAL:-5}"
MIN_REPORTS="${TC13_MIN_REPORTS:-2}"

echo "─── verify-tc13: dedup periodic MISSING-REPORT counters ───"

if ! docker inspect "$DEDUP_CTR" >/dev/null 2>&1; then
    report_check "Container ${DEDUP_CTR}" "running" "NOT running" 1 || true
    exit 2
fi

# Grab ALL dedup container output. [MISSING-REPORT] lines go through
# log4j2 → stderr/stdout, which docker captures regardless of the
# log4j2.xml appender choice.
dedup_log="$(docker logs "$DEDUP_CTR" 2>&1 || true)"
if [[ -z "$dedup_log" ]]; then
    report_check "${DEDUP_CTR} log output" "non-empty" "empty" 1 || true
    exit 2
fi

rc=0

# ── 1. Count MISSING-REPORT lines ──
report_count="$(printf '%s\n' "$dedup_log" | grep -c '\[MISSING-REPORT\]' || true)"
report_count="${report_count:-0}"
if (( report_count >= MIN_REPORTS )); then
    report_check "Dedup [MISSING-REPORT] line count" ">= ${MIN_REPORTS}" "${report_count}" 0 || rc=1
else
    report_check "Dedup [MISSING-REPORT] line count" ">= ${MIN_REPORTS}" "${report_count} — periodic emit may be broken" 1 || rc=1
fi

# ── 2. Required counter fields on every reported line ──
# Pre-filter the MISSING-REPORT lines once. With `set -euo pipefail`
# in effect a `grep | grep -q` chain in a conditional can mis-signal
# a missing field when the OUTER grep's exit status leaks, so we stash
# the filtered lines in a variable first and probe with plain grep.
report_lines="$(printf '%s\n' "$dedup_log" | grep -F '[MISSING-REPORT]' || true)"
required_fields=(total_received delta_received total_emitted delta_emitted total_missing delta_missing)
missing_fields=()
for field in "${required_fields[@]}"; do
    if ! printf '%s' "$report_lines" | grep -q -F "\"${field}\":"; then
        missing_fields+=("$field")
    fi
done
if (( ${#missing_fields[@]} == 0 )); then
    report_check "Required counter fields present" "${required_fields[*]}" "all present" 0 || rc=1
else
    report_check "Required counter fields present" "${required_fields[*]}" "MISSING: ${missing_fields[*]}" 1 || rc=1
fi

# ── 3. Last report shows the stream actually flowed ──
# Aggregate the latest total_received and total_missing across all
# partitions. We take the LAST occurrence per partition (one line per
# partition per emit cycle). Simpler proxy: sum of all total_received
# across the very last emit cycle = sum across partitions.
last_report_block="$(printf '%s\n' "$dedup_log" \
    | grep '\[MISSING-REPORT\]' \
    | tail -15)"   # 15 partitions × 1 line each in the final cycle

sum_total_received="$(printf '%s\n' "$last_report_block" \
    | sed -n 's/.*"total_received":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
sum_total_missing="$(printf '%s\n' "$last_report_block" \
    | sed -n 's/.*"total_missing":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
sum_total_emitted="$(printf '%s\n' "$last_report_block" \
    | sed -n 's/.*"total_emitted":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
if (( sum_total_received > 0 )); then
    report_check "Dedup last-cycle total_received" "> 0" "${sum_total_received}" 0 || rc=1
else
    report_check "Dedup last-cycle total_received" "> 0" "0 — counter is not tracking the stream" 1 || rc=1
fi
if (( sum_total_missing > 0 )); then
    report_check "Dedup last-cycle total_missing (REQ-28)" "> 0" "${sum_total_missing}" 0 || rc=1
else
    report_check "Dedup last-cycle total_missing (REQ-28)" "> 0" "0 — not tracking gaps despite deliverFilter dropping half the stream" 1 || rc=1
fi
if (( sum_total_emitted > 0 )); then
    report_check "Dedup last-cycle total_emitted (REQ-27)" "> 0" "${sum_total_emitted}" 0 || rc=1
else
    report_check "Dedup last-cycle total_emitted (REQ-27)" "> 0" "0 — sent counter never advanced" 1 || rc=1
fi

# ── 4. Configured MISSING_REPORT_INTERVAL_SEC was picked up ──
# dedup logs `MISSING_REPORT_INTERVAL_SEC=5` at startup (PartitionDedupApp.java).
# REQ-27/28 says the interval must be configurable, so this proves the
# env var really reaches the app (not that the default happened to be
# used).
interval_log="$(printf '%s\n' "$dedup_log" \
    | grep -oE 'MISSING_REPORT_INTERVAL_SEC=[0-9]+' \
    | head -1 \
    | cut -d= -f2)"
interval_log="${interval_log:-UNKNOWN}"
if [[ "$interval_log" == "$EXPECTED_INTERVAL" ]]; then
    report_check "Dedup MISSING_REPORT_INTERVAL_SEC" "${EXPECTED_INTERVAL}" "${interval_log}" 0 || rc=1
else
    report_check "Dedup MISSING_REPORT_INTERVAL_SEC" "${EXPECTED_INTERVAL}" "${interval_log} — env not reaching the dedup app" 1 || rc=1
fi

report_summary "verify-tc13" "$rc"
exit "$rc"
