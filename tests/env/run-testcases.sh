#!/usr/bin/env bash
# Run a selection of testcases given as a compact id spec.
#
# Usage:
#   ./run-testcases.sh [-l|--long] [<spec>] [-- extra args forwarded to run-testcase.sh]
#
#   <spec> is a comma-separated list where each token is one of:
#       N        single testcase id
#       N-M      inclusive range from N to M
#       N-       inclusive range from N to the highest available id (the
#                trailing '*' is optional: 'N-' and 'N-*' are equivalent)
#       N-*      same as 'N-'
#       -M       inclusive range from the lowest available id to M (the
#                leading '*' is optional: '-M' and '*-M' are equivalent)
#       *-M      same as '-M'
#       *        every available testcase
#
#   <spec> is OPTIONAL: with no spec at all (just `./run-testcases.sh`, or
#   `./run-testcases.sh -l`), it defaults to '1-' — every available
#   testcase from 1 through the highest id.
#
#   Spaces are allowed around commas and dashes. Order of ids in the spec
#   is preserved; duplicates are dropped on first occurrence.
#
#   -l | --long   also include the full verdict block from each testcase in
#                 the final summary (not only PASS/FAIL).
#
#   Each testcase's wall-clock runtime is shown inline (e.g. "TC-9 OK
#   (12s)") and in the chain summary; the total chain runtime is printed
#   on its own line at the very end ("total runtime: Xm YYs (Zs)") — handy
#   for comparing throughput across different machines/hosts.
#
# Examples:
#   ./run-testcases.sh
#   ./run-testcases.sh 1,4-7,9,11,12-
#   ./run-testcases.sh --long 9,11
#   ./run-testcases.sh '3, 5-8, 11' -- -pause
#   ./run-testcases.sh -l 5-
#
# After every testcase this script runs the compose teardown for all known
# profiles so the next one starts clean (same as run-all-testcases.sh).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

usage() { sed -n '2,42p' "$0"; exit "${1:-1}"; }

LONG=0
POSITIONAL=()
EXTRA_ARGS=()
while (( $# > 0 )); do
    case "$1" in
        -h|--help)   usage 0 ;;
        -l|--long)   LONG=1; shift ;;
        --)          shift; EXTRA_ARGS=("$@"); break ;;
        -*)
            echo "run-testcases: unknown option '$1'" >&2
            exit 2
            ;;
        *)           POSITIONAL+=("$1"); shift ;;
    esac
done

# No <spec> given at all (e.g. `./run-testcases.sh` or `./run-testcases.sh
# -l`) defaults to '1-' — every available testcase, same as if the user
# had typed the full range explicitly.
if (( ${#POSITIONAL[@]} == 0 )); then
    POSITIONAL=("1-")
fi
if (( ${#POSITIONAL[@]} > 1 )); then
    echo "run-testcases: extra positional args: ${POSITIONAL[*]:1}" >&2
    echo "run-testcases: (did you forget '--' before args forwarded to run-testcase.sh?)" >&2
    exit 2
fi
SPEC="${POSITIONAL[0]}"

# Discover available testcase ids from testcases/NN.env filenames.
available_ids=()
while IFS= read -r f; do
    bn=$(basename "$f" .env)
    available_ids+=("$((10#$bn))")
done < <(ls testcases/[0-9][0-9].env 2>/dev/null | sort)

if (( ${#available_ids[@]} == 0 )); then
    echo "run-testcases: no testcases/NN.env files found" >&2
    exit 2
fi

MIN_ID="${available_ids[0]}"
MAX_ID="${available_ids[$((${#available_ids[@]} - 1))]}"

is_available() {
    local needle="$1" id
    for id in "${available_ids[@]}"; do
        [[ "$id" == "$needle" ]] && return 0
    done
    return 1
}

# Resolve one endpoint of a range. '*' means min or max depending on side.
resolve_endpoint() {
    local tok="$1" side="$2"
    if [[ "$tok" == "*" ]]; then
        case "$side" in
            lo) echo "$MIN_ID" ;;
            hi) echo "$MAX_ID" ;;
        esac
        return 0
    fi
    if ! [[ "$tok" =~ ^[0-9]+$ ]]; then
        echo "run-testcases: invalid id '$tok' in spec" >&2
        exit 2
    fi
    echo "$((10#$tok))"
}

# Expand the spec into an ordered, deduplicated list of ids.
IDS=()
seen=""
IFS=',' read -r -a tokens <<< "$SPEC"
for raw in "${tokens[@]}"; do
    tok="${raw// /}"
    [[ -z "$tok" ]] && continue
    if [[ "$tok" == "*" ]]; then
        lo="$MIN_ID"; hi="$MAX_ID"
    elif [[ "$tok" == *-* ]]; then
        lhs="${tok%%-*}"; rhs="${tok##*-}"
        # The '*' on either side of a range is optional: 'N-' means the
        # same as 'N-*' (to the highest id), and '-M' means the same as
        # '*-M' (from the lowest id) — an empty endpoint defaults to '*'.
        [[ -z "$lhs" ]] && lhs="*"
        [[ -z "$rhs" ]] && rhs="*"
        lo="$(resolve_endpoint "$lhs" lo)"
        hi="$(resolve_endpoint "$rhs" hi)"
        if (( lo > hi )); then
            echo "run-testcases: empty range '$tok' (lo=$lo > hi=$hi)" >&2
            exit 2
        fi
    else
        lo="$(resolve_endpoint "$tok" lo)"
        hi="$lo"
    fi
    for (( id = lo; id <= hi; id++ )); do
        if ! is_available "$id"; then
            continue
        fi
        if [[ ",${seen}," != *",${id},"* ]]; then
            IDS+=("$id")
            seen+="${id},"
        fi
    done
done

if (( ${#IDS[@]} == 0 )); then
    echo "run-testcases: spec '$SPEC' selected no available testcases" >&2
    echo "run-testcases: available ids: ${available_ids[*]}" >&2
    exit 2
fi

printf '─── chain (%d testcase(s)): %s ───\n' "${#IDS[@]}" "${IDS[*]}"
if (( LONG )); then
    printf '─── long summary enabled (per-testcase verdict blocks will be reprinted) ───\n'
fi
if (( ${#EXTRA_ARGS[@]} )); then
    printf '─── forwarding to run-testcase.sh: %s ───\n' "${EXTRA_ARGS[*]}"
fi

PROFILE_FLAGS=(
    --profile dual --profile second-cluster
    --profile dedup --profile create --profile resend
    --profile lg-producer --profile lg-sink
)

# Per-run artifacts: one log file per testcase so we can reprint the verdict
# block at the end. Cleaned up on exit (success or failure).
LOGDIR="$(mktemp -d -t airgap-chain.XXXXXX)"
cleanup() { rm -rf "$LOGDIR"; }
trap cleanup EXIT

# Did the caller forward -pause? If so we leave the stack up after the final
# testcase (same semantics run-testcase.sh gives for a single run).
pause_forwarded=0
for a in ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}; do
    case "$a" in -pause|--pause|-p) pause_forwarded=1 ;; esac
done

# Status map: STATUS_<id> = PASS | FAIL | SKIP
PASS=()
FAIL=()
SKIP=()
declare_status() {
    local id="$1" verdict="$2"
    eval "STATUS_${id}=\"${verdict}\""
}
read_status() {
    local id="$1" var="STATUS_${id}"
    printf '%s' "${!var:-?}"
}

# Duration map: DURATION_<id> = elapsed seconds for that testcase. Same
# eval-based pattern as STATUS_<id> above (not an associative array —
# macOS ships bash 3.2 by default, which doesn't have them).
declare_duration() {
    local id="$1" seconds="$2"
    eval "DURATION_${id}=\"${seconds}\""
}
read_duration() {
    local id="$1" var="DURATION_${id}"
    printf '%s' "${!var:-0}"
}

# Format a whole-second duration as e.g. "3m07s" or "42s" — used for both
# the per-testcase and total chain runtimes so numbers are easy to read
# and compare across machines/runs at a glance.
format_duration() {
    local total="$1" m s
    m=$(( total / 60 ))
    s=$(( total % 60 ))
    if (( m > 0 )); then
        printf '%dm%02ds' "$m" "$s"
    else
        printf '%ds' "$s"
    fi
}

CHAIN_START_EPOCH="$(date +%s)"

# A testcase is chain-safe only if its manifest declares LG_PRODUCER_CONFIG
# OR AUTO_EXIT_SERVICE — either tells run-testcase.sh to orchestrate on a
# specific container and tear down when that container exits. Without one
# of them the runner falls through to `exec docker compose up` (foreground,
# no termination), which would hang the chain. Returns 0 (true) if TC-$id
# can auto-exit, 1 (false) otherwise.
tc_has_auto_exit() {
    local id="$1"
    local manifest="testcases/$(printf '%02d' "$id").env"
    [[ -f "$manifest" ]] || return 1
    # Source the manifest in a subshell so its variables don't leak into
    # the chain's own environment.
    (
        set +u
        # shellcheck disable=SC1090
        . "$manifest"
        [[ -n "${LG_PRODUCER_CONFIG-}" ]] || [[ -n "${AUTO_EXIT_SERVICE-}" ]]
    )
}

for id in "${IDS[@]}"; do
    echo
    echo "========================================================"
    echo "  running testcase $id"
    echo "========================================================"
    if ! tc_has_auto_exit "$id"; then
        echo "runner: testcase $id has no LG_PRODUCER_CONFIG and no AUTO_EXIT_SERVICE — would hang the chain." >&2
        echo "runner: skipping. Run it interactively with ./run-testcase.sh $id." >&2
        SKIP+=("$id")
        declare_status "$id" SKIP
        declare_duration "$id" 0
        continue
    fi
    LOG="$LOGDIR/tc-${id}.log"
    tc_start_epoch="$(date +%s)"
    set +e
    if (( ${#EXTRA_ARGS[@]} )); then
        ./run-testcase.sh "$id" "${EXTRA_ARGS[@]}" 2>&1 | tee "$LOG"
    else
        ./run-testcase.sh "$id" 2>&1 | tee "$LOG"
    fi
    rc="${PIPESTATUS[0]}"
    set -e
    tc_elapsed=$(( $(date +%s) - tc_start_epoch ))
    declare_duration "$id" "$tc_elapsed"
    if (( rc == 0 )); then
        echo "[chain] TC-$id OK ($(format_duration "$tc_elapsed"))"
        PASS+=("$id")
        declare_status "$id" PASS
    else
        echo "[chain] TC-$id FAILED (exit $rc) ($(format_duration "$tc_elapsed"))" >&2
        FAIL+=("$id")
        declare_status "$id" FAIL
    fi
    # Belt-and-braces teardown: run-testcase.sh already tears down on exit
    # unless -pause was requested, but if the user forwarded -pause we skip
    # this so the stack remains for inspection.
    if (( pause_forwarded == 0 )); then
        docker compose "${PROFILE_FLAGS[@]}" down -v --remove-orphans \
            >/dev/null 2>&1 || true
    fi
done

# Extract the verdict block for one testcase from its captured log.
# The block starts with "─── testcase <id> verdict ───" and continues while
# subsequent lines are indented with two spaces (the "  key : value" rows
# printed by run-testcase.sh). Writes nothing if no verdict was produced
# (e.g. the testcase aborted before the lg-producer / lg-sink summary).
print_verdict_block() {
    local id="$1" log="$2"
    awk -v id="$id" '
        show == 1 {
            if ($0 ~ /^  /) { print; next }
            exit
        }
        $0 ~ ("^─── testcase " id " verdict ───$") { print; show = 1; next }
    ' "$log"
}

echo
echo "─── chain summary ───"
for id in "${IDS[@]}"; do
    status="$(read_status "$id")"
    duration="$(read_duration "$id")"
    case "$status" in
        PASS) emoji="✅" ;;
        FAIL) emoji="❌" ;;
        SKIP) emoji="🤔" ;;
        *)    emoji="❔" ;;
    esac
    if [[ "$status" == "SKIP" ]]; then
        echo "${emoji} testcase ${id} ${status}"
    else
        echo "${emoji} testcase ${id} ${status} ($(format_duration "$duration"))"
    fi
    # Verdict block is always shown for failures (so the user can see *why*
    # it failed without re-running) and additionally shown for successes
    # when -l/--long was requested. SKIP has no captured log to extract.
    if [[ "$status" == "SKIP" ]]; then
        echo "  (no LG_PRODUCER_CONFIG in testcases/$(printf '%02d' "$id").env — chain-unsafe)"
        continue
    fi
    if (( LONG )) || [[ "$status" == "FAIL" ]]; then
        block="$(print_verdict_block "$id" "$LOGDIR/tc-${id}.log")"
        if [[ -n "$block" ]]; then
            printf '%s\n' "$block"
        else
            echo "  (no verdict block captured — see run-testcase.sh output above)"
        fi
        echo
    fi
done

echo
echo "  passed  (${#PASS[@]}): ${PASS[*]:-(none)}"
echo "  failed  (${#FAIL[@]}): ${FAIL[*]:-(none)}"
echo "  skipped (${#SKIP[@]}): ${SKIP[*]:-(none)}"
CHAIN_ELAPSED=$(( $(date +%s) - CHAIN_START_EPOCH ))
echo "  total runtime: $(format_duration "$CHAIN_ELAPSED") (${CHAIN_ELAPSED}s)"
if (( ${#FAIL[@]} )); then
    exit 1
fi
echo "chain OK"
