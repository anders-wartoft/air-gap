#!/usr/bin/env bash
# TC-8 — SIGHUP log-rotation verifier.
#
# Automates the manual TC-8 procedure from tests/documents/TESTCASES.md.
# For each air-gap process (upstream-a and downstream):
#   1. Confirm the configured log file exists in the container.
#   2. `docker exec` to `mv` it out of the way (e.g. upstream.log → upstream-1.log).
#   3. `docker kill --signal=HUP` the container (compose's PID 1 is the
#      air-gap binary itself; the shutdown-hook / SIGHUP handler reopens
#      the configured file).
#   4. Wait for a fresh log file to reappear at the original path.
#   5. Confirm the new file contains "Logrotate completed" and the old
#      file ends with "SIGHUP received: reopening logs".
#
# Env vars provided by run-testcase.sh:
#   TC_ID, PROJECT_NAME, CONTAINER_NAME (= tc8-settle-timer), SINK_CID,
#   VERIFY_RC_IN

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"

verify_rotation() {
    local service="$1" logpath="$2" rotated="$3"
    local ctr="${PROJECT}-${service}-1"
    local label="${service}:${logpath}"
    echo "─── ${label} ───"

    if ! docker inspect "$ctr" >/dev/null 2>&1; then
        report_check "Container ${ctr}" "running" "NOT found" 1 || true
        return 2
    fi

    # 1. Initial log must already exist (the air-gap binary opens it in
    #    the "Configuring log to:" step during startup).
    if docker exec "$ctr" test -f "$logpath"; then
        report_check "${label} initial file exists" "exists" "exists" 0
    else
        report_check "${label} initial file exists" "exists" "does NOT exist" 1 || true
        return 1
    fi
    local initial_bytes
    initial_bytes="$(docker exec "$ctr" sh -c "wc -c <'$logpath'" 2>/dev/null | tr -d ' ' || echo 0)"
    echo "${label}: initial size ${initial_bytes} bytes"

    # 2. Rename the file. The air-gap process still has it open on an
    #    old inode; new writes continue to go to the (now orphaned)
    #    rotated file until SIGHUP arrives.
    docker exec "$ctr" mv "$logpath" "$rotated"

    # 3. SIGHUP. docker kill --signal=HUP sends to PID 1 which is the
    #    air-gap binary (entrypoint uses `exec` so there's no wrapping
    #    shell to swallow the signal).
    echo "${label}: sending SIGHUP to ${ctr}"
    docker kill --signal=HUP "$ctr" >/dev/null

    # 4. Poll until the handler has (a) created a new file at the
    #    original path AND (b) written the "SIGHUP handling completed"
    #    marker to it. See src/upstream/upstream.go SIGHUP goroutine for
    #    the exact log sequence: the handler first prints "SIGHUP
    #    received: reopening logs with name …" and "Reopening logs for
    #    logrotate. New name: …" to the *old* (renamed) file, then calls
    #    SetLogFile which opens a fresh fd at the original path, then
    #    prints "SIGHUP handling completed" to that new file.
    local waited=0
    local max_wait=10
    while (( waited < max_wait )); do
        if docker exec "$ctr" sh -c "test -f '$logpath' && grep -q 'SIGHUP handling completed' '$logpath'" 2>/dev/null; then
            break
        fi
        sleep 1
        waited=$((waited + 1))
    done
    if (( waited >= max_wait )); then
        report_check "${label} new file + marker after SIGHUP" "appears within ${max_wait}s" "NOT appeared within ${max_wait}s" 1 || true
        echo "${label}: last 10 lines of ${rotated}:" >&2
        docker exec "$ctr" tail -n 10 "$rotated" 2>/dev/null >&2 || true
        echo "${label}: contents of ${logpath}:" >&2
        docker exec "$ctr" cat "$logpath" 2>/dev/null >&2 || true
        return 1
    fi
    report_check "${label} new file + marker after SIGHUP" "appears within ${max_wait}s" "appeared after ${waited}s" 0

    # 5. Marker-string checks on the rotated (old) file too — this is the
    #    one that holds "SIGHUP received: reopening logs ..." and
    #    "Reopening logs for logrotate. New name: ...".
    if docker exec "$ctr" grep -q "SIGHUP received" "$rotated"; then
        report_check "${label} rotated file contains 'SIGHUP received'" "present" "present" 0
    else
        report_check "${label} rotated file contains 'SIGHUP received'" "present" "MISSING" 1 || true
        echo "${label}: last 10 lines of ${rotated}:" >&2
        docker exec "$ctr" tail -n 10 "$rotated" 2>/dev/null >&2 || true
        return 1
    fi

    return 0
}

PASS=0
FAIL=0

if verify_rotation upstream-a /tmp/upstream.log /tmp/upstream-1.log; then
    PASS=$((PASS + 1))
else
    FAIL=$((FAIL + 1))
fi
if verify_rotation downstream /tmp/downstream.log /tmp/downstream-1.log; then
    PASS=$((PASS + 1))
else
    FAIL=$((FAIL + 1))
fi

echo
if (( FAIL > 0 )); then
    report_summary "verify-tc8 (${PASS} ok, ${FAIL} failed)" 1
    exit 1
fi
report_summary "verify-tc8 (${PASS} ok, ${FAIL} failed)" 0
exit 0
