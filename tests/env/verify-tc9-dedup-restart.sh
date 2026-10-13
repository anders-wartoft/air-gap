#!/usr/bin/env bash
# TC-9, part 2 — dedup restart safety (Docker).
#
# The TC-9 chain run proves that duplicates are removed and gaps are detected.
# This check covers the operational half of the same testcase: what dedup does
# when Kafka goes away for a while, and what it does when dedup itself is
# restarted.
#
# Regression under test: dedup used to terminate on the first failed Kafka
# connectivity probe, and again whenever startup did not reach RUNNING inside
# FAIL_FAST_STARTUP_TIMEOUT_MS. On an upgrade that restarts Kafka, both fired,
# and a restart that restored state from the changelog could exceed the startup
# limit and exit once more. The service then alternated between activating and
# running, which is what the product smoketest observed.
#
# The stack runs dedup with FAIL_FAST=true and a deliberately short startup
# timeout, with nothing supervising the process. If dedup terminates, its
# container stops and the checks below fail.
#
# Run standalone (after `make build-java`):
#     bash tests/env/verify-tc9-dedup-restart.sh

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/report.sh"

PROJECT="airgap-tc9-dedup-restart-$$"
COMPOSE_FILE="$HERE/docker-compose.tc9-dedup-restart.yml"

# Kafka stays down slightly longer than the issue's reproduction (15 s) and
# well past FAIL_FAST_CHECK_TIMEOUT_MS, so at least one probe must fail.
OUTAGE_SECONDS="${TC9_RESTART_OUTAGE_SECONDS:-20}"
RUNNING_TIMEOUT="${TC9_RESTART_RUNNING_TIMEOUT:-240}"
RESTORE_TIMEOUT="${TC9_RESTART_RESTORE_TIMEOUT:-120}"
DEGRADED_TIMEOUT="${TC9_RESTART_DEGRADED_TIMEOUT:-120}"
# How long dedup must keep running after a restart without terminating.
# Comfortably longer than FAIL_FAST_STARTUP_TIMEOUT_MS (5 s by default).
SETTLE_SECONDS="${TC9_RESTART_SETTLE_SECONDS:-20}"

RUNNING_MARKER='Kafka Streams reached RUNNING'
DEGRADED_MARKER='Kafka connectivity degraded|Kafka connectivity check failed'
RESTORED_MARKER='Kafka connectivity restored'
FATAL_MARKER='FAIL_FAST startup timeout|did not reach RUNNING state|terminating process'

RC=0
fail_check() { RC=1; }

compose() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }

dedup_cid() { compose ps -q dedup 2>/dev/null; }

dedup_logs() { compose logs --no-color dedup 2>/dev/null; }

count_marker() { dedup_logs | grep -cE "$1" || true; }

dedup_alive() {
    local cid
    cid="$(dedup_cid)"
    [[ -n "$cid" ]] || return 1
    [[ "$(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null)" == "true" ]]
}

dedup_started_at() {
    local cid
    cid="$(dedup_cid)"
    [[ -n "$cid" ]] || return 1
    docker inspect -f '{{.State.StartedAt}}' "$cid" 2>/dev/null
}

dedup_exit_state() {
    local cid
    cid="$(dedup_cid)"
    if [[ -z "$cid" ]]; then
        echo "container gone"
        return
    fi
    echo "running=$(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null)" \
         "exitCode=$(docker inspect -f '{{.State.ExitCode}}' "$cid" 2>/dev/null)"
}

# Wait until <pattern> has been logged at least <want> times. Give up early if
# dedup died, because that is itself the regression being tested.
wait_for_marker() {
    local pattern="$1" want="$2" timeout="$3" waited=0
    while (( waited < timeout )); do
        if (( $(count_marker "$pattern") >= want )); then
            return 0
        fi
        if ! dedup_alive; then
            return 2
        fi
        sleep 2
        waited=$(( waited + 2 ))
    done
    return 1
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 || RC != 0 )); then
        echo "─── TC-9 dedup restart: dedup log tail ───" >&2
        dedup_logs | tail -n 60 >&2
    fi
    if ! compose down -v --remove-orphans --timeout 10 >/dev/null 2>&1; then
        echo "TC-9 dedup restart teardown failed for project $PROJECT" >&2
        rc=1
    fi
    if (( rc == 0 )); then rc="$RC"; fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if ! compgen -G "$HERE/../../java-streams/target/air-gap-deduplication-fat-*.jar" >/dev/null; then
    report_check "TC-9 dedup restart prerequisites" \
        "dedup fat jar under java-streams/target" "missing — run: make build-java" 1 || fail_check
    exit 1
fi

compose up -d >/dev/null 2>&1

# ---------------------------------------------------------------------------
# 1. Baseline: dedup starts and reaches RUNNING.
# ---------------------------------------------------------------------------
wait_for_marker "$RUNNING_MARKER" 1 "$RUNNING_TIMEOUT"
case $? in
    0) report_check "TC-9 dedup initial startup" \
           "reaches RUNNING within ${RUNNING_TIMEOUT}s" "reached RUNNING" 0 || fail_check ;;
    2) report_check "TC-9 dedup initial startup" \
           "reaches RUNNING within ${RUNNING_TIMEOUT}s" "process terminated ($(dedup_exit_state))" 1 || fail_check ;;
    *) report_check "TC-9 dedup initial startup" \
           "reaches RUNNING within ${RUNNING_TIMEOUT}s" "timed out" 1 || fail_check ;;
esac

# Static group membership: the instance identity must come from INSTANCE_ID so
# a restarting instance reclaims its partitions instead of waiting out the old
# dynamic member's session timeout.
expected_instance="${TC9_RESTART_INSTANCE_ID:-dedup-1}"
if (( $(count_marker "Group Instance ID: ${expected_instance}\$") >= 1 )); then
    report_check "TC-9 dedup static membership" \
        "group.instance.id=${expected_instance} from INSTANCE_ID" "configured" 0 || fail_check
else
    report_check "TC-9 dedup static membership" \
        "group.instance.id=${expected_instance} from INSTANCE_ID" \
        "not configured (dedup logged: $(dedup_logs | grep -E 'Group Instance ID' | tail -n 1))" 1 || fail_check
fi

started_before_outage="$(dedup_started_at)"

# ---------------------------------------------------------------------------
# 2. Kafka outage: dedup must stay up and report degraded connectivity.
# ---------------------------------------------------------------------------
compose stop kafka >/dev/null 2>&1
outage_start="$(date +%s)"

wait_for_marker "$DEGRADED_MARKER" 1 "$DEGRADED_TIMEOUT"
case $? in
    0) report_check "TC-9 dedup outage reporting" \
           "logs degraded Kafka connectivity within ${DEGRADED_TIMEOUT}s" "reported" 0 || fail_check ;;
    2) report_check "TC-9 dedup outage reporting" \
           "logs degraded Kafka connectivity within ${DEGRADED_TIMEOUT}s" \
           "process terminated ($(dedup_exit_state))" 1 || fail_check ;;
    *) report_check "TC-9 dedup outage reporting" \
           "logs degraded Kafka connectivity within ${DEGRADED_TIMEOUT}s" "timed out" 1 || fail_check ;;
esac

# Hold the outage for the full configured duration before Kafka returns.
outage_elapsed=$(( $(date +%s) - outage_start ))
if (( outage_elapsed < OUTAGE_SECONDS )); then
    sleep "$(( OUTAGE_SECONDS - outage_elapsed ))"
fi

if dedup_alive; then
    report_check "TC-9 dedup during ${OUTAGE_SECONDS}s Kafka outage" \
        "process keeps running" "still running" 0 || fail_check
else
    report_check "TC-9 dedup during ${OUTAGE_SECONDS}s Kafka outage" \
        "process keeps running" "terminated ($(dedup_exit_state))" 1 || fail_check
fi

# ---------------------------------------------------------------------------
# 3. Kafka returns: dedup must recover in-process, without having restarted.
# ---------------------------------------------------------------------------
compose start kafka >/dev/null 2>&1

wait_for_marker "$RESTORED_MARKER" 1 "$RESTORE_TIMEOUT"
case $? in
    0) report_check "TC-9 dedup recovery after outage" \
           "logs restored Kafka connectivity within ${RESTORE_TIMEOUT}s" "recovered" 0 || fail_check ;;
    2) report_check "TC-9 dedup recovery after outage" \
           "logs restored Kafka connectivity within ${RESTORE_TIMEOUT}s" \
           "process terminated ($(dedup_exit_state))" 1 || fail_check ;;
    *) report_check "TC-9 dedup recovery after outage" \
           "logs restored Kafka connectivity within ${RESTORE_TIMEOUT}s" "timed out" 1 || fail_check ;;
esac

started_after_outage="$(dedup_started_at)"
if [[ -n "$started_before_outage" && "$started_before_outage" == "$started_after_outage" ]]; then
    report_check "TC-9 dedup process continuity" \
        "same process across the outage" "never restarted" 0 || fail_check
else
    report_check "TC-9 dedup process continuity" \
        "same process across the outage" \
        "restarted (${started_before_outage} -> ${started_after_outage})" 1 || fail_check
fi

# ---------------------------------------------------------------------------
# 4. Restart dedup, as the upgrade's service handler does. Startup has to
#    survive past FAIL_FAST_STARTUP_TIMEOUT_MS and reach RUNNING again.
# ---------------------------------------------------------------------------
running_before_restart="$(count_marker "$RUNNING_MARKER")"
compose restart dedup >/dev/null 2>&1

wait_for_marker "$RUNNING_MARKER" "$(( running_before_restart + 1 ))" "$RUNNING_TIMEOUT"
case $? in
    0) report_check "TC-9 dedup restart" \
           "reaches RUNNING again within ${RUNNING_TIMEOUT}s" "reached RUNNING" 0 || fail_check ;;
    2) report_check "TC-9 dedup restart" \
           "reaches RUNNING again within ${RUNNING_TIMEOUT}s" \
           "process terminated ($(dedup_exit_state))" 1 || fail_check ;;
    *) report_check "TC-9 dedup restart" \
           "reaches RUNNING again within ${RUNNING_TIMEOUT}s" "timed out" 1 || fail_check ;;
esac

# No restart loop: stay up well past the startup timeout that used to kill it.
sleep "$SETTLE_SECONDS"
if dedup_alive; then
    report_check "TC-9 dedup restart loop" \
        "still running ${SETTLE_SECONDS}s after restart" "stable" 0 || fail_check
else
    report_check "TC-9 dedup restart loop" \
        "still running ${SETTLE_SECONDS}s after restart" "terminated ($(dedup_exit_state))" 1 || fail_check
fi

# ---------------------------------------------------------------------------
# 5. None of the old fail-fast terminations may appear anywhere in the run.
# ---------------------------------------------------------------------------
fatal_hits="$(count_marker "$FATAL_MARKER")"
if (( fatal_hits == 0 )); then
    report_check "TC-9 dedup fail-fast terminations" \
        "none logged" "none found" 0 || fail_check
else
    report_check "TC-9 dedup fail-fast terminations" \
        "none logged" "${fatal_hits} found: $(dedup_logs | grep -E "$FATAL_MARKER" | tail -n 1)" 1 || fail_check
fi

report_summary "TC-9 dedup restart safety" "$RC" || true
exit "$RC"
