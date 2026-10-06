#!/usr/bin/env bash
# TC-20 verifier — prove the "downstream starts late" recovery path was
# actually exercised, not just that the bulk 1000/1000 delivery count
# (already checked by run-testcase.sh's generic verdict) happened to pass
# because the stack came up fast enough that the race never triggered.
#
# Checks:
#   1. upstream-a's log shows at least one "TCP connection unavailable"
#      retry line — proof that downstream really was unreachable for a
#      while (DOWNSTREAM_STARTUP_DELAY actually held it back).
#   2. upstream-a's log shows "Transport status restored to running" —
#      proof the retry loop recovered on its own once downstream came up,
#      rather than upstream giving up / crashing / restarting.
#   3. The "unavailable" marker appears BEFORE the "restored" marker (the
#      correct chronological order — otherwise this isn't proving what it
#      claims to).
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
UPSTREAM_CTR="${PROJECT}-upstream-a-1"

echo "─── verify-tc20: did upstream really hit TCP-unavailable and recover? ───"

if ! docker inspect "$UPSTREAM_CTR" >/dev/null 2>&1; then
    report_check "Container ${UPSTREAM_CTR}" "running" "NOT running" 1 || true
    exit 2
fi

upstream_log="$(docker logs "$UPSTREAM_CTR" 2>&1 || true)"
if [[ -z "$upstream_log" ]]; then
    report_check "${UPSTREAM_CTR} log output" "non-empty" "empty" 1 || true
    exit 2
fi

rc=0

unavailable_count="$(printf '%s\n' "$upstream_log" | grep -cF 'TCP connection unavailable' || true)"
unavailable_count="${unavailable_count:-0}"
if (( unavailable_count > 0 )); then
    report_check "upstream-a 'TCP connection unavailable' retries" "> 0" "${unavailable_count} found" 0 || rc=1
else
    report_check "upstream-a 'TCP connection unavailable' retries" "> 0" "0 found — downstream may not have been held back" 1 || rc=1
fi

restored_count="$(printf '%s\n' "$upstream_log" | grep -cF 'Transport status restored to running' || true)"
restored_count="${restored_count:-0}"
if (( restored_count > 0 )); then
    report_check "upstream-a 'Transport status restored to running'" "> 0" "${restored_count} found" 0 || rc=1
else
    report_check "upstream-a 'Transport status restored to running'" "> 0" "0 found — never recovered" 1 || rc=1
fi

if (( unavailable_count > 0 && restored_count > 0 )); then
    first_unavailable_line="$(printf '%s\n' "$upstream_log" | grep -nF 'TCP connection unavailable' | head -1 | cut -d: -f1)"
    first_restored_line="$(printf '%s\n' "$upstream_log" | grep -nF 'Transport status restored to running' | head -1 | cut -d: -f1)"
    if (( first_unavailable_line < first_restored_line )); then
        report_check "Chronological order (unavailable before restored)" "unavailable first" "unavailable @line ${first_unavailable_line}, restored @line ${first_restored_line}" 0 || rc=1
    else
        report_check "Chronological order (unavailable before restored)" "unavailable first" "restored @line ${first_restored_line} came before unavailable @line ${first_unavailable_line}" 1 || rc=1
    fi
fi

report_summary "verify-tc20" "$rc"
exit "$rc"
