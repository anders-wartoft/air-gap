#!/usr/bin/env bash
# TC-42 — the compacted gap topic stays bounded to the active windows.
#
# 1. Run the current dedup with WINDOW_SIZE=100 and MAX_WINDOWS=2 against a
#    compacted gap topic with aggressive cleaner settings.
# 2. Produce 20 windows, one at a time, each missing one offset, so every
#    window is emitted and later purged.
# 3. Require one tombstone and one WARN per purged window, and that after
#    compaction the topic physically holds only the MAX_WINDOWS active keys.
# 4. Run `create` and require only the active windows' gaps in the bundle.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc42.XXXXXX")"
chmod 0755 "$WORK"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC_WORK="$WORK" COMPOSE_PROGRESS=plain
WAIT_SECONDS="${TC42_WAIT_SECONDS:-180}"
WINDOWS=20
MAX_WINDOWS=2
FAILED=0

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc42.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    (( rc == 0 && FAILED )) && rc=1
    if (( rc != 0 )); then
        compose logs --no-color --tail 60 dedup || true
    fi
    if [[ "${TC_PAUSE:-0}" == 1 ]]; then
        echo "TC-42 retained: project=$PROJECT, artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans >/dev/null 2>&1; then rc=1; fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-42 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-42 bounded gap topic" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

check() {
    report_check "$@" || FAILED=1
}

current_jar() {
    local jar="${TC42_CURRENT_DEDUP_JAR:-}"
    if [[ -z "$jar" ]]; then
        jar="$(ls "$ROOT"/java-streams/target/air-gap-deduplication-fat-*-linux-amd64.jar 2>/dev/null | head -1 || true)"
    fi
    if [[ -z "$jar" || ! -s "$jar" ]]; then
        echo "TC-42: no current dedup fat JAR; build it with 'make build-java' or set TC42_CURRENT_DEDUP_JAR" >&2
        exit 2
    fi
    echo "$jar"
}

# produce_window N — write keys source_0_<offset> for window N, omitting N*100+5.
produce_window() {
    local base=$(( $1 * 100 )) i
    for ((i = base; i < base + 100; i++)); do
        (( i == base + 5 )) || printf 'source_0_%d|tc42 event %d\n' "$i" "$i"
    done | compose exec -T kafka kafka-console-producer --bootstrap-server kafka:9092 \
        --topic transfer --property parse.key=true --property key.separator='|' >/dev/null
}

# dump_gap_keys — keys currently stored in the (compacted) topic.
dump_gap_keys() {
    # --timeout-ms only fires when idle; dedup emits every second, so bound it.
    compose exec -T kafka timeout 10 kafka-console-consumer --bootstrap-server kafka:9092 --topic gaps \
        --from-beginning --timeout-ms 3000 --property print.key=true --property print.value=false \
        > "$WORK/gap-keys.txt" 2>/dev/null || true
}

# Keys physically present in the topic (records and tombstones).
distinct_keys() {
    LC_ALL=C sort -u "$WORK/gap-keys.txt" | grep -c . || true
}

# Latest record per key in the live capture: "tombstone KEY" or "live KEY".
latest_state() {
    LC_ALL=C awk -F'|' '{ last[$1] = ($2 == "null") ? "tombstone" : "live" }
        END { for (k in last) print last[k], k }' "$WORK/gaps-live.txt" | LC_ALL=C sort
}

# Tombstones are removed by the cleaner after delete.retention.ms, so capture
# every record as it is written with a consumer started before the producer.
start_live_capture() {
    compose exec -d kafka bash -c "kafka-console-consumer --bootstrap-server kafka:9092 --topic gaps \
        --from-beginning --property print.key=true --property key.separator='|' > /tmp/gaps-live.txt 2>/dev/null"
}

read_live_capture() {
    compose exec -T kafka cat /tmp/gaps-live.txt > "$WORK/gaps-live.txt" 2>/dev/null || true
}

cd "$ROOT"
cp "$(current_jar)" "$WORK/dedup-current.jar"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/create" ./src/cmd/create
cat > "$WORK/create.properties" <<'EOF'
bootstrapServers=kafka:9092
topic=gaps
groupID=tc42-create
logLevel=INFO
limit=all
resendFileName=/airgap/work/gaps.json
EOF
cat > "$WORK/log4j2.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<Configuration status="WARN">
  <Appenders>
    <Console name="Console" target="SYSTEM_OUT">
      <PatternLayout pattern="%d{HH:mm:ss} %-5p %c{1} - %m%n"/>
    </Console>
  </Appenders>
  <Loggers>
    <Root level="warn"><AppenderRef ref="Console"/></Root>
  </Loggers>
</Configuration>
EOF
chmod 0644 "$WORK"/*.jar "$WORK/create.properties" "$WORK/log4j2.xml"

compose up -d --wait kafka
compose up --exit-code-from topic-init topic-init >/dev/null
start_live_capture
compose up -d --no-deps dedup

# One window at a time so the periodic emitter writes a record for each.
for ((w = 0; w < WINDOWS; w++)); do
    produce_window "$w"
    sleep 2
done

purged=$(( WINDOWS - MAX_WINDOWS ))
deadline=$((SECONDS + WAIT_SECONDS))
while :; do
    read_live_capture
    tombstones="$(latest_state | grep -c '^tombstone ' || true)"
    (( tombstones >= purged || SECONDS >= deadline )) && break
    sleep 2
done
live="$(latest_state | awk '$1 == "live" { print $2 }' | paste -sd, -)"
check "Latest record per key is a tombstone for purged windows" "$purged" "$tombstones" "$(( tombstones != purged ))"
expected_live="source:0:$(( (WINDOWS - 2) * 100 )),source:0:$(( (WINDOWS - 1) * 100 ))"
check "Keys with a live latest record" "$expected_live" "${live:-none}" "$([[ "$live" == "$expected_live" ]]; echo $?)"

compose logs --no-color dedup > "$WORK/dedup.log" 2>&1
warns="$(grep -c 'left the detection range with 1 missing offsets' "$WORK/dedup.log" || true)"
check "WARN per purged window with gaps" "$purged" "$warns" "$(( warns != purged ))"

written="$(LC_ALL=C cut -d'|' -f1 "$WORK/gaps-live.txt" | LC_ALL=C sort -u | grep -c . || true)"

# Compaction: the purged windows' records and, after delete.retention.ms,
# their tombstones must disappear from the topic.
keys=0
deadline=$((SECONDS + WAIT_SECONDS))
while :; do
    dump_gap_keys
    keys="$(distinct_keys)"
    (( keys <= MAX_WINDOWS || SECONDS >= deadline )) && break
    sleep 5
done
check "Distinct keys in gap topic after compaction (of $written written)" "<= $MAX_WINDOWS" "$keys" "$(( keys > MAX_WINDOWS ))"

create_rc=0
compose run --rm --no-deps create > "$WORK/create.log" 2>&1 || create_rc=$?
check "create" "exit 0" "exit $create_rc" "$create_rc"
error_lines="$(grep -Ec '\[(ERROR|FATAL|PANIC)\]|"severityText":"(ERROR|FATAL)"' "$WORK/create.log" || true)"
check "ERROR/FATAL records in create log" "0" "$error_lines" "$(( error_lines != 0 ))"

gaps="missing bundle"
if [[ -s "$WORK/gaps.json" ]]; then
    gaps="$(python3 - "$WORK/gaps.json" <<'EOF'
import json, sys
bundle = json.load(open(sys.argv[1]))
rows = [r for r in bundle.get("results", []) if r.get("topic") == "source" and r.get("partition") == 0]
print(json.dumps(rows[0]["gaps"], separators=(",", ":")) if len(rows) == 1 else f"{len(rows)} results for source/0")
EOF
)"
fi
expected="[[$(( (WINDOWS - 2) * 100 + 5 ))],[$(( (WINDOWS - 1) * 100 + 5 ))]]"
check "Bundle gaps for source/0 (active windows only)" "$expected" "$gaps" "$([[ "$gaps" == "$expected" ]]; echo $?)"
