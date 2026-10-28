#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-verifier-test.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
mkdir "$WORK/bin"

cat >"$WORK/bin/docker" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
    inspect) exit 0 ;;
    logs)
        if [[ "${FAIL_LOGS:-0}" == 1 ]]; then
            echo "mock log read failure" >&2
            exit 1
        fi
        cat "$MOCK_LOG"
        ;;
    *) echo "Unexpected docker command: $*" >&2; exit 2 ;;
esac
MOCK
chmod +x "$WORK/bin/docker"
export PATH="$WORK/bin:$PATH" MOCK_LOG="$WORK/upstream.log"

expect_verifier() {
    local expected="$1" actual=0
    bash "$HERE/verify-tc20.sh" >"$WORK/result" 2>&1 || actual=$?
    if [[ "$actual" != "$expected" ]]; then
        cat "$WORK/result" >&2
        echo "Expected verifier status $expected, got $actual" >&2
        exit 1
    fi
}

# Large binary-containing logs used to trigger null-byte warnings and SIGPIPE.
{
    printf 'TCP connection unavailable\n'
    for (( i=0; i<10000; i++ )); do
        printf 'TCP connection unavailable retry=%s\0\n' "$i"
    done
    printf 'Transport status restored to running\n'
} >"$MOCK_LOG"
expect_verifier 0
grep -qF 'Chronological order' "$WORK/result"
if grep -qF 'ignored null byte' "$WORK/result"; then
    echo "Verifier stored binary logs in a shell variable" >&2
    exit 1
fi

printf 'Transport status restored to running\nTCP connection unavailable\n' >"$MOCK_LOG"
expect_verifier 1
printf 'TCP connection unavailable\n' >"$MOCK_LOG"
expect_verifier 1
printf 'Transport status restored to running\n' >"$MOCK_LOG"
expect_verifier 1
: >"$MOCK_LOG"
expect_verifier 2
export FAIL_LOGS=1
expect_verifier 2
grep -qF 'mock log read failure' "$WORK/result"
unset FAIL_LOGS

# Exercise the exact host-owned fixture setup without building or starting containers.
for script in tc30-memory-backpressure-test.sh tc39-properties-comments-test.sh; do
    fixture="$WORK/$script"
    mkdir "$fixture"
    sed -n '/^WORK=.*mktemp/,/^PROJECT=/{ /^WORK=/d; /^PROJECT=/d; p; }' "$HERE/$script" >"$WORK/setup.sh"
    (
        umask 077
        WORK="$fixture"
        source "$WORK/../setup.sh"
        test -d "$WORK/lost-events"
        if [[ "$script" == tc30-* ]]; then
            test -r "$WORK/sink.bytes"
            test "$(cat "$WORK/sink.bytes")" == 0
            test "$(find "$WORK/sink.bytes" -user "$(id -un)" -perm 0600 | wc -l | tr -d ' ')" == 1
        else
            for dir in server-trust client-trust; do
                test "$(find "$WORK/$dir" -type d -user "$(id -un)" -perm 0700 | wc -l | tr -d ' ')" == 1
            done
        fi
    )
done
echo "Verifier and host-owned fixture regressions: PASS"
