#!/usr/bin/env bash
# TC-41 — graceful upgrade from dedup 0.1.10 legacy JSON gap records.
#
# 1. Build the real dedup at TC41_LEGACY_TAG (default 0.1.10-SNAPSHOT), which
#    writes JSON gap records keyed "topic_partition:windowMin".
# 2. Produce keyed records with known missing offsets and let it emit gaps,
#    including a window it purges (that window then exists only as JSON).
# 3. Stop it and start the current dedup in place (same state volume and
#    APPLICATION_ID), produce more data with a new gap (binary records). The
#    current dedup purges the restored windows and tombstones both their
#    binary and legacy keys (REQ-54).
# 4. Run the current `create` and require the legacy-only gaps and the new
#    gap in the bundle, the purged window's gap gone, and no per-record parse
#    errors.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc41.XXXXXX")"
chmod 0755 "$WORK"
PROJECT="$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
export TC_WORK="$WORK" COMPOSE_PROGRESS=plain
LEGACY_TAG="${TC41_LEGACY_TAG:-0.1.10-SNAPSHOT}"
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/airgap-tests"
WAIT_SECONDS="${TC41_WAIT_SECONDS:-180}"
FAILED=0

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc41.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    (( rc == 0 && FAILED )) && rc=1
    if (( rc != 0 )); then
        compose logs --no-color --tail 60 dedup-legacy dedup || true
    fi
    if [[ "${TC_PAUSE:-0}" == 1 ]]; then
        echo "TC-41 retained: project=$PROJECT, artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans >/dev/null 2>&1; then rc=1; fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-41 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-41 graceful upgrade of legacy gap records" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

check() {
    report_check "$@" || FAILED=1
}

# Build (or reuse a cached copy of) the dedup fat JAR at LEGACY_TAG.
legacy_jar() {
    local jar="${TC41_LEGACY_DEDUP_JAR:-$CACHE_DIR/air-gap-deduplication-fat-$LEGACY_TAG.jar}"
    if [[ -s "$jar" ]]; then
        echo "$jar"
        return
    fi
    if ! git -C "$ROOT" rev-parse -q --verify "refs/tags/$LEGACY_TAG" >/dev/null; then
        echo "TC-41: git tag $LEGACY_TAG not found; run 'git fetch --tags' or set TC41_LEGACY_DEDUP_JAR" >&2
        exit 2
    fi
    local src="$WORK/legacy-src"
    mkdir -p "$src" "$CACHE_DIR"
    git -C "$ROOT" archive "$LEGACY_TAG" java-streams | tar -x -C "$src"
    echo "TC-41: building dedup $LEGACY_TAG (cached at $jar)" >&2
    if command -v mvn >/dev/null 2>&1; then
        (cd "$src/java-streams" && mvn -q -B package -DskipTests) >&2
    else
        mkdir -p "$CACHE_DIR/m2"
        docker run --rm -v "$src:/src:z" -v "$CACHE_DIR/m2:/root/.m2:z" -w /src/java-streams \
            docker.io/library/maven:3.9-eclipse-temurin-17 mvn -q -B package -DskipTests >&2
    fi
    local built
    built="$(ls "$src"/java-streams/target/air-gap-deduplication-fat-*.jar | head -1)"
    cp "$built" "$jar"
    echo "$jar"
}

current_jar() {
    local jar="${TC41_CURRENT_DEDUP_JAR:-}"
    if [[ -z "$jar" ]]; then
        jar="$(ls "$ROOT"/java-streams/target/air-gap-deduplication-fat-*-linux-amd64.jar 2>/dev/null | head -1 || true)"
    fi
    if [[ -z "$jar" || ! -s "$jar" ]]; then
        echo "TC-41: no current dedup fat JAR; build it with 'make build-java' or set TC41_CURRENT_DEDUP_JAR" >&2
        exit 2
    fi
    echo "$jar"
}

# produce FROM TO SKIP... — write keys source_0_<offset> for FROM..TO except SKIP.
produce() {
    local from="$1" to="$2"
    shift 2
    local skip=" $* " i
    for ((i = from; i <= to; i++)); do
        [[ "$skip" == *" $i "* ]] || printf 'source_0_%d|tc41 event %d\n' "$i" "$i"
    done | compose exec -T kafka kafka-console-producer --bootstrap-server kafka:9092 \
        --topic transfer --property parse.key=true --property key.separator='|' >/dev/null
}

dump_gaps() {
    # --timeout-ms only fires when idle; dedup emits every few seconds, so bound it.
    compose exec -T kafka timeout 15 kafka-console-consumer --bootstrap-server kafka:9092 --topic gaps \
        --from-beginning --timeout-ms 4000 --property print.key=true \
        --property key.separator='|' > "$WORK/gaps-topic.txt" 2>/dev/null || true
}

# wait_for_gap_record SUBJECT KEY [VALUE-SUBSTRING]
wait_for_gap_record() {
    local subject="$1" key="$2" needle="${3-}" deadline=$((SECONDS + WAIT_SECONDS))
    while (( SECONDS < deadline )); do
        dump_gaps
        local hits
        hits="$(grep -a "^${key}|" "$WORK/gaps-topic.txt" | grep -acF -- "$needle" || true)"
        if (( hits > 0 )); then
            check "$subject" "record $key${needle:+ containing $needle}" "found"
            return 0
        fi
        sleep 2
    done
    check "$subject" "record $key${needle:+ containing $needle}" "not found within ${WAIT_SECONDS}s" 1
    return 1
}

cd "$ROOT"
cp "$(legacy_jar)" "$WORK/dedup-legacy.jar"
cp "$(current_jar)" "$WORK/dedup-current.jar"
if [[ -n "${TC41_CREATE_REF:-}" ]]; then
    # Negative control: build `create` from an older ref (e.g. 0.1.16-SNAPSHOT).
    mkdir -p "$WORK/create-src"
    git archive "$TC41_CREATE_REF" go.mod go.sum src | tar -x -C "$WORK/create-src"
    (cd "$WORK/create-src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/create" ./src/cmd/create)
else
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/create" ./src/cmd/create
fi
cat > "$WORK/create.properties" <<'EOF'
bootstrapServers=kafka:9092
topic=gaps
groupID=tc41-create
logLevel=INFO
limit=all
resendFileName=/airgap/work/gaps.json
EOF
chmod 0644 "$WORK"/*.jar "$WORK/create.properties"

compose up -d --wait kafka
compose up --exit-code-from topic-init topic-init >/dev/null

# Phase 1: legacy dedup. WINDOW_SIZE=100, MAX_WINDOWS=2.
# Window 0 misses 5 and 10-12 and is purged once offset 200 arrives, so it
# remains only as a legacy JSON record. Window 300 misses 350.
produce 0 399 5 10 11 12 350
compose up -d dedup-legacy
wait_for_gap_record "Legacy dedup $LEGACY_TAG emits purged window as JSON" \
    "source_0:0" '[[5],[10,12]]' || exit 1
wait_for_gap_record "Legacy dedup $LEGACY_TAG emits active window as JSON" \
    "source_0:300" '[[50]]' || exit 1
compose stop -t 30 dedup-legacy >/dev/null
compose logs --no-color dedup-legacy > "$WORK/dedup-legacy.log" 2>&1

# Phase 2: in-place upgrade to the current dedup, then a new gap at 450.
# Offsets 400-599 push the restored windows 200 and 300 out of range, so the
# current dedup tombstones them (binary and legacy keys); gap 350 is dropped.
produce 400 599 450
compose up -d --no-deps dedup
wait_for_gap_record "Current dedup emits binary gap record after upgrade" "source:0:400" || exit 1
sleep 5
if [[ -n "$(compose ps -q --status running dedup)" ]]; then
    check "Current dedup after upgrade" "still running" "running"
else
    check "Current dedup after upgrade" "still running" "exited" 1
fi
compose logs --no-color dedup > "$WORK/dedup-current.log" 2>&1
dump_gaps
if grep -aq '^source:0:0|' "$WORK/gaps-topic.txt"; then
    check "Window 0 (purged before upgrade)" "only in legacy JSON format" "binary record present" 1
else
    check "Window 0 (purged before upgrade)" "only in legacy JSON format" "legacy only"
fi
for key in source:0:300 source_0:300; do
    if grep -aqx "${key}|null" "$WORK/gaps-topic.txt"; then
        check "Restored window 300 purged after upgrade" "tombstone for $key" "found"
    else
        check "Restored window 300 purged after upgrade" "tombstone for $key" "not found" 1
    fi
done

# Phase 3: current create reads both formats.
create_rc=0
compose run --rm --no-deps create > "$WORK/create.log" 2>&1 || create_rc=$?
check "create (${TC41_CREATE_REF:-current})" "exit 0" "exit $create_rc" "$create_rc"

parse_errors="$(grep -c 'Failed to parse gap message' "$WORK/create.log" || true)"
check "Per-record parse errors in create log" "0" "$parse_errors" "$(( parse_errors != 0 ))"
error_lines="$(grep -Ec '\[(ERROR|FATAL|PANIC)\]|"severityText":"(ERROR|FATAL)"' "$WORK/create.log" || true)"
check "ERROR/FATAL records in create log" "0" "$error_lines" "$(( error_lines != 0 ))"
summary="$(grep -Eo 'Read [0-9]+ gap records in the legacy JSON format' "$WORK/create.log" | head -1 || true)"
if [[ -n "$summary" && "$(grep -c 'legacy JSON format' "$WORK/create.log")" -eq 1 ]]; then
    check "Legacy record summary" "exactly one summary line" "$summary"
else
    check "Legacy record summary" "exactly one summary line" "'${summary:-none}'" 1
fi

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
expected='[[5],[10,12],[450]]'
check "Bundle gaps for source/0 (legacy + binary, purged window dropped)" "$expected" "$gaps" "$([[ "$gaps" == "$expected" ]]; echo $?)"
