#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc39.XXXXXX")"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC39_WORK="$WORK" COMPOSE_PROGRESS=plain

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc39.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color --tail 40 || true
    fi
    if [[ "${TC_PAUSE:-0}" == "1" ]]; then
        echo "TC-39 containers retained: project=$PROJECT, artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans; then
            rc=1
        fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-39 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-39 properties comments and pinned-TLS SIGHUP" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for() {
    local path="$1" marker="$2" offset="${3:-0}" deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        if [[ -f "$path" ]] && tail -c "+$((offset + 1))" "$path" | grep -qF "$marker"; then
            return 0
        fi
        sleep 0.2
    done
    report_check "$marker" "observed within 30 seconds" "timed out in $path" 1
}

PAYLOAD_COUNT=0
wait_for_payload() {
    local count deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        compose logs --no-color downstream > "$WORK/payload.log"
        count="$(grep -cF "Random message" "$WORK/payload.log" || true)"
        if (( count > PAYLOAD_COUNT )); then
            PAYLOAD_COUNT="$count"
            return 0
        fi
        sleep 0.2
    done
    report_check "Payload delivery" "new command-sink output" "timed out" 1
}

mark_payload() {
    compose logs --no-color downstream > "$WORK/payload.log"
    PAYLOAD_COUNT="$(grep -cF "Random message" "$WORK/payload.log" || true)"
}

reload() {
    local role="$1" expected="$2" path="$WORK/$1.log" offset
    offset="$(wc -c < "$path" | tr -d ' ')"
    compose kill -s SIGHUP "$role"
    wait_for "$path" "SIGHUP handling completed" "$offset"
    tail -c "+$((offset + 1))" "$path" > "$WORK/$role-reload.log"
    if ! grep -qF "$expected" "$WORK/$role-reload.log"; then
        report_check "$role reload" "$expected" "missing expected result" 1
    fi
    if [[ "$expected" == "Pinned TLS reload:" ]] &&
        grep -qF "reload failed:" "$WORK/$role-reload.log"; then
        report_check "$role reload" "no reload errors" "reload failed" 1
    fi
    report_check "$role reload" "$expected" "observed" 0
}

cd "$ROOT"
for role in upstream downstream; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/$role" "./src/cmd/$role"
    cp "config/testcases/$role-airgap-39-docker.properties" "$WORK/$role.properties"
    chmod 0600 "$WORK/$role.properties"
done
compose build downstream
compose run --rm --no-deps downstream --generate-tls-key \
    --tls-key-output-dir=/airgap/work --tls-key-name=server --tls-key-role=server \
    --json-file-dir=/airgap/work/server-trust
compose run --rm --no-deps upstream --generate-tls-key \
    --tls-key-output-dir=/airgap/work --tls-key-name=client --tls-key-role=client \
    --json-file-dir=/airgap/work/client-trust

compose up -d downstream
wait_for "$WORK/downstream.log" "TLS TCP listener started"
compose up -d upstream
wait_for_payload
for role in upstream downstream; do
    compose logs --no-color "$role" > "$WORK/$role-startup.log"
    if ! grep -qE 'id: tcp#1$' "$WORK/$role-startup.log"; then
        report_check "$role instance ID" "exactly tcp#1" "missing or incorrect value" 1
    fi
done
report_check "Commented configuration startup" "TLS payload and literal tcp#1" "observed" 0

for role in downstream upstream; do
    reload "$role" "Pinned TLS reload:"
done
mark_payload
wait_for_payload
report_check "Delivery after SIGHUP" "new payload after both reloads" "observed" 0

# Unknown active keys must still fail strict reload validation.
cp "$WORK/downstream.properties" "$WORK/downstream.valid"
printf '\nunknownTC39Setting=value # Not a commented-out property.\n' >> "$WORK/downstream.properties"
reload downstream "unknown configuration key: unknownTC39Setting"
mark_payload
wait_for_payload
report_check "Invalid reload retains active connection" "continued payload" "observed" 0
cp "$WORK/downstream.valid" "$WORK/downstream.properties"
reload downstream "Pinned TLS reload:"

# A new connection must also work after the reloads.
compose restart upstream
mark_payload
wait_for_payload
report_check "Fresh TLS connection after reload" "payload from restarted upstream" "observed" 0
