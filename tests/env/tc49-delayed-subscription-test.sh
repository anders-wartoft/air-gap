#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
if [[ "${TC_PAUSE:-0}" == "1" ]]; then
    echo "TC-49 does not support -pause" >&2
    exit 2
fi
airgap_init_test 49
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc49.XXXXXX")"
chmod 0755 "$WORK"
LOG_BASE="${AIRGAP_TEST_LOG_DIR:-$HERE/tmp}"
mkdir -p "$LOG_BASE"
LOG_BASE="$(cd "$LOG_BASE" && pwd)"
TC49_LOG_DIR="$(mktemp -d "$LOG_BASE/tc49.XXXXXX")"
chmod 0755 "$TC49_LOG_DIR"
mkdir -m 0755 "$WORK/bin"
export TC_WORK="$WORK" TC49_LOG_DIR AIRGAP_BIN_DIR="$WORK/bin" COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
SERVICES_STARTED=0
SERVICES_STOPPED=0
echo "TC-49 upstream log: $TC49_LOG_DIR/upstream.log"
echo "TC-49 downstream log: $TC49_LOG_DIR/downstream.log"

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.kafka-transfer.yml" \
        -f "$HERE/docker-compose.tc49.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( SERVICES_STARTED )); then
        if (( ! SERVICES_STOPPED )); then
            compose stop -t 10 upstream downstream >&2 || rc=1
        fi
        compose logs --no-color --no-log-prefix upstream >"$TC49_LOG_DIR/upstream-console.log" 2>&1 || rc=1
        compose logs --no-color --no-log-prefix downstream >"$TC49_LOG_DIR/downstream-console.log" 2>&1 || rc=1
        if (( rc != 0 )); then
            compose logs --no-color --no-log-prefix kafka topic-init >"$TC49_LOG_DIR/kafka-console.log" 2>&1 || rc=1
        fi
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    rm -f "$WORK/kafka-delay.test" "$WORK/airgap-load" "$WORK/keys.txt" \
        "$WORK/bin/upstream" "$WORK/bin/downstream"
    rmdir "$WORK/bin" "$WORK" || rc=1
    echo "TC-49 logs retained at $TC49_LOG_DIR"
    report_summary "TC-49 delayed Kafka subscription stability" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

cd "$HERE/../.."
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-49: unsupported Docker architecture" >&2; exit 2 ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$WORK/kafka-delay.test" ./src/kafka
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/upstream" ./src/cmd/upstream
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/downstream" ./src/cmd/downstream
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/airgap-load" ./tests/env/lib/loadgen
chmod 0755 "$WORK/kafka-delay.test" "$WORK/airgap-load"
compose build upstream downstream
SERVICES_STARTED=1
compose up -d kafka topic-init downstream upstream
KAFKA_CID="$(compose ps -q kafka)"
UPSTREAM_CID="$(compose ps -q upstream)"
DOWNSTREAM_CID="$(compose ps -q downstream)"
[[ -n "$KAFKA_CID" && -n "$UPSTREAM_CID" && -n "$DOWNSTREAM_CID" ]] || {
    echo "TC-49: required containers did not start" >&2
    exit 1
}

deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
    if [[ -f "$TC49_LOG_DIR/upstream.log" ]] &&
       grep -Fq 'Sarama consumer (WithContext) now up and running' "$TC49_LOG_DIR/upstream.log" &&
       grep -Fq 'Sarama consumer (WithContext) sec-180 up and running' "$TC49_LOG_DIR/upstream.log" &&
       grep -Fq 'Sarama consumer (WithContext) sec-86400 up and running' "$TC49_LOG_DIR/upstream.log" &&
       docker exec "$DOWNSTREAM_CID" ss -uln | grep -q ':1234 '; then
        break
    fi
    sleep 1
done
if (( SECONDS >= deadline )); then
    echo "TC-49: upstream threads or downstream UDP listener did not become ready" >&2
    exit 1
fi

echo "TC-49: sending aged and fresh records through three UDP sending threads"
seed_start=$SECONDS
compose run --rm --no-deps producer backlog --topic=source --count=36 --size=64 --partitions=3 --printable \
    --timestamp-offset=-23h59m
compose run --rm --no-deps producer backlog --topic=source --count=36 --size=64 --partitions=3 --printable \
    --timestamp-offset=-2m
compose run --rm --no-deps producer backlog --topic=source --count=36 --size=64 --partitions=3 --printable \
    --timestamp-offset=0s
if (( SECONDS - seed_start >= 55 )); then
    echo "TC-49: seeding took too long to check delayed delivery before eligibility" >&2
    exit 1
fi

group_offsets_match() {
    local group="$1" expected="$2" mode="$3" offsets
    offsets="$(docker exec "$KAFKA_CID" kafka-consumer-groups \
        --bootstrap-server kafka:9092 --group "$group" --describe 2>/dev/null)" || return 1
    awk -v expected="$expected" -v mode="$mode" '
        $2 == "source" && $3 ~ /^[0-9]+$/ {
            n++
            if ($4 == "-") {
                if (mode == "exact") bad=1
            } else if ($4 !~ /^[0-9]+$/ ||
                       (mode == "exact" && $4 != expected) ||
                       (mode == "at-most" && $4 > expected)) {
                bad=1
            }
        }
        END { exit !(n == 3 && !bad) }
    ' <<<"$offsets"
}

group_offsets_match tc49-sec-86400 0 at-most || {
    echo "TC-49: 24-hour group advanced before its first records were eligible" >&2
    exit 1
}
group_offsets_match tc49-sec-180 12 at-most || {
    echo "TC-49: three-minute group advanced beyond eligible records" >&2
    exit 1
}
if (( SECONDS - seed_start >= 55 )); then
    echo "TC-49: offset checks finished too late to verify delivery timing" >&2
    exit 1
fi

deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
    if group_offsets_match tc49-now 36 exact &&
       group_offsets_match tc49-sec-180 24 exact &&
       group_offsets_match tc49-sec-86400 12 exact; then
        break
    fi
    sleep 2
done
if (( SECONDS >= deadline )); then
    echo "TC-49: sending groups did not reach their expected offsets" >&2
    exit 1
fi

deadline=$((SECONDS + 30))
transfer_count=0
while (( SECONDS < deadline )); do
    transfer_count="$(docker exec "$KAFKA_CID" kafka-get-offsets \
        --bootstrap-server kafka:9092 --topic transfer 2>/dev/null |
        awk -F: '{ total += $3 } END { print total + 0 }')"
    (( transfer_count >= 216 )) && break
    sleep 1
done
if [[ "$transfer_count" != 216 ]]; then
    echo "TC-49: downstream Kafka has $transfer_count records, expected 216" >&2
    exit 1
fi
docker exec "$KAFKA_CID" kafka-console-consumer --bootstrap-server kafka:9092 \
    --topic transfer --from-beginning --max-messages 216 --timeout-ms 30000 \
    --property print.key=true --property print.value=false >"$WORK/keys.txt"
awk -F_ '
    NF != 3 || $1 != "transfer" || $2 !~ /^[0-2]$/ || $3 !~ /^[0-9]+$/ || $3 >= 36 {
        printf "unexpected downstream key: %s\n", $0 > "/dev/stderr"
        bad=1
        next
    }
    { counts[$2, $3]++ }
    END {
        for (p=0; p<3; p++) {
            for (offset=0; offset<36; offset++) {
                expected = offset < 12 ? 3 : (offset < 24 ? 2 : 1)
                if (counts[p, offset] != expected) {
                    printf "partition %d offset %d: received %d, expected %d\n",
                        p, offset, counts[p, offset], expected > "/dev/stderr"
                    bad=1
                }
            }
        }
        exit bad
    }
' "$WORK/keys.txt"

if grep -q 'abandoned subscription.*consuming was taking too long' "$TC49_LOG_DIR/upstream.log"; then
    echo "TC-49: upstream abandoned a delayed Kafka subscription" >&2
    exit 1
fi
for thread in sec-180 sec-86400; do
    grep -Fq "Delaying message delivery on thread: $thread" "$TC49_LOG_DIR/upstream.log" || {
        echo "TC-49: no delayed delivery logged for $thread" >&2
        exit 1
    }
done
for cid in "$UPSTREAM_CID" "$DOWNSTREAM_CID"; do
    [[ "$(docker inspect --format '{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}}' "$cid")" == "true false 0" ]] || {
        echo "TC-49: upstream or downstream restarted or stopped unexpectedly" >&2
        exit 1
    }
done
compose stop -t 10 upstream downstream
SERVICES_STOPPED=1
for cid in "$UPSTREAM_CID" "$DOWNSTREAM_CID"; do
    [[ "$(docker inspect --format '{{.State.ExitCode}}' "$cid")" == 0 ]] || {
        echo "TC-49: upstream or downstream did not shut down cleanly" >&2
        exit 1
    }
done
report_check "three sending threads over UDP" \
    "216 Kafka records, correct copies and offsets, no abandonment" \
    "216 records across three partitions" 0

echo "TC-49: running Kafka delay and rebalance regression tests"
airgap_docker_run --rm --network "${PROJECT}_default" \
    -v "$WORK:/airgap/work:ro,z" \
    -e AIRGAP_DELAY_KAFKA_BROKERS=kafka:9092 \
    --entrypoint /airgap/work/kafka-delay.test "$KAFKA_IMAGE" \
    -test.v -test.run '^TestDelayedSubscriptionWithKafka$' -test.timeout 3m
