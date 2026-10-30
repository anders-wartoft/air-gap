#!/usr/bin/env bash
# TC-43: upstream (source=kafka) starts before its local Kafka, as after a
# host reboot. It must retry with WARN records, never FATAL/PANIC or restart,
# and the same process must forward events once Kafka answers.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc43.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC_WORK="$WORK" AIRGAP_BIN_DIR="$WORK" COMPOSE_PROGRESS=plain

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.kafka-transfer.yml" \
        -f "$HERE/docker-compose.tc43.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color --tail 40 || true
    fi
    if [[ "${TC_PAUSE:-0}" == 1 ]]; then
        echo "TC-43 retained: project=$PROJECT, artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans; then rc=1; fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-43 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-43 upstream Kafka startup retry" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_log() {
    local service="$1" marker="$2" minimum="${3:-1}" count deadline=$((SECONDS + 120))
    while (( SECONDS < deadline )); do
        compose logs --no-color "$service" > "$WORK/$service.log"
        count="$(grep -cF "$marker" "$WORK/$service.log" || true)"
        if (( count >= minimum )); then return 0; fi
        sleep 0.5
    done
    report_check "$service: $marker" "at least $minimum occurrences" "timed out" 1
}

assert_no_fatal() {
    compose logs --no-color upstream > "$WORK/upstream.log"
    if grep -Eq '\[FATAL\]|\[PANIC\]|^upstream.*panic:' "$WORK/upstream.log"; then
        report_check "Upstream startup" "no FATAL/PANIC" "fatal record found" 1
    fi
}

assert_same_process() {
    local cid="$1" started="$2"
    if [[ "$(docker inspect -f '{{.State.Running}}' "$cid")" != true ]] ||
        [[ "$(docker inspect -f '{{.State.StartedAt}}' "$cid")" != "$started" ]] ||
        [[ "$(docker inspect -f '{{.RestartCount}}' "$cid")" != 0 ]]; then
        report_check "Upstream process" "same process, running, no restart" "exit or restart detected" 1
    fi
}

cd "$ROOT"
for role in upstream downstream; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/$role" "./src/cmd/$role"
done
cat > "$WORK/downstream.properties" <<'EOF2'
id=tc43-downstream
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=tcp
target=kafka
bootstrapServers=kafka:9092
topic=transfer
topicTranslations={"source":"transfer"}
clientId=tc43
mtu=1500
batchSize=1
kafkaRetryInterval=500
EOF2
cat > "$WORK/upstream.properties" <<'EOF2'
id=tc43-upstream
nic=eth0
targetIP=downstream
targetPort=1234
transport=tcp
source=kafka
bootstrapServers=kafka:9092
topic=source
groupID=tc43
encryption=false
payloadSize=1400
kafkaRetryInterval=500
EOF2
compose build downstream
compose up -d --no-deps downstream
compose up -d --no-deps upstream
cid="$(compose ps -q upstream)"
started="$(docker inspect -f '{{.State.StartedAt}}' "$cid")"
wait_for_log upstream "[WARN] Kafka consumer group client unavailable:" 3
assert_no_fatal
assert_same_process "$cid" "$started"
report_check "Kafka absent" "repeated WARN retries without FATAL or restart" "observed" 0

compose up -d kafka
compose run --rm topic-init
wait_for_log downstream "Connected to Kafka."
delivered=0
deadline=$((SECONDS + 120))
n=0
while (( SECONDS < deadline )); do
    n=$((n + 1))
    echo "tc43-after-kafka-start-$n" | compose exec -T kafka \
        kafka-console-producer --bootstrap-server kafka:9092 --topic source >/dev/null
    compose exec -T kafka timeout 10 kafka-console-consumer \
        --bootstrap-server kafka:9092 --topic transfer --from-beginning --timeout-ms 5000 \
        > "$WORK/received.log" 2>/dev/null || true
    if grep -qF "tc43-after-kafka-start-" "$WORK/received.log"; then
        delivered=1
        break
    fi
done
if (( delivered == 0 )); then
    report_check "Kafka recovery" "event produced after Kafka start reaches transfer" "not delivered" 1
fi
assert_no_fatal
assert_same_process "$cid" "$started"
report_check "Kafka recovery" "same upstream process forwards events" "observed" 0

# Separate shutdown control: a closed local port and a 60-second backoff.
compose stop upstream
cat >> "$WORK/upstream.properties" <<'EOF2'
bootstrapServers=127.0.0.1:1
kafkaRetryInterval=60000
EOF2
compose up -d --no-deps --force-recreate upstream
wait_for_log upstream "Retrying in 1m0s"
cid="$(compose ps -q upstream)"
compose stop -t 10 upstream
if [[ "$(docker inspect -f '{{.State.ExitCode}}' "$cid")" != 0 ]]; then
    report_check "Shutdown while Kafka unavailable" "exit 0 within 10 seconds" "unclean exit" 1
fi
wait_for_log upstream "context cancelled while waiting for Kafka"
assert_no_fatal
report_check "Shutdown while Kafka unavailable" "60-second backoff canceled, exit 0" "observed" 0
