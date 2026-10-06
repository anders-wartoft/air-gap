#!/usr/bin/env bash
# Shared "what we tested and why it passed/failed" reporter for the
# Docker chain test scripts (verify-tc*.sh, tc*-test.sh).
#
# Produces ONE line per concrete assertion, in the form:
#   <subject>, expected <expected>, <actual>
# colored green on pass / red on fail, so a reviewer (including
# non-engineers, e.g. for a management-facing report) can scan the
# transcript and see exactly what was checked and why it passed or
# failed, without reading the shell logic that produced it.
#
# Usage:
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/report.sh"
#   report_check "Input 123-45-6789" "blocked" "not found" 0            # -> green PASS line
#   report_check "Input ANDERS@SITIA.NU" "blocked" "FOUND in Kafka" 1   # -> red FAIL line
#
# The 4th argument is a pass/fail code using normal shell exit-code
# convention: 0 = pass, non-zero = fail. report_check itself returns
# that same code, so it composes naturally with `if`/`&&`/`||`:
#
#   if grep -qF "$marker" "$capture_file"; then
#       report_check "Phase A wire capture" "plaintext visible" "found" 0
#   else
#       report_check "Phase A wire capture" "plaintext visible" "NOT found" 1
#   fi
#
# or, chaining a function's own exit code directly:
#
#   some_check; report_check "some check" "ok" "$(describe_actual)" $?
#
# Colors are emitted unconditionally — these scripts run through
# `docker compose` / `tee` pipes already (see run-testcase.sh's
# TTY-avoidance re-exec), so ANSI escapes survive into both the
# terminal and the captured log file; the whole point here is for a
# human to visually scan the transcript. Set NO_COLOR=1 in the
# environment to suppress colors (https://no-color.org convention),
# e.g. for a CI system that chokes on escape codes.

if [[ -z "${NO_COLOR-}" ]]; then
    REPORT_GREEN=$'\033[0;32m'
    REPORT_RED=$'\033[0;31m'
    REPORT_RESET=$'\033[0m'
else
    REPORT_GREEN=''
    REPORT_RED=''
    REPORT_RESET=''
fi

# report_check <subject> <expected> <actual-description> [pass-code]
# pass-code defaults to 0 (pass) if omitted.
report_check() {
    local subject="$1" expected="$2" actual="$3" code="${4:-0}"
    if [[ "$code" -eq 0 ]]; then
        printf '%s%s, expected %s, %s%s\n' "$REPORT_GREEN" "$subject" "$expected" "$actual" "$REPORT_RESET"
    else
        printf '%s%s, expected %s, %s%s\n' "$REPORT_RED" "$subject" "$expected" "$actual" "$REPORT_RESET"
    fi
    return "$code"
}

# report_summary <label> <overall-rc>
# Prints a final green/red banner line summarizing a whole script's
# result — use once at the end, after all report_check calls.
report_summary() {
    local label="$1" rc="${2:-0}"
    if [[ "$rc" -eq 0 ]]; then
        printf '%s─── %s: PASS ───%s\n' "$REPORT_GREEN" "$label" "$REPORT_RESET"
    else
        printf '%s─── %s: FAIL ───%s\n' "$REPORT_RED" "$label" "$REPORT_RESET" >&2
    fi
    return "$rc"
}
