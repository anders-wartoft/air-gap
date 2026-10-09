#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
source "$HERE/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
umask 077

export TC27_WORK_DIR
TC27_WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc27.XXXXXX")"
PROJECT="$(basename "$TC27_WORK_DIR" | tr '[:upper:].' '[:lower:]-')"
export TC27_CLIENT_KEY=1 TC27_SERVER_KEY=1
PAUSE="${TC_PAUSE:-0}"

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc27.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        if ! compose logs --no-color >&2; then
            echo "TC-27 could not collect container logs" >&2
        fi
    fi
    if (( rc == 0 && PAUSE == 1 )); then
        report_summary "TC-27 Docker pinned TLS" 0
        printf 'Stack retained. Inspect or tear down using:\n'
        printf 'export TC27_WORK_DIR=%q\n' "$TC27_WORK_DIR"
        printf 'docker compose -p %q -f %q logs\n' "$PROJECT" "$HERE/docker-compose.tc27.yml"
        printf 'docker compose -p %q -f %q down\n' "$PROJECT" "$HERE/docker-compose.tc27.yml"
        printf 'Remove temporary private-key fixtures at %s after teardown.\n' "$TC27_WORK_DIR"
        return
    fi
    if ! compose down --timeout 10; then
        echo "TC-27 teardown failed; fixtures retained at $TC27_WORK_DIR" >&2
        report_summary "TC-27 Docker pinned TLS" 1 || true
        exit 1
    fi
    # Only this invocation's mktemp directory; never the shared test stack.
    rm -r "$TC27_WORK_DIR"
    report_summary "TC-27 Docker pinned TLS" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

mkdir -m 700 "$TC27_WORK_DIR/client" "$TC27_WORK_DIR/server" \
    "$TC27_WORK_DIR/clients" "$TC27_WORK_DIR/servers"
compose build
compose run --rm --no-deps --user "$(id -u):$(id -g)" -v "$TC27_WORK_DIR/client:/keys" \
    --entrypoint /airgap/bin/upstream upstream-a \
    --generate-tls-keysets=2 --tls-key-output-dir=/keys --tls-key-name=client --tls-key-role=client
compose run --rm --no-deps --user "$(id -u):$(id -g)" -v "$TC27_WORK_DIR/server:/keys" \
    --entrypoint /airgap/bin/downstream downstream \
    --generate-tls-keysets=2 --tls-key-output-dir=/keys --tls-key-name=server --tls-key-role=server

public_entry() {
    local base="$1" peer="$2" role="$3" path="$4" fingerprint
    fingerprint="$(cat "$base.fingerprint")"
    {
        printf '{"version":1,"peer":"%s","role":"%s","publicKey":"' "$peer" "$role"
        awk '{printf "%s\\n", $0}' "$base.pub"
        printf '","fingerprint":"%s"}\n' "$fingerprint"
    } > "$path"
    chmod 600 "$path"
}
public_entry "$TC27_WORK_DIR/client/client-1" tc27-client client "$TC27_WORK_DIR/clients/client.json"
public_entry "$TC27_WORK_DIR/server/server-1" tc27-server server "$TC27_WORK_DIR/servers/server.json"
CLIENT="$(cat "$TC27_WORK_DIR/client/client-1.fingerprint")"
SERVER="$(cat "$TC27_WORK_DIR/server/server-1.fingerprint")"
UNKNOWN_CLIENT="$(cat "$TC27_WORK_DIR/client/client-2.fingerprint")"
UNKNOWN_SERVER="$(cat "$TC27_WORK_DIR/server/server-2.fingerprint")"

capture() {
    compose logs --no-color --no-log-prefix "$1" > "$TC27_WORK_DIR/$1.log" 2>&1
}

assert_contains() {
    local service="$1" text="$2" subject="$3"
    capture "$service"
    if grep -qF "$text" "$TC27_WORK_DIR/$service.log"; then
        report_check "$subject" "$text" "found" 0
    else
        report_check "$subject" "$text" "not found" 1
    fi
}

wait_for() {
    local service="$1" text="$2" subject="$3" deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        capture "$service"
        if grep -qF "$text" "$TC27_WORK_DIR/$service.log"; then
            report_check "$subject" "$text" "found" 0
            return
        fi
        sleep 1
    done
    report_check "$subject" "$text within 30s" "timed out" 1
}

wait_delivery() {
    local subject="$1" deadline=$((SECONDS + 30)) count
    while (( SECONDS < deadline )); do
        capture downstream
        count="$(awk '
            match($0, /Random message [0-9]+/) {
                number = substr($0, RSTART, RLENGTH)
                sub(/Random message /, "", number)
                seen[number] = 1
            }
            END { for (number in seen) count++; print count + 0 }
        ' "$TC27_WORK_DIR/downstream.log")"
        if (( count >= 5 )); then
            report_check "$subject" "at least 5 distinct received application messages" "$count received" 0
            return
        fi
        sleep 1
    done
    report_check "$subject" "at least 5 received application messages within 30s" "$count received" 1
}

restart_pair() {
    compose stop --timeout 10 upstream-a downstream
    compose up -d --force-recreate --no-deps downstream
    wait_for downstream "TLS TCP listener started" "Downstream listener starts"
    compose up -d --force-recreate --no-deps upstream-a
}

assert_no_delivery() {
    local subject="$1"
    sleep 3
    capture downstream
    if grep -qE 'Random message [0-9]' "$TC27_WORK_DIR/downstream.log"; then
        report_check "$subject" "no application delivery" "unauthorized messages received" 1
    else
        report_check "$subject" "no application delivery" "none received" 0
    fi
    local service
    for service in downstream upstream-a; do
        if compose exec -T "$service" sh -c 'kill -0 1'; then
            report_check "$service survives rejected handshake" "running" "running" 0
        else
            report_check "$service survives rejected handshake" "running" "not running" 1
        fi
    done
}

echo "TC-27: trusted client/server, self-signed wrappers, no CA configured"
restart_pair
wait_delivery "Trusted pinned TLS data path"
assert_contains upstream-a "Pinned TLS authenticated server peer=tc27-server fingerprint=$SERVER" "Client pins exact server key"
assert_contains downstream "Pinned TLS authenticated client peer=tc27-client fingerprint=$CLIENT" "Server pins exact client key"
assert_contains downstream "TLS 1.3 /" "TLS 1.3 negotiated"

echo "TC-27: valid but untrusted client key must not deliver"
export TC27_CLIENT_KEY=2
restart_pair
wait_for downstream "untrusted client key $UNKNOWN_CLIENT" "Unknown client rejected by SPKI pin"
assert_no_delivery "Unknown client cannot send application data"

echo "TC-27: valid but untrusted server key must not deliver"
export TC27_CLIENT_KEY=1 TC27_SERVER_KEY=2
restart_pair
wait_for upstream-a "untrusted server key $UNKNOWN_SERVER" "Unknown server rejected by SPKI pin"
assert_no_delivery "Unknown server cannot receive application data"

echo "TC-27: restore original keys and prove end-to-end recovery"
export TC27_SERVER_KEY=1
restart_pair
wait_delivery "Trusted keys restored: application delivery recovers"
assert_contains upstream-a "Pinned TLS authenticated server peer=tc27-server fingerprint=$SERVER" "Restored server authenticated"
assert_contains downstream "Pinned TLS authenticated client peer=tc27-client fingerprint=$CLIENT" "Restored client authenticated"
