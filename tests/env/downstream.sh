#!/usr/bin/env bash
# sudo -A bash downstream.sh analys-01-andwar-analys.lab5.dev.lia.vmint.ctr:9093 /etc/kafka/client.properties  export
set -euo pipefail
umask 077

if (( $# < 2 )); then
    echo "Usage: $0 BROKER CLIENT_PROPERTIES [TOPIC ... (default: export)]" >&2
    exit 2
fi

broker=$1
config=$2
shift 2
if (( $# == 0 )); then set -- export; fi
log=${DOWNSTREAM_LOG:-/var/log/stream/downstream-udp.log}
snmp=${SNMP_FILE:-/proc/net/snmp}
offsets=${KAFKA_GET_OFFSETS:-kafka-get-offsets}

if [[ ! -r $config || ! -r $log || ! -r $snmp ]] ||
    ! command -v "$offsets" >/dev/null || ! command -v timeout >/dev/null; then
    echo "Need a readable Kafka client config, downstream log ($log), UDP counters ($snmp), $offsets, and timeout." >&2
    exit 2
fi

report=$(mktemp "./downstream-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
log_start=$(date '+%Y/%m/%d %H:%M:%S')
status=0

snapshot() {
    local phase=$1 topic rc
    shift
    printf '\n=== %s (UTC): %s ===\n' "$phase" "$(date -u '+%FT%TZ')"
    if ! awk '/^Udp:/ { print; n++ } END { if (n != 2) exit 1 }' "$snmp"; then
        echo 'ERROR: Expected two UDP counter lines.'
        status=1
    fi
    for topic in "$@"; do
        printf '\n--- Kafka end offsets: %s (UTC: %s) ---\n' "$topic" "$(date -u '+%FT%TZ')"
        rc=0
        timeout 30s "$offsets" --bootstrap-server "$broker" --command-config "$config" --topic "$topic" || rc=$?
        if (( rc != 0 )); then
            printf 'ERROR: Kafka end-offset query failed for %s (exit %s).\n' "$topic" "$rc"
            status=1
        fi
    done
}

echo "Writing $report"
snapshot BEFORE "$@" >> "$report" 2>&1
if (( status != 0 )); then
    echo "Baseline capture was incomplete; see $report." >&2
fi
if ! read -r -p 'Baseline saved. Start upstream now. After the delayed copy drains and the next STATISTICS line is logged, press Enter: '; then
    echo 'No confirmation that the delayed threads drained.' | tee -a "$report" >&2
    status=1
fi
snapshot AFTER "$@" >> "$report" 2>&1
{
    echo
    echo '=== Downstream STATISTICS and Kafka errors ==='
    awk -v start="$log_start" '
        /STATISTICS:/ && substr($0, 1, 19) < start { before=$0 }
        substr($0, 1, 19) >= start && /STATISTICS:|Kafka status changed to error:|maximum request accumulated|Disk error when trying to access log file|connection refused|circuit breaker is open/ {
            if ($0 ~ /STATISTICS:/) { stats++; print; next }
            if (++errors <= 20) print
        }
        END {
            if (before != "") print "Last STATISTICS before capture: " before
            printf "Matching in-window Kafka log lines: %d (first 20 shown)\n", errors
            if (!stats) { print "No in-window downstream STATISTICS found; check log path and rotation."; exit 1 }
        }' "$log"
} >> "$report" || status=1
echo "Report: $report"
exit "$status"
