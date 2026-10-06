#!/usr/bin/env bash
# TC-12 verifier — prove each upstream chain delivered ~half the load.
#
# With deliverFilter=1,3,5 on 12a and deliverFilter=2,4,6 on 12b, each
# chain forwards a *disjoint* subset of Kafka offsets (odd vs even,
# with offset 0 landing on the even chain after the posMod fix in
# src/filter/filter.go). Two authoritative measurements prove this:
#
#   1. Each upstream app emits a periodic STATISTICS JSON line at
#      INFO (enabled by logStatistics=5 in TC-12 upstream configs).
#      The final "total_sent" field is the number of events that
#      chain actually forwarded over UDP after filtering.
#   2. kafka-downstream[transfer-12a] record count — must be ≈
#      LG_LIMIT (NOT 2× LG_LIMIT) because the two chains' outputs
#      are disjoint.
#
# Why no consumer-group check? kafka-consumer-groups --describe is
# brittle at teardown — the upstream containers have exited by the
# time VERIFY_SCRIPT runs, their group members are marked inactive,
# and the describe output format shifts between Kafka versions. The
# two measurements above are more direct and don't depend on broker
# state introspection.
#
# Env (set by run-testcase.sh):
#   TC_ID, PROJECT_NAME, CONTAINER_NAME, SINK_CID, VERIFY_RC_IN

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
LG_LIMIT="${TC12_LG_LIMIT:-100}"
DOWN_BROKER="${PROJECT}-kafka-downstream-1"
DOWN_TOPIC="${TC12_DOWNSTREAM_TOPIC:-transfer-12a}"

echo "─── verify-tc12: 50/50 load share across both chains? ───"

rc=0

# ── 1. Per-chain forwarded count from each upstream's STATISTICS log ──
#
# upstream.go periodically marshals a stats map to JSON and logs it at
# INFO as `STATISTICS: {"id":"Upstream_12a",...,"total_sent":NN,...}`.
# The last STATISTICS line before the container exits reflects the
# chain's final forwarded count — events that passed the deliverFilter
# AND were sent on the wire. Ideal per chain: LG_LIMIT/2 (50 for 100).
min_half=$(( LG_LIMIT * 2 / 5 ))        # 40 for LG_LIMIT=100 — allow 20% slack
max_half=$(( LG_LIMIT * 3 / 5 ))        # 60 for LG_LIMIT=100 — allow 20% slack
for ab in a b; do
    ctr="${PROJECT}-upstream-${ab}-1"
    if ! docker inspect "$ctr" >/dev/null 2>&1; then
        report_check "Container ${ctr}" "running" "NOT running — chain never started" 1 || rc=1
        continue
    fi
    # Last STATISTICS line + extract "total_sent":NN with sed/awk (no jq
    # inside the stock airgap image). Fall back to docker logs if the
    # file log isn't readable.
    stats_line="$(docker exec "$ctr" sh -c '
        for p in /tmp/upstream.log; do
            [ -f "$p" ] || continue
            grep "STATISTICS:" "$p" | tail -1
            exit 0
        done
        echo ""
    ' 2>/dev/null)"
    if [[ -z "$stats_line" ]]; then
        # Fallback — grep the container's stdout/stderr via docker logs.
        stats_line="$(docker logs "$ctr" 2>&1 | grep "STATISTICS:" | tail -1 || true)"
    fi
    if [[ -z "$stats_line" ]]; then
        report_check "upstream-${ab} STATISTICS line" "found" "NOT found (logStatistics not enabled?)" 1 || rc=1
        continue
    fi
    total_sent="$(printf '%s\n' "$stats_line" \
                    | sed -n 's/.*"total_sent":\([0-9]*\).*/\1/p' \
                    | head -1)"
    total_sent="${total_sent:-0}"
    if (( total_sent >= min_half && total_sent <= max_half )); then
        report_check "upstream-${ab} total_sent" "${min_half}..${max_half} (≈half of ${LG_LIMIT})" "${total_sent}" 0 || rc=1
    elif (( total_sent < min_half )); then
        report_check "upstream-${ab} total_sent" "${min_half}..${max_half} (≈half of ${LG_LIMIT})" "${total_sent} — too low" 1 || rc=1
    else
        report_check "upstream-${ab} total_sent" "${min_half}..${max_half} (≈half of ${LG_LIMIT})" "${total_sent} — too high, filter likely double-sending" 1 || rc=1
    fi
done

# ── 2. Downstream topic record count (authoritative total) ──
if ! docker inspect "$DOWN_BROKER" >/dev/null 2>&1; then
    report_check "Container ${DOWN_BROKER}" "running" "NOT running" 1 || true
    exit 2
fi
offsets_raw="$(docker exec "$DOWN_BROKER" kafka-run-class \
                    kafka.tools.GetOffsetShell \
                    --broker-list kafka-downstream:9092 \
                    --topic "$DOWN_TOPIC" 2>&1 || true)"
downstream_total="$(printf '%s\n' "$offsets_raw" | awk -F: '
    /:[0-9]+:[0-9]+$/ { s += $3 } END { print s+0 }')"

expected_ideal="$LG_LIMIT"
expected_min=$(( LG_LIMIT * 4 / 5 ))        # 80 for LG_LIMIT=100
expected_max=$(( LG_LIMIT * 6 / 5 ))        # 120 for LG_LIMIT=100
if (( downstream_total >= expected_min && downstream_total <= expected_max )); then
    report_check "kafka-downstream[${DOWN_TOPIC}] record count" "${expected_min}..${expected_max} (≈${expected_ideal})" "${downstream_total}" 0 || rc=1
elif (( downstream_total < expected_min )); then
    report_check "kafka-downstream[${DOWN_TOPIC}] record count" "${expected_min}..${expected_max} (≈${expected_ideal})" "${downstream_total} — one chain probably didn't deliver" 1 || rc=1
else
    report_check "kafka-downstream[${DOWN_TOPIC}] record count" "${expected_min}..${expected_max} (≈${expected_ideal})" "${downstream_total} — chains may be double-forwarding" 1 || rc=1
fi

report_summary "verify-tc12" "$rc"
exit "$rc"
