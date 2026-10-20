#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc16-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/env/lib" "$WORK/env/bin" "$WORK/mock" "$WORK/state"
cp "$HERE/tc16-performance.sh" "$HERE/run-testcase.sh" "$WORK/env/"
cp "$HERE/lib/report.sh" "$WORK/env/lib/"
cp "$HERE/lib/tc16-fake-docker.sh" "$WORK/mock/docker"
chmod +x "$WORK/mock/docker"
touch "$WORK/env/bin/LogGenerator-1.1-9.jar"
export PATH="$WORK/mock:$PATH" FAKE_STATE="$WORK/state"
export TC16_COUNT=10 TC16_PARTITIONS=1 TC16_SETTLE=1 TC16_TIMEOUT=3 TC16_POLL=1

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

run_case 0 16
grep -q 'TC-16 PASS' "$WORK/result"
before 'up .*lg-producer$' 'up .*upstream$'
before 'stop .*downstream$' 'up .*dedup$'
before 'stop .*upstream$' 'up .*dedup$'
before 'stop .*dedup$' 'up .*lg-sink$'
echo "PASS: single-machine phase ordering and full verification"

# Each mock run starts with new state; real runs require explicit reset.
rm "$FAKE_STATE/upstream" "$FAKE_STATE/lg-producer"
: > "$FAKE_STATE/calls"
export FAKE_DROP=1
run_case 1 16
grep -q 'transfer timed out' "$WORK/result"
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
unset FAKE_DUPLICATES
echo "PASS: extra arguments and duplicate verification fail explicitly"

: > "$FAKE_STATE/calls"
rm "$FAKE_STATE/upstream" "$FAKE_STATE/lg-producer"
run_case 0 TC-16-US
if grep -qE 'up .*downstream$|up .*dedup$|up .*lg-sink$|up .*kafka-ds' "$FAKE_STATE/calls"; then
    echo "US alias started downstream workloads" >&2; exit 1
fi
echo "PASS: US alias starts only its own services"

for service in upstream lg-producer downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
: > "$FAKE_STATE/calls"
export FAKE_REMOTE=1
run_case 0 TC-16-DS
if grep -qE 'up .*upstream$|up .*lg-producer$|up .*kafka-us' "$FAKE_STATE/calls"; then
    echo "DS alias started upstream workloads" >&2; exit 1
fi
echo "PASS: DS alias starts only its own services and defers dedup/verification"

for service in downstream dedup lg-sink; do rm -f "$FAKE_STATE/$service"; done
export FAKE_EXTRA=1
run_case 1 TC-16-DS
grep -q 'received 11 raw records, expected 10' "$WORK/result"
unset FAKE_REMOTE FAKE_EXTRA
echo "PASS: excess raw records fail immediately"

rm "$WORK/env/bin/LogGenerator-1.1-9.jar"
touch "$WORK/env/bin/LogGenerator-1.1-6.jar"
run_case 1 TC-16-DS
grep -q 'LogGenerator 1.1-9+ required' "$WORK/result"
echo "PASS: obsolete LogGenerator rejected before starting a benchmark"
