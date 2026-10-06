#!/usr/bin/env bash
# TC-25 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT
# after tc25-settle-timer exits. Runs both the negative and positive
# control from the manual procedure, plus a bonus blast/stress check:
#
#   1. Negative control (downstream already running with enableRxqOvfl=
#      false, the default): STATISTICS lines must NOT mention SO_RXQ_OVFL
#      at all.
#   2. Positive control: recreate downstream with enableRxqOvfl=true;
#      STATISTICS lines must now include BOTH "SO_RXQ_OVFL" (delta) and
#      "SO_RXQ_OVFL_TOTAL" (cumulative) keys.
#   3. Bonus (informational, not a hard pass/fail): recreate upstream with
#      eps=-1 ("as fast as possible") to try to actually trigger non-zero
#      kernel drops. Whether this succeeds is host/container-resource-
#      dependent, so it's reported but doesn't affect the testcase result.
#
# Env inherited from run-testcase.sh's manifest. Explicitly passed:
# PROJECT_NAME, TC_ID.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DOWNSTREAM_CTR="${PROJECT}-downstream-1"

rc=0

# ───────────────────────── Negative control ─────────────────────────
echo "tc25-rxqovfl-test: ── negative control (SO_RXQ_OVFL disabled, the default) ──"
downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
stats_count="$(printf '%s\n' "$downstream_log" | grep -cF 'STATISTICS:' || true)"
stats_count="${stats_count:-0}"
if (( stats_count > 0 )); then
    report_check "Negative-control STATISTICS lines" "> 0" "${stats_count}" 0 || rc=1
else
    report_check "Negative-control STATISTICS lines" "> 0" "0 — logStatistics never fired" 1 || rc=1
fi
leaked_count="$(printf '%s\n' "$downstream_log" | grep -F 'STATISTICS:' | grep -cF 'SO_RXQ_OVFL' || true)"
leaked_count="${leaked_count:-0}"
if (( leaked_count == 0 )); then
    report_check "Negative-control STATISTICS mentioning SO_RXQ_OVFL" "0" "0" 0 || rc=1
else
    report_check "Negative-control STATISTICS mentioning SO_RXQ_OVFL" "0" "${leaked_count} — leaked despite being disabled" 1 || rc=1
fi

# ───────────────────────── Positive control ─────────────────────────
echo "tc25-rxqovfl-test: ── positive control (recreating downstream with SO_RXQ_OVFL enabled) ──"
env DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-25b-docker.properties \
    docker compose up -d --force-recreate --no-deps downstream >/dev/null 2>&1 || true
echo "tc25-rxqovfl-test: waiting for upstream-a to reconnect and for a few STATISTICS cycles..."
sleep 14

downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
stats_count="$(printf '%s\n' "$downstream_log" | grep -cF 'STATISTICS:' || true)"
stats_count="${stats_count:-0}"
if (( stats_count > 0 )); then
    report_check "Positive-control STATISTICS lines" "> 0" "${stats_count}" 0 || rc=1
else
    report_check "Positive-control STATISTICS lines" "> 0" "0 — logStatistics never fired" 1 || rc=1
fi
present_count="$(printf '%s\n' "$downstream_log" | grep -F 'STATISTICS:' | grep -cF '"SO_RXQ_OVFL":' || true)"
present_count="${present_count:-0}"
present_total_count="$(printf '%s\n' "$downstream_log" | grep -F 'STATISTICS:' | grep -cF '"SO_RXQ_OVFL_TOTAL":' || true)"
present_total_count="${present_total_count:-0}"
if (( present_count > 0 )); then
    report_check "Positive-control STATISTICS with SO_RXQ_OVFL key" "> 0" "${present_count}" 0 || rc=1
else
    report_check "Positive-control STATISTICS with SO_RXQ_OVFL key" "> 0" "0 — key never appeared despite being enabled" 1 || rc=1
fi
if (( present_total_count > 0 )); then
    report_check "Positive-control STATISTICS with SO_RXQ_OVFL_TOTAL key" "> 0" "${present_total_count}" 0 || rc=1
else
    report_check "Positive-control STATISTICS with SO_RXQ_OVFL_TOTAL key" "> 0" "0 — key never appeared despite being enabled" 1 || rc=1
fi

# ───────────────────────── Bonus: blast/stress (informational) ─────────────────────────
echo "tc25-rxqovfl-test: ── bonus: blasting with eps=-1 to try to trigger real kernel drops (informational only) ──"
# target=null (not cmd) for this phase: cmd prints every received message,
# which at eps=-1 is 100K+ lines/sec for zero benefit to this check — see
# downstream-airgap-25c-docker.properties. SO_RXQ_OVFL tracking happens at
# the UDP socket layer, unaffected by which output target is active.
env DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-25c-docker.properties \
    docker compose up -d --force-recreate --no-deps downstream >/dev/null 2>&1 || true
env UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-25-blast-docker.properties \
    docker compose up -d --force-recreate --no-deps upstream-a >/dev/null 2>&1 || true
sleep 14
downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
max_rxq_ovfl="$(printf '%s\n' "$downstream_log" | grep -F 'STATISTICS:' \
    | sed -n 's/.*"SO_RXQ_OVFL":\([0-9]*\).*/\1/p' \
    | sort -n | tail -1 || true)"
max_rxq_ovfl="${max_rxq_ovfl:-0}"
if (( max_rxq_ovfl > 0 )); then
    echo "tc25-rxqovfl-test: [info] blast triggered real kernel drops — max SO_RXQ_OVFL delta observed: ${max_rxq_ovfl}"
else
    echo "tc25-rxqovfl-test: [info] blast did not trigger any observable kernel drops in this environment (host/container-dependent, not a failure)"
fi

report_summary "tc25-rxqovfl-test" "$rc"
exit "$rc"
