#!/usr/bin/env bash
# TC-44: Kafka stops for 60 seconds while upstream (source=kafka), upstream
# (source=random) and downstream are running. All processes must survive and
# resume, upstream must report its Kafka status as running again, and the
# outage errors must be summarized instead of logged once per message.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc44.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
OUTAGE_SECONDS="${TC44_OUTAGE_SECONDS:-60}"
export TC_WORK="$WORK" AIRGAP_BIN_DIR="$WORK" COMPOSE_PROGRESS=plain
SERVICES="downstream upstream upstream-kafka"

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.kafka-transfer.yml" \
        -f "$HERE/docker-compose.tc44.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color --tail 40 || true
    fi
    if [[ "${TC_PAUSE:-0}" == 1 ]]; then
        echo "TC-44 retained: project=$PROJECT, artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans; then rc=1; fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-44 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-44 Kafka outage recovery and grouped logging" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

save_logs() {
    local service
    for service in $SERVICES; do
        compose logs --no-color "$service" > "$WORK/$service.log"
    done
}

# Prints the lines a service logged after the given line number.
since() {
    tail -n +$(($2 + 1)) "$WORK/$1.log"
}

count() {
    grep -cF -- "$1" || true
}

# Prints the compacted log lines matching a marker (first records and
# "Message repeated" summaries), green when the check passed, red otherwise.
show_compacted() {
    local title="$1" marker="$2" code="$3" lines="$4" color="$REPORT_GREEN"
    (( code == 0 )) || color="$REPORT_RED"
    echo "--- $title"
    printf '%s\n' "$lines" | { grep -F -- "$marker" || true; } |
        sed -E 's/^[^|]*\| //' |
        while IFS= read -r line; do printf '%s%s%s\n' "$color" "$line" "$REPORT_RESET"; done
}

produce_until_delivered() {
    local tag="$1" deadline=$((SECONDS + 180)) n=0
    while (( SECONDS < deadline )); do
        n=$((n + 1))
        echo "$tag-$n" | compose exec -T kafka kafka-console-producer \
            --bootstrap-server kafka:9092 --topic source >/dev/null 2>&1 || true
        compose exec -T kafka timeout 15 kafka-console-consumer --bootstrap-server kafka:9092 \
            --topic transfer --from-beginning --timeout-ms 8000 > "$WORK/received.log" 2>/dev/null || true
        if grep -qF "$tag-" "$WORK/received.log"; then return 0; fi
    done
    return 1
}

cd "$ROOT"
for role in upstream downstream; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/$role" "./src/cmd/$role"
done
cat > "$WORK/downstream.properties" <<'EOF2'
id=tc44-downstream
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=tcp
target=kafka
bootstrapServers=kafka:9092
topic=transfer
topicTranslations={"source":"transfer"}
clientId=tc44
mtu=1500
batchSize=1
EOF2
cat > "$WORK/upstream.properties" <<'EOF2'
id=tc44-random
nic=eth0
targetIP=downstream
targetPort=1234
transport=tcp
source=random
eps=20
encryption=false
payloadSize=1400
topic=transfer
EOF2
cat > "$WORK/upstream-kafka.properties" <<'EOF2'
id=tc44-kafka
nic=eth0
targetIP=downstream
targetPort=1234
transport=tcp
source=kafka
bootstrapServers=kafka:9092
topic=source
groupID=tc44
encryption=false
payloadSize=1400
logStatistics=5
EOF2
compose build downstream
compose up -d kafka
compose run --rm topic-init
for service in $SERVICES; do
    compose up -d --no-deps "$service"
done
if ! produce_until_delivered tc44-before; then
    report_check "Before outage" "event from source reaches transfer" "not delivered" 1
fi
report_check "Before outage" "event from source reaches transfer" "observed" 0

ids=""
for service in $SERVICES; do
    cid="$(compose ps -q "$service")"
    ids="$ids $service=$cid@$(docker inspect -f '{{.State.StartedAt}}' "$cid")"
done
save_logs
before_downstream=$(wc -l < "$WORK/downstream.log")
before_upstream=$(wc -l < "$WORK/upstream-kafka.log")
before_transfer=$(compose exec -T kafka kafka-run-class kafka.tools.GetOffsetShell \
    --bootstrap-server kafka:9092 --topic transfer 2>/dev/null | awk -F: '{s+=$3} END {print s+0}')

compose stop kafka
sleep "$OUTAGE_SECONDS"
save_logs
start_upstream=$(wc -l < "$WORK/upstream-kafka.log")
compose start kafka

if ! produce_until_delivered tc44-after; then
    report_check "After outage" "event produced after restart reaches transfer" "not delivered" 1
fi
# Let at least two repeat intervals and two statistics periods pass.
sleep 25
save_logs
after_transfer=$(compose exec -T kafka kafka-run-class kafka.tools.GetOffsetShell \
    --bootstrap-server kafka:9092 --topic transfer 2>/dev/null | awk -F: '{s+=$3} END {print s+0}')
if (( after_transfer <= before_transfer + 20 )); then
    report_check "After outage" "random events resume to transfer" "offset $before_transfer -> $after_transfer" 1
fi
report_check "After outage" "same processes deliver from both upstreams" "transfer offset $before_transfer -> $after_transfer" 0

for entry in $ids; do
    service="${entry%%=*}"; rest="${entry#*=}"; cid="${rest%%@*}"; started="${rest#*@}"
    if [[ "$(docker inspect -f '{{.State.Running}}' "$cid")" != true ]] ||
        [[ "$(docker inspect -f '{{.State.StartedAt}}' "$cid")" != "$started" ]] ||
        [[ "$(docker inspect -f '{{.RestartCount}}' "$cid")" != 0 ]]; then
        report_check "$service process" "same process, running, no restart" "exit or restart detected" 1
    fi
    if grep -Eq '\[FATAL\]|\[PANIC\]|panic:' "$WORK/$service.log"; then
        report_check "$service log" "no FATAL/PANIC" "fatal record found" 1
    fi
done
report_check "Processes" "no FATAL/PANIC, exit or restart" "observed" 0

# Downstream receives about 20 events/s that it cannot write during the
# outage. Without grouping that is one ERROR per event (> 1000 lines).
downstream_outage="$(since downstream "$before_downstream")"
send_failures=$(printf '%s\n' "$downstream_outage" | count "Failed to send message")
send_summaries=$(printf '%s\n' "$downstream_outage" | count "Message repeated")
repeated_failures=$(printf '%s\n' "$downstream_outage" | awk '/Message repeated [0-9]+ times: Failed to send message/ {
    sub(/.*Message repeated /, ""); s += $1 } END { print s + 0 }')
echo "downstream: $send_failures send-failure lines, summarizing $repeated_failures suppressed records"
send_code=0
(( send_failures > 60 || send_summaries == 0 || repeated_failures < 200 )) && send_code=1
show_compacted "Compacted downstream log during the outage" "Failed to send message" "$send_code" "$downstream_outage"
if (( send_code != 0 )); then
    report_check "Downstream outage logging" "<= 60 send-failure lines, summaries for >= 200 records" \
        "$send_failures lines, $repeated_failures summarized" 1
fi
report_check "Downstream outage logging" "send failures summarized per destination and error" \
    "$send_failures lines for $((send_failures + repeated_failures)) failures" 0

upstream_outage="$(since upstream-kafka "$before_upstream")"
status_errors=$(printf '%s\n' "$upstream_outage" | count "Kafka status changed to error")
echo "upstream-kafka: $status_errors status-change lines"
status_code=0
(( status_errors > 40 )) && status_code=1
show_compacted "Compacted upstream (source=kafka) status log during the outage" "Kafka status" "$status_code" "$upstream_outage"
if (( status_code != 0 )); then
    report_check "Upstream outage logging" "<= 40 Kafka status lines" "$status_errors lines" 1
fi
report_check "Upstream outage logging" "status changes summarized" "$status_errors lines" 0

upstream_recovery="$(since upstream-kafka "$start_upstream")"
if ! printf '%s\n' "$upstream_recovery" | grep -qF "Kafka status restored to running"; then
    report_check "Upstream status" "restored to running after Kafka restart" "no restore record" 1
fi
last_status="$(printf '%s\n' "$upstream_recovery" | { grep -F "STATISTICS" || true; } | tail -1 |
    sed -E 's/.*"kafka_status":"([^"]*)".*/\1/')"
if [[ "$last_status" != running ]]; then
    report_check "Upstream status" "STATISTICS kafka_status running" "${last_status:-no statistics}" 1
fi
report_check "Upstream status" "restored and reported as running" "observed" 0
