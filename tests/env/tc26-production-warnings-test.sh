#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
source "$HERE/lib/report.sh"
source "$HERE/lib/production-warnings.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
umask 077
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc26.XXXXXX")"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC26_LOG_LEVEL=DEBUG

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc26.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        if ! compose logs --no-color >&2; then
            echo "TC-26 could not collect service logs" >&2
        fi
    fi
    if ! compose down --timeout 10; then
        echo "TC-26 teardown failed; logs retained at $WORK" >&2
        report_summary "TC-26 Docker production warnings" 1 || true
        exit 1
    fi
    rm -r "$WORK"
    report_summary "TC-26 Docker production warnings" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

capture() {
    compose logs --no-color --no-log-prefix "$1" > "$WORK/$1.log" 2>&1
}

expected() {
    if [[ "$TC26_LOG_LEVEL" == DEBUG ]]; then
        printf 'logLevel=DEBUG,'
    fi
    printf 'logStatistics=0,'
    if [[ "$1" == upstream-a ]]; then
        printf 'source=random,tcpTLSEnabled=false'
    else
        printf 'target=cmd,tcpTLSCertFile='
    fi
}

check_warnings() {
    local service="$1" phases="$2"
    capture "$service"
    if validate_production_warnings "$WORK/$service.log" "$(expected "$service")" "$phases"; then
        report_check "$service $TC26_LOG_LEVEL $phases warnings" \
            "exact effective set, WARN severity, nonempty risks, final shutdown block" "verified" 0
    else
        report_check "$service $TC26_LOG_LEVEL $phases warnings" \
            "exact effective set and final shutdown block" "mismatch" 1
    fi
}

wait_delivery() {
    local deadline=$((SECONDS + 30)) count=0
    while (( SECONDS < deadline )); do
        capture downstream
        count="$(awk '
            match($0, /Random message [0-9]+/) {
                number = substr($0, RSTART, RLENGTH)
                seen[number] = 1
            }
            END { for (number in seen) count++; print count + 0 }
        ' "$WORK/downstream.log")"
        if (( count >= 5 )); then
            report_check "$TC26_LOG_LEVEL red-thread delivery" \
                "at least 5 distinct received messages" "$count received" 0
            return
        fi
        sleep 1
    done
    report_check "$TC26_LOG_LEVEL red-thread delivery" \
        "at least 5 distinct received messages within 30s" "$count received" 1
}

stop_and_check() {
    local service="$1" signal="$2" cid deadline state
    cid="$(compose ps -q "$service")"
    [[ -n "$cid" ]] || { echo "$service has no running container" >&2; return 1; }
    docker kill --signal="$signal" "$cid" >/dev/null
    deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        state="$(docker inspect --format '{{.State.Status}}' "$cid")"
        [[ "$state" == exited ]] && break
        sleep 1
    done
    state="$(docker inspect --format '{{.State.Status}}/{{.State.ExitCode}}' "$cid")"
    if [[ "$state" == exited/0 ]]; then
        report_check "$service orderly $signal shutdown" "exited/0" "$state" 0
    else
        report_check "$service orderly $signal shutdown" "exited/0 within 30s" "$state" 1
    fi
    check_warnings "$service" both
}

compose build
for level in DEBUG ERROR; do
    export TC26_LOG_LEVEL="$level"
    echo "TC-26: $level startup -> real TCP delivery -> orderly shutdown"
    compose up -d --force-recreate --no-deps downstream upstream-a
    wait_delivery
    check_warnings upstream-a startup
    check_warnings downstream startup
    signal=TERM
    [[ "$level" == ERROR ]] && signal=INT
    stop_and_check upstream-a "$signal"
    stop_and_check downstream "$signal"
done

if [[ "${TC_PAUSE:-0}" == 1 ]]; then
    echo "TC-26 checks require stopping both services; -pause retains their final stopped containers."
    report_summary "TC-26 Docker production warnings" 0
    printf 'Inspect/tear down: docker compose -p %q -f %q logs\n' "$PROJECT" "$HERE/docker-compose.tc26.yml"
    printf 'docker compose -p %q -f %q down\n' "$PROJECT" "$HERE/docker-compose.tc26.yml"
    rm -r "$WORK"
    trap - EXIT
fi
