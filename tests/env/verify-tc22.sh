#!/usr/bin/env bash
# TC-22 verifier — prove downstream can be stopped and restarted mid-run
# over TCP without upstream needing a restart and without losing/
# duplicating any events (REQ-37).
#
# No Kafka/LogGenerator anywhere in this testcase's data path (source=
# random upstream, target=cmd downstream — see testcases/22.env), so
# there's nothing for the generic sent/received/duplicate verdict to
# compare; this IS the only verdict for TC-22.
#
# Checks:
#   1. downstream's log (one continuous history across the docker
#      stop/start — see DOWNSTREAM_RESTART_AT_SECONDS in testcases/22.env)
#      shows "Random message N" forming a STRICTLY sequential run from 0
#      with zero gaps and zero duplicates, proving nothing was lost or
#      re-delivered across the restart.
#   2. upstream's log shows "TCP connection unavailable" during the
#      downtime window — proof downstream really was unreachable, not
#      that the restart was too fast to matter.
#   3. downstream's log shows a fresh "New TCP connection from" after the
#      restart — proof it's a genuine reconnection, not a fluke.
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
UPSTREAM_CTR="${PROJECT}-upstream-a-1"
DOWNSTREAM_CTR="${PROJECT}-downstream-1"

echo "─── verify-tc22: did downstream restart cleanly with no loss? ───"

if ! docker inspect "$UPSTREAM_CTR" >/dev/null 2>&1; then
    report_check "Container ${UPSTREAM_CTR}" "running" "NOT running" 1 || true
    exit 2
fi
if ! docker inspect "$DOWNSTREAM_CTR" >/dev/null 2>&1; then
    report_check "Container ${DOWNSTREAM_CTR}" "running" "NOT running" 1 || true
    exit 2
fi

upstream_log="$(docker logs "$UPSTREAM_CTR" 2>&1 || true)"
downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"

rc=0

# ── 1. downstream's "Random message N" sequence: no gaps, no duplicates ──
numbers="$(printf '%s\n' "$downstream_log" | grep -oE 'Random message [0-9]+' | grep -oE '[0-9]+' || true)"
total_count="$(printf '%s\n' "$numbers" | grep -c . || true)"
total_count="${total_count:-0}"
unique_count="$(printf '%s\n' "$numbers" | sort -n -u | grep -c . || true)"
unique_count="${unique_count:-0}"

if (( total_count > 0 )); then
    report_check "downstream 'Random message N' lines received" "> 0" "${total_count}" 0 || rc=1
else
    report_check "downstream 'Random message N' lines received" "> 0" "0 — nothing arrived at all" 1 || rc=1
fi

if (( total_count == unique_count )); then
    report_check "Duplicate 'Random message N' deliveries" "0" "0" 0 || rc=1
else
    dup_count=$(( total_count - unique_count ))
    report_check "Duplicate 'Random message N' deliveries" "0" "${dup_count}" 1 || rc=1
fi

# Gaps: sorted unique numbers should be exactly 0..max with no jumps.
if (( unique_count > 0 )); then
    max_n="$(printf '%s\n' "$numbers" | sort -n -u | tail -n 1)"
    expected_count=$(( max_n + 1 ))
    if (( unique_count == expected_count )); then
        report_check "downstream sequence gaps (0..${max_n})" "none" "none — all $((max_n + 1)) values present" 0 || rc=1
    else
        missing_count=$(( expected_count - unique_count ))
        report_check "downstream sequence gaps (0..${max_n})" "none" "${missing_count} missing out of ${expected_count}" 1 || rc=1
    fi
fi

# ── 2. upstream genuinely saw downstream go unreachable ──
unavailable_count="$(printf '%s\n' "$upstream_log" | grep -cF 'TCP connection unavailable' || true)"
unavailable_count="${unavailable_count:-0}"
if (( unavailable_count > 0 )); then
    report_check "upstream-a 'TCP connection unavailable' during downtime" "> 0" "${unavailable_count} found" 0 || rc=1
else
    report_check "upstream-a 'TCP connection unavailable' during downtime" "> 0" "0 found — restart window may have been too short" 1 || rc=1
fi

# ── 3. downstream shows a genuine reconnection after the restart ──
reconnect_count="$(printf '%s\n' "$downstream_log" | grep -cF 'New TCP connection from' || true)"
reconnect_count="${reconnect_count:-0}"
if (( reconnect_count >= 2 )); then
    report_check "downstream 'New TCP connection from' count" ">= 2 (initial + post-restart)" "${reconnect_count}" 0 || rc=1
else
    report_check "downstream 'New TCP connection from' count" ">= 2 (initial + post-restart)" "${reconnect_count}" 1 || rc=1
fi

report_summary "verify-tc22" "$rc"
exit "$rc"
