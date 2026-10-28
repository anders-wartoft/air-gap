#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc40.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC_WORK="$WORK" AIRGAP_BIN_DIR="$WORK" COMPOSE_PROGRESS=plain

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.kafka-transfer.yml" \
        -f "$HERE/docker-compose.tc40.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color --tail 40 || true
    fi
    if [[ "${TC_PAUSE:-0}" == 1 ]]; then
        echo "TC-40 retained: project=$PROJECT, artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans; then rc=1; fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-40 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-40 downstream Kafka startup retry" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_log() {
    local marker="$1" minimum="${2:-1}" count deadline=$((SECONDS + 120))
    while (( SECONDS < deadline )); do
        compose logs --no-color downstream > "$WORK/downstream.log"
        count="$(grep -cF "$marker" "$WORK/downstream.log" || true)"
        if (( count >= minimum )); then return 0; fi
        sleep 0.5
    done
    report_check "$marker" "at least $minimum occurrences" "timed out" 1
}

assert_no_fatal() {
    compose logs --no-color downstream > "$WORK/downstream.log"
    if grep -Eq '\[FATAL\]|\[PANIC\]' "$WORK/downstream.log"; then
        report_check "Downstream startup" "no FATAL/PANIC" "fatal record found" 1
    fi
}

cd "$ROOT"
for role in upstream downstream; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/$role" "./src/cmd/$role"
done
cat > "$WORK/downstream.properties" <<'EOF'
id=tc40
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=tcp
target=kafka
bootstrapServers=kafka:9092
topic=transfer
clientId=tc40
mtu=1500
batchSize=1
kafkaRetryInterval=500
EOF
cat > "$WORK/upstream.properties" <<'EOF'
id=tc40
nic=eth0
targetIP=downstream
targetPort=1234
transport=tcp
source=random
eps=5
encryption=false
payloadSize=1400
topic=transfer
EOF
compose build downstream
compose up -d --no-deps downstream
cid="$(compose ps -q downstream)"
started="$(docker inspect -f '{{.State.StartedAt}}' "$cid")"
wait_for_log "[WARN] Kafka producer unavailable:" 2
assert_no_fatal
report_check "Kafka absent" "multiple WARN retries without FATAL" "observed" 0

compose up -d kafka
compose run --rm topic-init
wait_for_log "Connected to Kafka."
wait_for_log "TCP listener started"
if [[ "$(docker inspect -f '{{.State.StartedAt}}' "$cid")" != "$started" ]] ||
    [[ "$(docker inspect -f '{{.RestartCount}}' "$cid")" != 0 ]]; then
    report_check "Kafka recovery" "same process without restart" "restart detected" 1
fi
compose up -d --no-deps upstream
compose exec -T kafka kafka-console-consumer --bootstrap-server kafka:9092 \
    --topic transfer --from-beginning --max-messages 5 --timeout-ms 30000 > "$WORK/received.log"
if ! grep -qF "Random message" "$WORK/received.log"; then
    report_check "Kafka delivery" "real random payload" "payload missing" 1
fi
assert_no_fatal
report_check "Kafka recovery" "same process delivers real payload" "observed" 0

# Recreate only for the separate shutdown control, targeting a closed local port.
compose stop upstream downstream
cat >> "$WORK/downstream.properties" <<'EOF'
bootstrapServers=127.0.0.1:1
kafkaRetryInterval=60000
EOF
compose up -d --no-deps --force-recreate downstream
wait_for_log "Retrying in 1m0s"
cid="$(compose ps -q downstream)"
compose stop -t 10 downstream
if [[ "$(docker inspect -f '{{.State.ExitCode}}' "$cid")" != 0 ]]; then
    report_check "Shutdown while Kafka unavailable" "exit 0 within 10 seconds" "unclean exit" 1
fi
wait_for_log "Kafka startup canceled by shutdown signal"
assert_no_fatal
report_check "Shutdown while Kafka unavailable" "60-second backoff canceled, exit 0" "observed" 0
