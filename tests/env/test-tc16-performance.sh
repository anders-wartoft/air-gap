#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc16-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/env/lib" "$WORK/env/bin" "$WORK/mock" "$WORK/state"
cp "$HERE/tc16-performance.sh" "$HERE/run-testcase.sh" "$WORK/env/"
cp "$HERE/lib/report.sh" "$HERE/lib/docker-test.sh" "$HERE/lib/docker-images.sh" "$WORK/env/lib/"
cp "$HERE/lib/dedup-jar.sh" "$WORK/env/lib/"
cp "$HERE/lib/udp-calibration.sh" "$WORK/env/lib/"
cp "$HERE/lib/tc16-fake-docker.sh" "$WORK/mock/docker"
chmod +x "$WORK/mock/docker"
touch "$WORK/env/bin/LogGenerator-1.1-9.jar"
export PATH="$WORK/mock:$PATH" FAKE_STATE="$WORK/state"
export TC16_COUNT=10 TC16_PARTITIONS=1 TC16_SETTLE=1 TC16_POLL=1
unset TC16_TIMEOUT TC16_EPS AIRGAP_TEST_UDP_PACING
printf 'dedup fixture' > "$WORK/dedup.jar"
export TC16_CURRENT_DEDUP_JAR="$WORK/dedup.jar"

run_case() {
    local expected="$1"; shift
    local rc=0
    bash "$WORK/env/run-testcase.sh" "$@" > "$WORK/result" 2>&1 || rc=$?
    if [[ "$rc" != "$expected" ]]; then
        cat "$WORK/result"
        echo "Expected exit $expected, got $rc" >&2
        exit 1
    fi
}
line() { grep -nE "$1" "$FAKE_STATE/calls" | head -1 | cut -d: -f1; }
before() { [[ "$(line "$1")" -lt "$(line "$2")" ]] || { echo "Ordering failed: $1 before $2" >&2; exit 1; }; }
reset_fake() {
    rm -f "$FAKE_STATE/upstream" "$FAKE_STATE/downstream" "$FAKE_STATE/lg-producer" \
        "$FAKE_STATE/dedup" "$FAKE_STATE/lg-sink" "$FAKE_STATE/partial-bind"
    : > "$FAKE_STATE/calls"
}

test_dedup_polling() (
    export FAKE_REMOTE=1 TC16_TIMEOUT=4 TC16_RESULTS_DIR="$WORK/poll-results"
    local scenario expected
    for scenario in once always error; do
        rm -f "$FAKE_STATE/downstream" "$FAKE_STATE/dedup" "$FAKE_STATE/lg-sink"
        : > "$FAKE_STATE/calls"
        expected=1
        [[ "$scenario" != once ]] || expected=0
        if [[ "$scenario" == once ]]; then
            FAKE_UDP_READY=delayed FAKE_DEDUP_POLL="$scenario" run_case "$expected" TC-16-DS
            [[ "$(grep -c 'logs --no-color downstream$' "$FAKE_STATE/calls")" == 2 ]]
            before 'logs --no-color downstream$' 'up .*dedup$'
        else
            FAKE_DEDUP_POLL="$scenario" run_case "$expected" TC-16-DS
        fi
        case "$scenario" in
            once)
                grep -q 'dedup offset poll timed out' "$WORK/result"
                grep -q 'TC-16 PASS: all 10 sequence IDs delivered exactly once' "$WORK/result"
                [[ "$(grep -c '^dedup,' "$TC16_RESULTS_DIR/progress.csv")" == 1 ]]
                ;;
            always)
                grep -q 'dedup timed out: no successful offset poll' "$WORK/result"
                [[ "$(grep -c 'dedup offset poll timed out' "$WORK/result")" -gt 1 ]]
                if grep -Eq '^dedup,' "$TC16_RESULTS_DIR/progress.csv" ||
                    grep -Eq 'up .*lg-sink$' "$FAKE_STATE/calls"; then
                    echo "Timed-out polls were treated as progress" >&2
                    exit 1
                fi
                ;;
            error)
                grep -q 'dedup offset poll failed: group=tc16-dedup topic=tc16-transfer exit=2' "$WORK/result"
                [[ "$(grep -c 'kafka-consumer-groups' "$FAKE_STATE/calls")" == 1 ]]
                ;;
        esac
    done
    rm -f "$FAKE_STATE/downstream" "$FAKE_STATE/dedup" "$FAKE_STATE/lg-sink"
    : > "$FAKE_STATE/calls"
    echo "PASS: delayed UDP socket binds with verbose logs, transient/persistent dedup timeouts, and command errors"
)

test_dedup_polling
if [[ "${1:-}" == --polling-only ]]; then exit 0; fi

run_case 0 16
grep -qx '300' "$FAKE_STATE/timeout"
grep -qx '30000' "$FAKE_STATE/eps"
grep -q 'TC-16 PASS' "$WORK/result"
if [[ -z "${NO_COLOR-}" ]]; then
    grep -Fq $'\033[0;32m─── TC-16 (all): PASS' "$WORK/result"
    grep -Fq $'  \033[0;32mStage' "$WORK/result"
fi
grep -q 'Unique delivered sequence IDs, expected 10, 10' "$WORK/result"
grep -q 'Duplicate sequence IDs, expected 0, 0' "$WORK/result"
grep -q 'Next expected sequence ID, expected 11, 11' "$WORK/result"
grep -q 'Measured phase.*Events.*Elapsed.*Average EPS' "$WORK/result"
grep -q 'generation .*| *10 .*| *[0-9]*s .*|' "$WORK/result"
grep -q 'Total runtime : [0-9]*s' "$WORK/result"
previous=0
for message in \
    'Starting ds Kafka cluster' \
    'Starting downstream UDP receiver' \
    'Starting us Kafka cluster' \
    'Adding 10 test events' \
    'Deduplicating data' \
    'Checking received data' \
    'Collecting diagnostic logs'; do
    current="$(grep -nF "$message" "$WORK/result" | head -1 | cut -d: -f1)"
    [[ "$current" -gt "$previous" ]]
    previous="$current"
done
grep -q 'Sending data to downstream over UDP' "$WORK/result"
grep -q 'Waiting for downstream Kafka to store all 10 events' "$WORK/result"
grep -q 'allowing 1s to check for late or extra events' "$WORK/result"
before 'up .*lg-producer$' 'up .*upstream$'
before 'stop .*downstream$' 'up .*dedup$'
before 'stop .*upstream$' 'up .*dedup$'
before 'stop .*dedup$' 'up .*lg-sink$'
echo "PASS: single-machine default timeout, phase ordering and full verification"

reset_fake
AIRGAP_TEST_UDP_PACING=38583,54016200,32 run_case 0 16
grep -qx '30866' "$FAKE_STATE/eps"
reset_fake
TC16_EPS=45000 AIRGAP_TEST_UDP_PACING=38583,54016200,32 run_case 0 16
grep -qx '45000' "$FAKE_STATE/eps"
echo "PASS: calibrated rate uses headroom and explicit EPS overrides it"

export TC16_TIMEOUT=3
rc=0
TC16_CURRENT_DEDUP_JAR="$WORK/missing.jar" bash "$WORK/env/tc16-performance.sh" all > "$WORK/result" 2>&1 || rc=$?
[[ "$rc" == 1 ]]
grep -q 'cannot select current dedup JAR' "$WORK/result"
if grep -q 'Starting ds Kafka' "$WORK/result"; then
    echo "Missing dedup JAR was not rejected before setup" >&2; exit 1
fi
echo "PASS: missing dedup artifact fails before workload setup"

# Each mock run starts with new state; real runs require explicit reset.
rm "$FAKE_STATE/upstream" "$FAKE_STATE/lg-producer"
: > "$FAKE_STATE/calls"
export FAKE_DROP=1
run_case 1 16
grep -qx '3' "$FAKE_STATE/timeout"
grep -q 'transfer timed out' "$WORK/result"
grep -q 'Upstream sent (last statistics).*| 10' "$WORK/result"
grep -q 'Downstream received (last statistics).*| 9' "$WORK/result"
grep -q 'Missing raw Kafka records : 1 (10.0000%)' "$WORK/result"
grep -q 'transfer (incomplete/armed wall clock).*| *9 .*|' "$WORK/result"
if [[ -z "${NO_COLOR-}" ]]; then
    grep -Fq $'\033[0;31m─── TC-16 (all): FAIL' "$WORK/result"
fi
if grep -qE 'up .*dedup$|up .*lg-sink$' "$FAKE_STATE/calls"; then
    echo "Dedup/verification started despite lost records" >&2; exit 1
fi
unset FAKE_DROP
echo "PASS: missing UDP records fail without starting later phases"

export FAKE_DUPLICATES=1
run_case 2 TC-16-DS down-only --bad-option
grep -q 'TC-16 accepts only a side' "$WORK/result"
rc=0
bash "$WORK/env/tc16-performance.sh" verify > "$WORK/result" 2>&1 || rc=$?
[[ "$rc" == 1 ]]
grep -q 'verification failed' "$WORK/result"
grep -q 'TC-16 (verify): FAIL' "$WORK/result"
grep -q 'Duplicate sequence IDs, expected 0, 1' "$WORK/result"
unset FAKE_DUPLICATES
echo "PASS: extra arguments and duplicate verification fail explicitly"

: > "$FAKE_STATE/calls"
rm "$FAKE_STATE/upstream" "$FAKE_STATE/lg-producer"
unset TC16_TIMEOUT
run_case 0 TC-16-US
grep -qx '1800' "$FAKE_STATE/timeout"
grep -q 'sender startup only; delivery verification runs on DS: PASS' "$WORK/result"
if grep -qE 'up .*downstream$|up .*dedup$|up .*lg-sink$|up .*kafka-ds' "$FAKE_STATE/calls"; then
    echo "US alias started downstream workloads" >&2; exit 1
fi
echo "PASS: US alias starts only its own services"

for service in upstream lg-producer downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
: > "$FAKE_STATE/calls"
export FAKE_REMOTE=1
run_case 0 TC-16-DS
grep -qx '1800' "$FAKE_STATE/timeout"
if grep -qE 'up .*upstream$|up .*lg-producer$|up .*kafka-us' "$FAKE_STATE/calls"; then
    echo "DS alias started upstream workloads" >&2; exit 1
fi
echo "PASS: DS alias starts only its own services and defers dedup/verification"
export TC16_TIMEOUT=3

for service in downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
export FAKE_EXTRA=1
run_case 1 TC-16-DS
grep -q 'received 11 raw records, expected 10' "$WORK/result"
unset FAKE_REMOTE FAKE_EXTRA
echo "PASS: excess raw records fail immediately"

for service in downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
run_case 1 TC-16-DS
grep -q 'DS does not generate or send events' "$WORK/result"
grep -q 'no raw Kafka records arrived' "$WORK/result"
grep -q 'transfer timed out: current=0' "$WORK/result"
echo "PASS: DS without a sender explains the zero-record timeout"

export FAKE_ALREADY_RUNNING=1
run_case 1 TC-16-DS
grep -q 'bash .*tc16-performance.sh.* reset-ds' "$WORK/result"
grep -q 'deletes this side' "$WORK/result"
unset FAKE_ALREADY_RUNNING
echo "PASS: retained stack error prints the complete destructive reset command"

for service in upstream lg-producer downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
NO_COLOR=1 run_case 0 16
grep -q 'TC-16 (all): PASS' "$WORK/result"
if grep -Fq $'\033[' "$WORK/result"; then
    echo "NO_COLOR output contains ANSI escapes" >&2; exit 1
fi
echo "PASS: colored success/failure summaries and NO_COLOR output"

mkdir -p "$WORK/env/testcases"
cp "$HERE/run-testcases.sh" "$WORK/env/"
cp "$HERE/testcases/16.env" "$WORK/env/testcases/"
cat > "$WORK/env/calibrate-test-udp.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$1"
printf 'udpPacingDatagramsPerSecond=38583\nudpPacingBytesPerSecond=54016200\nudpPacingBurstDatagrams=32\n' \
    > "$1/pacing.properties"
EOF
chmod +x "$WORK/env/calibrate-test-udp.sh"
for selection in 16 1-; do
    # Seed retained input: batch preparation must remove it without user resets.
    touch "$FAKE_STATE/lg-producer"
    rm -f "$FAKE_STATE/eps"
    : > "$FAKE_STATE/calls"
    bash "$WORK/env/run-testcases.sh" "$selection" > "$WORK/result" 2>&1
    grep -q 'TC-16 PASS: all 10 sequence IDs' "$WORK/result"
    grep -qx '30866' "$FAKE_STATE/eps"
    grep -q 'testcase 16 PASS' "$WORK/result"
    grep -q 'Cleaning up TC-16 containers and Kafka volumes' "$WORK/result"
    before 'airgap-tc16-[a-z0-9]+-us .*down -v --remove-orphans' 'up .*lg-producer$'
    before 'airgap-tc16-[a-z0-9]+-ds .*down -v --remove-orphans' 'up .*downstream$'
    for side in us ds; do
        [[ "$(grep -cE "airgap-tc16-[a-z0-9]+-$side .*down -v --remove-orphans" "$FAKE_STATE/calls")" == 2 ]]
    done
    for service in upstream lg-producer downstream dedup lg-sink; do
        [[ ! -f "$FAKE_STATE/$service" ]]
    done
    if grep -q 'testcase 16 SKIP' "$WORK/result"; then
        echo "Batch skipped the staged benchmark" >&2; exit 1
    fi
done
for service in upstream lg-producer downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
rc=0
FAKE_DROP=1 bash "$WORK/env/run-testcases.sh" 16 > "$WORK/result" 2>&1 || rc=$?
[[ "$rc" == 1 ]]
grep -q 'testcase 16 FAIL' "$WORK/result"
grep -q 'Missing raw Kafka records : 1 (10.0000%)' "$WORK/result"
if grep -q 'no verdict block captured' "$WORK/result"; then
    echo "Batch did not capture failure details" >&2; exit 1
fi
for service in upstream lg-producer downstream dedup lg-sink; do
    [[ ! -f "$FAKE_STATE/$service" ]]
done
echo "PASS: batch explicit/range selections run TC-16 and propagate delivery failures"

rc=0
FAKE_CLEANUP_FAIL=1 bash "$WORK/env/run-testcases.sh" 16 > "$WORK/result" 2>&1 || rc=$?
[[ "$rc" == 1 ]]
grep -q 'failed to clean up us stack' "$WORK/result"
grep -q 'failed to clean up ds stack' "$WORK/result"
echo "PASS: batch cleanup errors fail explicitly and attempt both sides"

rm "$WORK/env/bin/LogGenerator-1.1-9.jar"
touch "$WORK/env/bin/LogGenerator-1.1-6.jar"
run_case 1 TC-16-DS
grep -q 'LogGenerator 1.1-9+ required' "$WORK/result"
echo "PASS: obsolete LogGenerator rejected before starting a benchmark"
