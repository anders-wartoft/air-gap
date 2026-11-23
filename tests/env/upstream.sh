#!/usr/bin/env bash
# sudo -A bash upstream.sh insamling-01-andwar-insamling.lab5.dev.lia.vmint.ctr:9093 /etc/kafka/client.properties export
set -euo pipefail
umask 077

if (( $# < 2 || $# > 3 )); then
    echo "Usage: $0 BROKER CLIENT_PROPERTIES [TOPIC (default: export)]" >&2
    exit 2
fi

broker=$1
config=$2
topic=${3:-export}
log=${UPSTREAM_LOG:-/var/log/stream/upstream-u1.log}
producer=${KAFKA_PRODUCER_PERF_TEST:-kafka-producer-perf-test}
throughput=${THROUGHPUT:-60000}
records=${NUM_RECORDS:-1800000}
record_size=${RECORD_SIZE:-600}

if [[ ! -r $config || ! -r $log ]] || ! command -v "$producer" >/dev/null; then
    echo "Need a readable Kafka client config, readable upstream log ($log), and $producer on PATH." >&2
    exit 2
fi

report=$(mktemp "./upstream-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
log_start=$(date '+%Y/%m/%d %H:%M:%S')
printf 'Upstream start (UTC): %s\nTopic: %s\nRequested records: %s\nRate: %s records/s\nRecord size: %s bytes\n' \
    "$(date -u '+%FT%TZ')" "$topic" "$records" "$throughput" "$record_size" > "$report"
echo "Writing $report"
{
    echo
    echo '=== Latest upstream startup settings ==='
    awk '/Upstream version:/ { version=$0 } /UDP pacing:/ { pacing=$0 }
         /sendingThread\[0\]:/ { threads="" }
         /sendingThread\[[0-9]+\]:/ { threads=threads $0 "\n" }
         END { if (version != "") print version; if (pacing != "") print pacing; printf "%s", threads }' "$log"
    echo
    echo '=== Kafka producer ==='
} >> "$report"

status=0
"$producer" --bootstrap-server "$broker" --command-config "$config" --topic "$topic" \
    --throughput "$throughput" --num-records "$records" --record-size "$record_size" 2>&1 |
    tee -a "$report" || status=$?
printf 'Producer exit status: %s\nProducer finished (UTC): %s\n' \
    "$status" "$(date -u '+%FT%TZ')" >> "$report"

if ! read -r -p 'After the delayed sending threads drain and the next STATISTICS line is logged, press Enter to finish: '; then
    echo 'No confirmation that the delayed threads drained.' | tee -a "$report" >&2
    if (( status == 0 )); then status=1; fi
fi
{
    echo
    printf '=== Upstream log captured at %s (UTC) ===\n' "$(date -u '+%FT%TZ')"
    awk -v start="$log_start" '
        /STATISTICS:/ && substr($0, 1, 19) < start { before=$0 }
        substr($0, 1, 19) >= start {
            if ($0 ~ /STATISTICS:/) stats[++n]=$0
            if ($0 ~ /closed dead subscription/) closed++
            if ($0 ~ /abandoned subscription/) abandoned++
            if ($0 ~ /Delayed Kafka buffer limit reached/) overflow++
            if ($0 ~ /Kafka status changed to error:/ && ++errors <= 10) print
        }
        END {
            if (before != "") print before
            for (i=1; i<=n; i++) print stats[i]
            printf "In-window log counts: closed=%d abandoned=%d delayed_buffer_overflow=%d Kafka_status_errors=%d\n", closed, abandoned, overflow, errors
            if (!n) { print "No in-window upstream STATISTICS found; check log path and rotation."; exit 1 }
        }' "$log"
} >> "$report" || { if (( status == 0 )); then status=1; fi; }
echo "Report: $report"
exit "$status"
