#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/docker-test.sh"
source "$HERE/lib/report.sh"
if [[ "${TC_PAUSE:-0}" == 1 ]]; then
    echo "TC-50 does not support -pause" >&2
    exit 2
fi
airgap_init_test 50
mkdir -p "$HERE/tmp"
WORK="$(mktemp -d "$HERE/tmp/tc50.XXXXXX")"
chmod 0755 "$WORK"
mkdir -m 0755 "$WORK/bin"
export TC_WORK="$WORK" AIRGAP_BIN_DIR="$WORK/bin" COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
export UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-50-docker.properties
export DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-14-docker.properties
export DEDUP_ENV_FILE=../../config/testcases/dedup-14-docker.env
export CREATE_CONFIG=/airgap/config/testcases/create-14.properties
export RESEND_CONFIG=/airgap/config/testcases/resend-14.properties
export TOPIC_INIT_UPSTREAM_PARTITIONS=1 TOPIC_INIT_DOWNSTREAM_PARTITIONS=1
export TOPIC_INIT_UPSTREAM_TOPICS=transfer TOPIC_INIT_DOWNSTREAM_TOPICS="transfer dedup gaps airgap-internal"
STARTED=0
echo "TC-50 working directory: $WORK"

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" \
        -f "$HERE/docker-compose.tc50.yml" --profile dedup --profile create --profile resend "$@"
}

cleanup() {
    local rc=$? service
    trap - EXIT
    if (( STARTED && rc != 0 )); then
        for service in upstream-a downstream dedup kafka-upstream kafka-upstream-2 kafka-downstream kafka-downstream-2; do
            compose logs --no-color --no-log-prefix "$service" >"$WORK/$service.log" 2>&1 || rc=1
        done
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || rc=1
    if (( rc == 0 )); then
        rm -f "$WORK/bin/upstream" "$WORK/bin/downstream" "$WORK/bin/create" "$WORK/bin/resend" \
            "$WORK/airgap-load" "$WORK/bundle.json" "$WORK/bundle-after.json" \
            "$WORK/received.txt" "$WORK/consumer-error.log" \
            "$WORK/downstream-current.log" "$WORK/dedup-current.log"
        rmdir "$WORK/bin" "$WORK" || rc=1
    fi
    if (( rc != 0 )); then
        echo "TC-50 failure artifacts retained at $WORK" >&2
    fi
    report_summary "TC-50 downstream Kafka outage and resend recovery" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

cd "$ROOT"
case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "TC-50: unsupported Docker architecture" >&2; exit 2 ;;
esac
for role in upstream downstream create resend; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/$role" "./src/cmd/$role"
done
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/airgap-load" ./tests/env/lib/loadgen
compose build upstream-a downstream
STARTED=1
compose up -d --no-build kafka-upstream kafka-upstream-2 kafka-downstream kafka-downstream-2 \
    topic-init-upstream topic-init-downstream downstream upstream-a dedup

upstream_id="$(compose ps -q upstream-a)"
downstream_id="$(compose ps -q downstream)"
dedup_id="$(compose ps -q dedup)"
source_id="$(compose ps -q kafka-upstream)"
source2_id="$(compose ps -q kafka-upstream-2)"
[[ -n "$upstream_id" && -n "$downstream_id" && -n "$dedup_id" && -n "$source_id" && -n "$source2_id" ]] || exit 1
deadline=$((SECONDS + 120))
while (( SECONDS < deadline )); do
    if compose logs --no-color upstream-a | grep -q 'Sarama consumer (WithContext) now up and running' &&
       docker exec "$downstream_id" ss -uln | grep -q ':1234 ' &&
       compose logs --no-color dedup | grep -q 'RUNNING'; then
        break
    fi
    sleep 2
done
if (( SECONDS >= deadline )); then
    echo "TC-50: applications did not become ready" >&2
    exit 1
fi

produce() {
    compose run --rm --no-deps tc50-producer backlog --topic=transfer --partitions=1 \
        --size=64 --printable --timestamp-offset=0s --count="$1"
}

wait_for_count() {
    local topic="$1" expected="$2" deadline=$((SECONDS + 120))
    while (( SECONDS < deadline )); do
        if compose exec -T kafka-downstream kafka-console-consumer \
            --bootstrap-server kafka-downstream:9092 --topic "$topic" --from-beginning \
            --isolation-level read_committed --max-messages "$expected" --timeout-ms 4000 \
            >"$WORK/received.txt" 2>"$WORK/consumer-error.log"; then
            return 0
        fi
        if ! grep -q 'TimeoutException' "$WORK/consumer-error.log"; then
            echo "TC-50: failed to read $topic:" >&2
            cat "$WORK/consumer-error.log" >&2
            return 1
        fi
        sleep 2
    done
    echo "TC-50: $topic did not reach $expected records" >&2
    return 1
}

echo "TC-50: sending baseline records"
produce 4
wait_for_count dedup 4
compose stop kafka-downstream kafka-downstream-2
echo "TC-50: sending five records while downstream Kafka is stopped"
produce 5
deadline=$((SECONDS + 120))
while (( SECONDS < deadline )); do
    compose logs --no-color --no-log-prefix downstream >"$WORK/downstream-current.log"
    failures=0
    for offset in 4 5 6 7 8; do
        if grep -Fq "Failed to send message: topic=transfer partition=0 key=transfer_0_$offset " "$WORK/downstream-current.log"; then
            failures=$((failures + 1))
        fi
    done
    (( failures == 5 )) && break
    sleep 2
done
if (( failures != 5 )); then
    echo "TC-50: observed failures for $failures of the five outage records" >&2
    exit 1
fi

compose start kafka-downstream kafka-downstream-2
echo "TC-50: sending later records to make the outage gaps detectable"
deadline=$((SECONDS + 120))
until compose exec -T kafka-downstream kafka-topics --bootstrap-server kafka-downstream:9092 --list >/dev/null 2>&1; do
    (( SECONDS < deadline )) || { echo "TC-50: downstream Kafka did not recover" >&2; exit 1; }
    sleep 2
done
produce 4
wait_for_count dedup 8
sleep 10
compose run --rm --no-deps create create --resendFileName=/airgap/work/bundle.json --limit=all
python3 - "$WORK/bundle.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as file:
    bundle = json.load(file)
found = set()
for result in bundle["results"]:
    if result["topic"] != "transfer" or result["partition"] != 0:
        raise SystemExit(f"unexpected bundle entry: {result}")
    for gap in result["gaps"]:
        if len(gap) not in (1, 2):
            raise SystemExit(f"invalid gap: {gap}")
        found.update(range(gap[0], gap[-1] + 1))
if found != set(range(4, 9)):
    raise SystemExit(f"bundle offsets {sorted(found)}, expected 4..8")
print("TC-50: bundle contains exactly outage offsets 4..8")
PY
compose run --rm --no-deps resend resend --resendFileName=/airgap/work/bundle.json --eps=50
wait_for_count dedup 13
if ! compose exec -T kafka-downstream kafka-console-consumer --bootstrap-server kafka-downstream:9092 \
    --topic dedup --from-beginning --isolation-level read_committed --timeout-ms 5000 \
    --property print.key=true --property print.value=true >"$WORK/received.txt" 2>"$WORK/consumer-error.log"; then
    if ! grep -q 'TimeoutException' "$WORK/consumer-error.log"; then
        echo "TC-50: failed to read final deduplicated records:" >&2
        cat "$WORK/consumer-error.log" >&2
        exit 1
    fi
fi
python3 - "$WORK/received.txt" <<'PY'
import sys

seen = {}
with open(sys.argv[1], encoding="utf-8") as file:
    for line in file:
        key, value = line.rstrip("\n").split("\t", 1)
        if key in seen:
            raise SystemExit(f"duplicate key: {key}")
        seen[key] = value
expected = {}
for offset in range(13):
    index = offset if offset < 4 else offset - 4 if offset < 9 else offset - 9
    prefix = f"AIRGAP_{index:09d}_"
    expected[f"transfer_0_{offset}"] = prefix + "x" * (64 - len(prefix))
if seen != expected:
    raise SystemExit(f"clean records do not match expected keys and payloads: {seen}")
print("TC-50: all 13 original records recovered exactly once with correct payloads")
PY
sleep 10
compose run --rm --no-deps create create --resendFileName=/airgap/work/bundle-after.json --limit=all
python3 - "$WORK/bundle-after.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as file:
    bundle = json.load(file)
remaining = [(result["topic"], result["partition"], result["gaps"])
             for result in bundle["results"] if result["gaps"]]
if remaining:
    raise SystemExit(f"gaps remain after resend: {remaining}")
print("TC-50: create confirms no gaps remain")
PY
compose logs --no-color --no-log-prefix dedup >"$WORK/dedup-current.log"
python3 - "$WORK/dedup-current.log" <<'PY'
import json
import sys

reports = []
with open(sys.argv[1], encoding="utf-8") as file:
    for line in file:
        if "[MISSING-REPORT] " in line:
            reports.extend(json.loads(line.split("[MISSING-REPORT] ", 1)[1]))
reports = [report for report in reports if report["partition"] == 0]
if not reports:
    raise SystemExit("no dedup gap report for source partition")
latest = reports[-1]
if latest["total_missing"] != 0 or latest["total_received"] != 13 or latest["total_gap_fill"] < 5:
    raise SystemExit(f"dedup did not confirm gap recovery: {latest}")
print("TC-50: dedup confirms all records received and outage gaps closed")
PY
for cid in "$upstream_id" "$downstream_id" "$dedup_id" "$source_id" "$source2_id"; do
    [[ "$(docker inspect --format '{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}}' "$cid")" == "true false 0" ]] || {
        echo "TC-50: an application stopped or restarted" >&2
        exit 1
    }
done
report_check "Downstream Kafka outage recovery" \
    "five failed offsets exported and recovered exactly once" "observed" 0
