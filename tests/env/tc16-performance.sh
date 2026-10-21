#!/usr/bin/env bash
# Staged native-Linux UDP benchmark. No SSH or control traffic crosses the diode.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/report.sh"
cd "$HERE"
export COMPOSE_PROGRESS=plain

die() {
    echo "TC-16: $*" >&2
    if [[ -n "${FAILURE_FILE:-}" ]]; then printf '%s\n' "$*" > "$FAILURE_FILE"; fi
    exit 1
}
positive() { [[ "$2" =~ ^[1-9][0-9]*$ ]] || die "$1 must be a positive integer"; }

export TC16_COUNT="${TC16_COUNT:-1000000}"
export TC16_PARTITIONS="${TC16_PARTITIONS:-5}"
export TC16_TIMEOUT="${TC16_TIMEOUT:-1800}"
export TC16_POLL="${TC16_POLL:-1}"
export TC16_SETTLE="${TC16_SETTLE:-5}"
positive TC16_COUNT "$TC16_COUNT"
positive TC16_PARTITIONS "$TC16_PARTITIONS"
positive TC16_TIMEOUT "$TC16_TIMEOUT"
positive TC16_POLL "$TC16_POLL"
positive TC16_SETTLE "$TC16_SETTLE"
[[ "${TC16_EPS:-50000}" =~ ^(-1|[1-9][0-9]*)$ ]] || die "TC16_EPS must be -1 or a positive integer"

MODE="${1:-all}"
case "$MODE" in all|us|ds|verify|stop-us|reset-us|reset-ds) ;; *)
    die "usage: $0 {all|us|ds|verify|stop-us|reset-us|reset-ds}" ;;
esac
[[ $# -le 1 ]] || die "unexpected arguments"
RESULTS="${TC16_RESULTS_DIR:-$ROOT/tmp/tc16}"
mkdir -p "$RESULTS"
RESULTS="$(cd "$RESULTS" && pwd)"
FAILURE_FILE=""
case "$MODE" in
    reset-*|stop-us) ;;
    *)
        FAILURE_FILE="$(mktemp "$RESULTS/failure.XXXXXX")"
        : > "$RESULTS/phase-timing.csv"
        rm -f "$RESULTS/failure-summary.txt" "$RESULTS/success-summary.txt"
        if [[ "$MODE" == all || "$MODE" == ds ]]; then
            : > "$RESULTS/progress.csv"
        fi
        ;;
esac

compose() {
    local side="$1"; shift
    docker compose -p "airgap-tc16-$side" -f "$HERE/docker-compose.tc16.yml" "$@"
}
kafka() {
    local side="$1"; shift
    local port=39092
    [[ "$side" != us ]] || port=29092
    compose "$side" exec -T "kafka-$side-1" timeout 30s "$@" --bootstrap-server "127.0.0.1:$port"
}
now() { date +%s; }
metric() {
    local phase="$1" seconds="$2" count="${3:-$TC16_COUNT}"
    printf '%s,%s,%s\n' "$phase" "$count" "$seconds" >> "$RESULTS/phase-timing.csv"
    awk -v p="$phase" -v n="$count" -v s="$seconds" \
        'BEGIN { printf "%s: %d events, %ds wall time", p,n,s;
        if(s>0) printf ", %.2f events/s", n/s; print "" }' | tee -a "$RESULTS/timing.txt"
}
assert_running() {
    local side="$1" service="$2" cid status
    cid="$(compose "$side" ps -aq "$service")"
    [[ -n "$cid" ]] || { echo "TC-16: $service has no container" >&2; return 1; }
    status="$(docker inspect -f '{{.State.Status}}' "$cid")"
    [[ "$status" == running ]] || { echo "TC-16: $service is $status; inspect $RESULTS/$side.log" >&2; return 1; }
}
stat_total() {
    local file="$1" field="$2"
    awk -v field="$field" '
        /STATISTICS:/ {
            pattern="\"" field "\":[[:space:]]*[0-9]+"
            if (match($0,pattern)) {
                value=substr($0,RSTART,RLENGTH)
                sub(/.*:[[:space:]]*/,"",value)
                found=value
            }
        }
        END { print (found=="" ? "unavailable" : found) }
    ' "$file"
}
result_details() {
    local rc="$1"
    local phase epoch current end lag sent received written
    sent="$(stat_total "$RESULTS/us.log" total_sent)"
    received="$(stat_total "$RESULTS/ds.log" total_received)"
    written="$(stat_total "$RESULTS/ds.log" total_sent)"
    echo "─── testcase 16 verdict ───"
    if (( rc == 0 )); then
        if [[ "$MODE" == us ]]; then
            echo "  result : sender startup passed; delivery verification runs on DS"
        else
            echo "  result : all expected sequence IDs verified exactly once"
        fi
    elif [[ -s "$FAILURE_FILE" ]]; then
        printf '  reason : %s\n' "$(cat "$FAILURE_FILE")"
    else
        echo "  reason : command failed or run interrupted; inspect diagnostic logs"
    fi
    printf '  %-37s | %s\n' "Stage" "Events"
    printf '  %-37s | %s\n' "Expected" "$TC16_COUNT"
    printf '  %-37s | %s\n' "Upstream read (last statistics)" "$(stat_total "$RESULTS/us.log" total_received)"
    printf '  %-37s | %s\n' "Upstream sent (last statistics)" "$sent"
    printf '  %-37s | %s\n' "Downstream received (last statistics)" "$received"
    printf '  %-37s | %s\n' "Downstream written (last statistics)" "$written"
    printf '  %-37s | %10s | %10s | %12s\n' "Measured phase" "Events" "Elapsed" "Average EPS"
    awk -F, '{
        eps=($3>0 ? sprintf("%.2f",$2/$3) : "n/a (<1s)")
        printf "  %-37s | %10s | %9ss | %12s\n",$1,$2,$3,eps
    }' "$RESULTS/phase-timing.csv"
    printf '  Total runtime : %ss (includes setup, waiting, shutdown and cleanup)\n' "$SECONDS"
    echo "  timing : phase EPS includes startup/CLI polling; not steady-state throughput. Unreached phases have no timing."
    if [[ "$MODE" != us && -f "$RESULTS/progress.csv" ]]; then
        while IFS=, read -r phase epoch current end lag; do
            printf '  %s progress : current=%s end=%s lag=%s\n' "$phase" "$current" "$end" "$lag"
            if [[ "$phase" == transfer ]]; then
                awk -v n="$TC16_COUNT" -v c="$current" 'BEGIN {
                    if(c<=n) printf "  Missing raw Kafka records : %d (%.4f%%)\n",n-c,100*(n-c)/n;
                    else printf "  Extra raw Kafka records : %d\n",c-n
                }'
            fi
        done < <(awk -F, '{last[$1]=$0} END {for(p in last) print last[p]}' "$RESULTS/progress.csv")
    fi
    echo "  note : statistics are sampled and may lag; unavailable is not zero."
    echo "  note : UDP send success is not delivery confirmation; a shortfall alone does not identify the drop location."
    printf '  evidence : %s\n' "$RESULTS"
}
capture() {
    local rc=$?
    trap - EXIT
    echo "TC-16: Collecting diagnostic logs..."
    for side in us ds; do
        if ! compose "$side" logs --no-color > "$RESULTS/$side.log" 2>&1; then
            echo "TC-16: could not collect $side logs" >&2
            (( rc != 0 )) || rc=1
        fi
    done
    if [[ "${TC16_BATCH_MODE:-0}" == 1 && "$MODE" == all ]]; then
        echo "TC-16: Cleaning up TC-16 containers and Kafka volumes..."
        for side in us ds; do
            if ! compose "$side" down -v --remove-orphans; then
                echo "TC-16: failed to clean up $side stack" >&2
                (( rc != 0 )) || rc=1
            fi
        done
        echo "TC-16: exit=$rc; artifacts=$RESULTS; batch cleanup attempted for both TC-16 stacks"
    else
        echo "TC-16: exit=$rc; artifacts=$RESULTS; containers/data retained (explicit reset required)"
    fi
    local summary_file="$RESULTS/failure-summary.txt" color="$REPORT_RED"
    if (( rc == 0 )); then
        summary_file="$RESULTS/success-summary.txt"
        color="$REPORT_GREEN"
    fi
    result_details "$rc" | tee "$summary_file" | while IFS= read -r line; do
        # Keep the verdict marker uncolored for the batch summary extractor.
        if [[ "$line" == "  "* ]]; then
            printf '  %s%s%s\n' "$color" "${line#  }" "$REPORT_RESET"
        else
            printf '%s\n' "$line"
        fi
    done
    rm -f "$FAILURE_FILE"
    local label="TC-16 ($MODE)"
    if [[ "$MODE" == us ]]; then
        label="$label sender startup only; delivery verification runs on DS"
    fi
    report_summary "$label" "$rc" || true
    exit "$rc"
}
case "$MODE" in reset-*|stop-us) ;; *) trap capture EXIT ;; esac
trap 'exit 130' INT
trap 'exit 143' TERM

require_lg() {
    local jar version
    jar="$(printf '%s\n' "$HERE"/bin/LogGenerator-*.jar | sort -V | tail -1)"
    [[ -f "$jar" ]] || die "place LogGenerator-1.1-9.jar or newer in tests/env/bin/"
    version="$(basename "$jar")"
    [[ "$version" =~ ^LogGenerator-([0-9]+)\.([0-9]+)-([0-9]+)\.jar$ ]] ||
        die "unrecognized LogGenerator version: $version"
    (( BASH_REMATCH[1] > 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] > 1) ||
        (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] == 1 && BASH_REMATCH[3] >= 9) )) ||
        die "LogGenerator 1.1-9+ required for --from-beginning true; selected $version"
}
wait_brokers() {
    local side="$1" deadline=$((SECONDS + 180))
    until kafka "$side" kafka-broker-api-versions > "$RESULTS/$side-brokers.log" 2>&1 &&
        grep -qE '\(id: 1 ' "$RESULTS/$side-brokers.log" &&
        grep -qE '\(id: 2 ' "$RESULTS/$side-brokers.log"; do
        (( SECONDS < deadline )) || die "$side brokers not ready; see $RESULTS/$side-brokers.log"
        assert_running "$side" "kafka-$side-1"
        assert_running "$side" "kafka-$side-2"
        sleep 2
    done
}
offset_count() {
    local side="$1" topic="$2" output
    output="$(kafka "$side" kafka-get-offsets --topic "$topic" --time -1)" || return
    printf '%s\n' "$output" | awk -F: -v t="$topic" -v p="$TC16_PARTITIONS" '
        $1==t && $2~/^[0-9]+$/ && $3~/^[0-9]+$/ { n++; total+=$3 }
        END {
            if(n!=p) {
                printf "TC-16: expected %d partition offsets for %s, got %d\n",p,t,n > "/dev/stderr"
                exit 1
            }
            printf "%.0f\n",total
        }'
}
create_topic() {
    kafka "$1" kafka-topics --create --if-not-exists --topic "$2" \
        --partitions "$TC16_PARTITIONS" --replication-factor 2 \
        --config retention.ms=-1 --config cleanup.policy=delete
}
prepare() {
    local side="$1" services
    echo "TC-16: Starting $side Kafka cluster and preparing test topics..."
    services="$(compose "$side" ps --services --status running)"
    [[ -z "$services" ]] || die "$side stack is already running; for a fresh run execute: bash \"$HERE/tc16-performance.sh\" reset-$side (deletes this side's TC-16 Kafka data)"
    compose "$side" up -d --no-deps "kafka-$side-1" "kafka-$side-2"
    wait_brokers "$side"
    create_topic "$side" tc16-transfer
    [[ "$(offset_count "$side" tc16-transfer)" == 0 ]] || die "$side input topic is not empty; reset-$side first"
    if [[ "$side" == ds ]]; then
        create_topic ds tc16-dedup
        create_topic ds tc16-gaps
        # Downstream writes diagnostics here; do not auto-create arbitrary topics.
        create_topic ds airgap-internal
        [[ "$(offset_count ds tc16-dedup)" == 0 ]] || die "dedup topic is not empty; reset-ds first"
        echo "TC-16: Starting downstream UDP receiver..."
        compose ds up -d --build --no-deps downstream
        local deadline=$((SECONDS + 120))
        until compose ds logs --no-color downstream | grep -q 'UDP listener starting'; do
            (( SECONDS < deadline )) || die "downstream UDP listener not ready"
            assert_running ds downstream
            sleep 1
        done
        echo "TC-16-DS READY: UDP/1234; waiting for $TC16_COUNT events. Start TC-16-US now."
        if [[ "$MODE" == ds ]]; then
            echo "TC-16-DS does not generate or send events. Leave this terminal running and start TC-16-US on machine 1."
            echo "For an automatic single-machine run, reset both TC-16 sides first, then run: ./run-testcase.sh 16"
        fi
    fi
}
wait_job() {
    local side="$1" service="$2" deadline=$((SECONDS + TC16_TIMEOUT)) cid state code
    cid="$(compose "$side" ps -aq "$service")"
    [[ -n "$cid" ]] || die "$service container missing"
    while :; do
        state="$(docker inspect -f '{{.State.Status}}' "$cid")"
        if [[ "$state" == exited ]]; then
            compose "$side" logs --no-color "$service" > "$RESULTS/$service.log"
            code="$(docker inspect -f '{{.State.ExitCode}}' "$cid")"
            [[ "$code" == 0 ]] || die "$service exited $code; see $RESULTS/$service.log"
            return
        fi
        [[ "$state" == running || "$state" == created ]] || die "$service is $state"
        (( SECONDS < deadline )) || die "$service timed out; container left for diagnosis"
        sleep "$TC16_POLL"
    done
}
generate() {
    local started
    echo "TC-16: Adding $TC16_COUNT test events to upstream Kafka with LogGenerator (sender not running)..."
    started="$(now)"
    compose us up -d --no-deps lg-producer
    wait_job us lg-producer
    [[ "$(offset_count us tc16-transfer)" == "$TC16_COUNT" ]] || die "prefill count differs from TC16_COUNT"
    metric generation "$(( $(now) - started ))"
    echo "TC-16-US: generation finished; starting UDP sender (no generator remains running)"
}
send_events() {
    echo "TC-16: Sending data to downstream over UDP; starting upstream sender..."
    compose us up -d --build --no-deps upstream || return
    sleep 1
    assert_running us upstream
}

# GetOffsetShell is suitable only for the nontransactional raw topic.
# Kafka Streams output offsets include transaction markers, so dedup completion
# is measured with its committed INPUT offsets instead.
group_progress() {
    local group="$1" topic="$2" output
    output="$(kafka ds kafka-consumer-groups --describe --group "$group")" || return
    printf '%s\n' "$output" | awk -v g="$group" -v t="$topic" -v p="$TC16_PARTITIONS" '
        $1==g && $2==t && $3~/^[0-9]+$/ && $4~/^[0-9]+$/ && $5~/^[0-9]+$/ && $6~/^[0-9]+$/ {
            rows++; current+=$4; end+=$5; lag+=$6
        }
        END { if(rows!=p) print "0 0 1"; else printf "%.0f %.0f %.0f\n",current,end,lag }'
}
wait_phase() {
    local phase="$1" service="$2" group="${3:-}" topic="${4:-tc16-transfer}"
    local deadline=$((SECONDS + TC16_TIMEOUT)) first=0 previous last_zero started count end lag sample
    started="$(now)"; previous="$started"; last_zero="$started"
    while :; do
        assert_running ds "$service"
        if [[ -z "$group" ]]; then
            count="$(offset_count ds tc16-transfer)"; end="$TC16_COUNT"; lag=$((TC16_COUNT - count))
            (( count <= TC16_COUNT )) || die "received $count raw records, expected $TC16_COUNT"
        else
            sample="$(group_progress "$group" "$topic")"
            read -r count end lag <<< "$sample"
        fi
        local sampled
        sampled="$(now)"
        if (( count > 0 && first == 0 )); then
            first="$sampled"
            echo "$phase: first progress observed at epoch=$first"
        elif (( count == 0 )); then
            # Use the beginning of the query, not its completion, as a
            # conservative lower bracket for the first append/commit.
            last_zero="$previous"
        fi
        printf '%s,%s,%s,%s,%s\n' "$phase" "$sampled" "$count" "$end" "$lag" >> "$RESULTS/progress.csv"
        if (( count > 0 && lag == 0 && count == end )); then
            if [[ "$phase" == dedup ]]; then
                (( count == TC16_COUNT )) || die "dedup committed $count input records, expected $TC16_COUNT"
            fi
            metric "$phase (cold/armed wall clock)" "$((sampled - started))"
            echo "$phase: first-to-complete observed=$((sampled-first))s; conservative active upper bound=$((sampled-last_zero+1))s (1s clock resolution; includes CLI polling)" |
                tee -a "$RESULTS/timing.txt"
            return
        fi
        if (( SECONDS >= deadline )); then
            metric "$phase (incomplete/armed wall clock)" "$((sampled - started))" "$count"
            if [[ "$phase" == transfer && "$count" == 0 ]]; then
                echo "TC-16: no raw Kafka records arrived. Confirm TC-16-US finished generation and started upstream; DS alone produces no traffic." >&2
                echo "TC-16: if upstream is running, check its logs, target IP, UDP/1234 firewall and route; Docker Desktop is not native Linux host networking." >&2
            fi
            die "$phase timed out: current=$count end=$end lag=$lag; no success inferred from an idle topic"
        fi
        previous="$sampled"
        sleep "$TC16_POLL"
    done
}
verify() {
    local running
    running="$(compose ds ps --services --status running)"
    if printf '%s\n' "$running" | grep -qE '^(downstream|dedup)$'; then
        die "stop downstream and dedup before isolated verification"
    fi
    [[ "$(offset_count ds tc16-transfer)" == "$TC16_COUNT" ]] || die "raw input count differs from TC16_COUNT"
    echo "TC-16: Checking received data with LogGenerator: expecting $TC16_COUNT unique sequence IDs and no duplicates..."
    # A fresh group prevents an earlier verification's committed end offsets
    # from looking like completion before this reader has consumed anything.
    export TC16_VERIFY_GROUP="tc16-verifier-$(now)-$$"
    local started
    started="$(now)"
    compose ds up -d --force-recreate --no-deps lg-sink
    wait_phase verification lg-sink "$TC16_VERIFY_GROUP" tc16-dedup
    compose ds stop -t 30 lg-sink
    compose ds logs --no-color lg-sink > "$RESULTS/lg-sink.log"
    local cid code oom
    cid="$(compose ds ps -aq lg-sink)"
    code="$(docker inspect -f '{{.State.ExitCode}}' "$cid")"
    oom="$(docker inspect -f '{{.State.OOMKilled}}' "$cid")"
    [[ ( "$code" == 0 || "$code" == 143 ) && "$oom" == false ]] ||
        die "verifier did not stop cleanly: exit=$code OOMKilled=$oom"
    local unique duplicates next
    unique="$(sed -nE 's/.*Number of unique received numbers: ([0-9]+).*/\1/p' "$RESULTS/lg-sink.log" | tail -1)"
    duplicates="$(sed -nE 's/.*Duplicate detection found: ([0-9]+).*/\1/p' "$RESULTS/lg-sink.log" | tail -1)"
    next="$(sed -nE 's/.*Next expected number: ([0-9]+).*/\1/p' "$RESULTS/lg-sink.log" | tail -1)"
    local check_rc=0
    [[ "$unique" == "$TC16_COUNT" ]] || check_rc=1
    report_check "Unique delivered sequence IDs" "$TC16_COUNT" "${unique:-missing summary}" "$check_rc" || true
    check_rc=0
    [[ "$duplicates" == 0 ]] || check_rc=1
    report_check "Duplicate sequence IDs" "0" "${duplicates:-missing summary}" "$check_rc" || true
    check_rc=0
    [[ "$next" == "$((TC16_COUNT+1))" ]] || check_rc=1
    report_check "Next expected sequence ID" "$((TC16_COUNT+1))" "${next:-missing summary}" "$check_rc" || true
    [[ "$unique" == "$TC16_COUNT" && "$duplicates" == 0 && "$next" == "$((TC16_COUNT+1))" ]] ||
        die "verification failed: unique=$unique duplicates=$duplicates next=$next; expected $TC16_COUNT,0,$((TC16_COUNT+1)); see lg-sink.log"
    metric "verification (including shutdown)" "$(( $(now) - started ))"
    echo "TC-16 PASS: all $TC16_COUNT sequence IDs delivered exactly once; no fixed EPS threshold."
}
receive_events() {
    echo "TC-16: Waiting for downstream Kafka to store all $TC16_COUNT events..."
    wait_phase transfer downstream
    echo "TC-16: All raw events stored; allowing ${TC16_SETTLE}s to check for late or extra events..."
    sleep "$TC16_SETTLE"
    compose ds stop -t 30 downstream
    [[ "$(offset_count ds tc16-transfer)" == "$TC16_COUNT" ]] || die "raw record count changed during settle/shutdown"
    if [[ "$MODE" == all ]]; then compose us stop -t 30 upstream; fi
    echo "TC-16-DS: raw backlog stored; receiver stopped; starting dedup"
    echo "TC-16: Deduplicating data: reading $TC16_COUNT raw events and writing deduplicated output..."
    # Fresh container also guarantees a fresh local Streams state directory.
    compose ds up -d --force-recreate --no-deps dedup
    wait_phase dedup dedup tc16-dedup
    compose ds stop -t 30 dedup
    echo "TC-16-DS: dedup stopped; starting isolated verification"
    verify
}

case "$MODE" in
    reset-us|reset-ds)
        compose "${MODE#reset-}" down -v
        ;;
    stop-us) compose us stop -t 30 upstream ;;
    verify) require_lg; verify ;;
    us)
        require_lg
        prepare us
        generate
        send_events
        echo "TC-16-US: sender running; leave it up until DS completes, then run $0 stop-us"
        ;;
    ds)
        require_lg
        : > "$RESULTS/timing.txt"
        : > "$RESULTS/progress.csv"
        prepare ds
        receive_events
        ;;
    all)
        require_lg
        if [[ "${TC16_BATCH_MODE:-0}" == 1 ]]; then
            echo "TC-16: batch mode resets only TC-16 containers and Kafka volumes before the run"
            compose us down -v --remove-orphans
            compose ds down -v --remove-orphans
        fi
        : > "$RESULTS/timing.txt"
        : > "$RESULTS/progress.csv"
        prepare ds
        prepare us
        generate
        # The monitor must be armed before sender startup, including on loopback.
        receive_events &
        monitor=$!
        trap 'kill "$monitor" 2>/dev/null || true; exit 130' INT TERM
        if ! send_events; then
            kill "$monitor"
            wait "$monitor" || true
            die "could not start sender"
        fi
        rc=0
        wait "$monitor" || rc=$?
        compose us stop -t 30 upstream
        exit "$rc"
        ;;
esac
