#!/usr/bin/env bash
# TC-23 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT
# after the LG producer has finished and the pipeline has drained, but
# BEFORE the sink is sent SIGTERM for its final summary.
#
# Same create -> resend -> propagation-wait flow as TC-14's tc14-resend.sh,
# except resend-23-docker.properties ALSO has inputFilterRules=deny:10
# enabled — the point of TC-23 is proving resend applies that filter too
# (REQ-39), not just that it fills gaps (REQ-3b/3d/40, already proven by
# TC-14).
#
# Env inherited from run-testcase.sh's manifest: CREATE_CONFIG,
# RESEND_CONFIG, TC23_BUNDLE_FILE, TC23_PROPAGATION_WAIT, TC23_RESEND_EPS.
# Explicitly passed: PROJECT_NAME, TC_ID.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

BUNDLE_BASENAME="${TC23_BUNDLE_FILE:-resend-23-all.json}"
BUNDLE_HOST_PATH="../../tmp/${BUNDLE_BASENAME}"
BUNDLE_CONTAINER_PATH="/airgap/tmp/${BUNDLE_BASENAME}"
PROPAGATION_WAIT="${TC23_PROPAGATION_WAIT:-20}"
RESEND_EPS="${TC23_RESEND_EPS:-50}"

echo "tc23-resend: ── step 1: export all gaps via create → ${BUNDLE_HOST_PATH} ──"
rm -f "$BUNDLE_HOST_PATH"
# See tc14-resend.sh for why the service name must be repeated as the
# override COMMAND (docker compose run SERVICE [COMMAND] [ARGS...]).
if docker compose --profile create run --rm create create \
        --resendFileName="$BUNDLE_CONTAINER_PATH" --limit=all; then
    report_check "create (export all gaps)" "exit 0" "exit 0" 0
else
    report_check "create (export all gaps)" "exit 0" "non-zero exit" 1 || true
    exit 1
fi

if [[ -s "$BUNDLE_HOST_PATH" ]]; then
    report_check "Bundle file ${BUNDLE_HOST_PATH}" "exists and non-empty" "exists and non-empty" 0
else
    report_check "Bundle file ${BUNDLE_HOST_PATH}" "exists and non-empty" "missing or empty" 1 || true
    exit 1
fi
gap_entries="$(grep -c '"partition"' "$BUNDLE_HOST_PATH" || true)"
if (( gap_entries > 0 )); then
    report_check "Bundle partition entries" "> 0" "${gap_entries}" 0
else
    report_check "Bundle partition entries" "> 0" "0 — nothing for resend to fill (was deliverFilter actually dropping events?)" 1 || true
    exit 1
fi

echo "tc23-resend: ── step 2: replay bundle via resend (eps=${RESEND_EPS}, inputFilterRules=deny:10) ──"
if docker compose --profile resend run --rm resend resend \
        --resendFileName="$BUNDLE_CONTAINER_PATH" --eps="$RESEND_EPS"; then
    report_check "resend (replay gaps, filter-aware)" "exit 0" "exit 0" 0
else
    report_check "resend (replay gaps, filter-aware)" "exit 0" "non-zero exit" 1 || true
    exit 1
fi

echo "tc23-resend: ── step 3: waiting ${PROPAGATION_WAIT}s for resent events to reach dedup/the clean topic ──"
echo "tc23-resend:   path: resend -> UDP -> downstream -> kafka-downstream[transfer] -> dedup (GAP_FILL) -> kafka-downstream[dedup]"
sleep "$PROPAGATION_WAIT"

echo "tc23-resend: post-drain hook complete"
