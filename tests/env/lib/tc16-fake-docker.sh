#!/usr/bin/env bash
# Deterministic Docker boundary for the orchestration regression tests.
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_STATE/calls"
if [[ "$1" == inspect ]]; then
    case "$3" in
        *Status*) [[ "$4" != lg-producer ]] || { echo exited; exit; }; echo running ;;
        *ExitCode*) echo 0 ;;
        *OOMKilled*) echo false ;;
    esac
    exit
fi
[[ "$1" == compose ]] || exit 1
side="${3##*-}"
shift 5
command="$1"; shift
service="${*: -1}"
case "$command" in
    ps)
        if [[ "$1" == -aq ]]; then
            echo "$2"
        elif [[ "${FAKE_ALREADY_RUNNING:-0}" == 1 ]]; then
            echo downstream
        fi
        ;;
    up)
        touch "$FAKE_STATE/$service"
        ;;
    stop|down) ;;
    logs)
        if [[ "$service" == downstream ]]; then
            echo "UDP listener starting"
        elif [[ "$service" == lg-sink ]]; then
            echo "Number of unique received numbers: $TC16_COUNT"
            echo "Duplicate detection found: ${FAKE_DUPLICATES:-0}"
            echo "Next expected number: $((TC16_COUNT+1))"
        fi
        ;;
    exec)
        tool="$5"
        case "$tool" in
            kafka-broker-api-versions)
                printf '127.0.0.1 (id: 1 rack: null)\n127.0.0.1 (id: 2 rack: null)\n'
                ;;
            kafka-topics) ;;
            kafka-get-offsets)
                count=0
                if [[ "$side" == us && -f "$FAKE_STATE/lg-producer" ]] ||
                   [[ "$side" == ds && ( -f "$FAKE_STATE/upstream" ||
                       ( "${FAKE_REMOTE:-0}" == 1 && -f "$FAKE_STATE/downstream" ) ) ]]; then
                    count="$TC16_COUNT"
                    if [[ "$side" == ds && "${FAKE_DROP:-0}" == 1 ]]; then count=$((count-1)); fi
                    if [[ "$side" == ds && "${FAKE_EXTRA:-0}" == 1 ]]; then count=$((count+1)); fi
                fi
                printf '%s:0:%s\n' "$7" "$count"
                ;;
            kafka-consumer-groups)
                group="$8"
                topic=tc16-transfer
                [[ "$group" != tc16-verifier* ]] || topic=tc16-dedup
                printf '%s %s 0 %s %s 0 consumer host client\n' "$group" "$topic" "$TC16_COUNT" "$TC16_COUNT"
                ;;
            *) echo "Unexpected Kafka tool: $tool" >&2; exit 1 ;;
        esac
        ;;
    *) echo "Unexpected Compose command: $command" >&2; exit 1 ;;
esac
