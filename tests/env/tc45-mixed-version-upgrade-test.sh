#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/docker-test.sh"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
for tool in git go make docker; do
    command -v "$tool" >/dev/null || { echo "TC-45 requires $tool" >&2; exit 2; }
done
docker info >/dev/null
for tag in 0.1.10-SNAPSHOT 0.1.17.1-SNAPSHOT; do
    git -C "$ROOT" rev-parse --verify "refs/tags/$tag" >/dev/null
done
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc45.XXXXXX")"
mkdir -m 0755 "$WORK/lost-events"
airgap_init_test 45
export TC_WORK="$WORK" TC45_UPSTREAM=modern TC45_DOWNSTREAM=modern COMPOSE_PROGRESS=plain
compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.kafka-transfer.yml" \
        -f "$HERE/docker-compose.tc45.yml" "$@"
}
cleanup() {
    local rc=$?
    trap - EXIT
    if [[ "${TC_PAUSE:-0}" == 1 ]]; then
        echo "TC-45 retained: project=$PROJECT artifacts=$WORK"
    else
        if ! compose down -v --remove-orphans; then rc=1; fi
        if (( rc == 0 )); then
            rm -rf "$WORK"
        else
            echo "TC-45 failure artifacts retained at $WORK" >&2
        fi
    fi
    report_summary "TC-45 mixed-version upgrade" "$rc" || true
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$ROOT"
for version in legacy modern; do
    tag=0.1.10-SNAPSHOT
    [[ "$version" == legacy ]] || tag=0.1.17.1-SNAPSHOT
    mkdir "$WORK/$version" "$WORK/$version-src"
    git archive "$tag" go.mod go.sum src build/go.mk | tar -x -C "$WORK/$version-src"
    (cd "$WORK/$version-src" &&
        make -f build/go.mk src/version/version.go "GIT_VERSION=$tag")
    git rev-parse "$tag^{commit}" > "$WORK/$version.commit"
    for role in upstream downstream; do
        (cd "$WORK/$version-src" &&
            CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/$version/$role" "./src/cmd/$role")
    done
    echo "TC-45 built $tag at $(cat "$WORK/$version.commit")"
done
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/fixture" ./tests/env/lib/tc45fixture

compose build downstream
compose up -d kafka
compose run --rm topic-init

wait_for_log() {
    local service="$1" pattern="$2" log="$3" deadline=$((SECONDS + 60))
    while (( SECONDS < deadline )); do
        compose logs --no-color "$service" > "$log"
        if grep -aEq "$pattern" "$log"; then return 0; fi
        sleep 0.5
    done
    report_check "$service readiness" "$pattern" "timeout; see $log" 1
}

write_configs() {
    local transport="$1" number="$2"
    cat > "$WORK/upstream.properties" <<EOF
id=tc45-upstream
nic=eth0
targetIP=downstream
targetPort=1234
transport=$transport
source=kafka
bootstrapServers=kafka:9092
topic=src$number
groupID=tc45-$number
encryption=false
compressWhenLengthExceeds=0
payloadSize=1400
eps=5
tcpRetryInterval=200
logLevel=INFO
EOF
    cat > "$WORK/downstream.properties" <<EOF
id=tc45-downstream
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=$transport
target=kafka
bootstrapServers=kafka:9092
topic=airgap-internal
topicTranslations={"src$number":"dst$number"}
clientId=tc45-$number
mtu=1500
batchSize=1
logLevel=INFO
EOF
}

number=0
scenario() {
    local us="$1" ds="$2" transport="$3"
    number=$((number + 1))
    export TC45_UPSTREAM="$us" TC45_DOWNSTREAM="$ds"
    local label="$number-$us-to-$ds-$transport"
    mkdir "$WORK/$label"
    write_configs "$transport" "$number"
    cp "$WORK/upstream.properties" "$WORK/downstream.properties" "$WORK/$label/"
    compose run --rm --no-deps fixture --mode=seed "--source=src$number" "--target=dst$number"
    compose up -d --no-deps --force-recreate downstream
    wait_for_log downstream '(UDP listener starting|TCP listener started)' "$WORK/$label/downstream.log"
    compose up -d --no-deps --force-recreate upstream
    local result=0
    compose run --rm --no-deps fixture --mode=verify "--source=src$number" "--target=dst$number" \
        > "$WORK/$label/verdict.log" 2>&1 || result=$?
    for role in upstream downstream; do
        compose logs --no-color "$role" > "$WORK/$label/$role.log"
        cid="$(compose ps -q "$role")"
        if [[ -z "$cid" ]] || [[ "$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$cid")" != "true 0" ]] ||
            grep -aEq '\[FATAL\]|\[PANIC\]|panic:' "$WORK/$label/$role.log"; then
            result=1
        fi
    done
    cat "$WORK/$label/verdict.log"
    report_check "$label" "six exact records, no duplicates/restart/panic" "exit=$result" "$result"
    compose stop -t 10 upstream downstream
}

for transport in udp tcp; do
    scenario legacy modern "$transport"
    scenario modern legacy "$transport"
done
